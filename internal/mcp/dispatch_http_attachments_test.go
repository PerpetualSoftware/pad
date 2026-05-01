package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// --- attachment list ---

func TestDispatch_AttachmentList_BuildsExpectedQueryString(t *testing.T) {
	gotPath := ""
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/attachments", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"attachments":[],"total":0,"limit":50,"offset":0}`))
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace":  "docapp",
			"category":   "image",
			"collection": "tasks-uuid",
			"sort":       "created_at_desc",
			"limit":      float64(20),
			"offset":     float64(40),
		}),
		[]string{"attachment", "list"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	idx := strings.Index(gotPath, "?")
	if idx < 0 {
		t.Fatalf("expected query string in path; got %q", gotPath)
	}
	values, err := url.ParseQuery(gotPath[idx+1:])
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	want := map[string]string{
		"category":   "image",
		"collection": "tasks-uuid",
		"sort":       "created_at_desc",
		"limit":      "20",
		"offset":     "40",
	}
	for k, v := range want {
		if got := values.Get(k); got != v {
			t.Errorf("query[%q] = %q, want %q", k, got, v)
		}
	}
}

func TestDispatch_AttachmentList_AttachedAndUnattachedFolds(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{
			name: "attached=true folds to item=attached",
			in: map[string]any{
				"workspace": "docapp",
				"attached":  true,
			},
			want: "attached",
		},
		{
			name: "unattached=true folds to item=unattached",
			in: map[string]any{
				"workspace":  "docapp",
				"unattached": true,
			},
			want: "unattached",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPath := ""
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/workspaces/docapp/attachments", func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.RequestURI()
				_, _ = w.Write([]byte(`{"attachments":[],"total":0,"limit":50,"offset":0}`))
			})
			d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
			res, err := d.Dispatch(
				WithDispatchInput(context.Background(), tc.in),
				[]string{"attachment", "list"}, nil,
			)
			if err != nil || res.IsError {
				t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
			}
			values, _ := url.ParseQuery(strings.SplitN(gotPath, "?", 2)[1])
			if got := values.Get("item"); got != tc.want {
				t.Errorf("item filter = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDispatch_AttachmentList_RejectsAttachedAndUnattachedTogether(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not call handler when both flags set"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace":  "docapp",
			"attached":   true,
			"unattached": true,
		}),
		[]string{"attachment", "list"}, nil,
	)
	if err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError when both --attached and --unattached set")
	}
	if !containsToolText(res, "mutually exclusive") {
		t.Errorf("error should mention mutex; got %#v", res)
	}
}

func TestDispatch_AttachmentList_RejectsItemAndUnattached(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not call handler when both --item and --unattached set"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace":  "docapp",
			"item":       "TASK-5",
			"unattached": true,
		}),
		[]string{"attachment", "list"}, nil,
	)
	if err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError when --item and --unattached set together")
	}
}

func TestDispatch_AttachmentList_ResolvesItemRefToUUID(t *testing.T) {
	// CLI passes --item TASK-5; handler reads item_id (UUID). The
	// dispatcher must prefetch the item to convert ref → UUID
	// before forwarding, otherwise the filter silently matches
	// nothing.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/items/TASK-5", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"item-uuid-5","slug":"task-5"}`))
	})
	gotItemID := ""
	mux.HandleFunc("/api/v1/workspaces/docapp/attachments", func(w http.ResponseWriter, r *http.Request) {
		gotItemID = r.URL.Query().Get("item_id")
		_, _ = w.Write([]byte(`{"attachments":[],"total":0,"limit":50,"offset":0}`))
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"item":      "TASK-5",
		}),
		[]string{"attachment", "list"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	if gotItemID != "item-uuid-5" {
		t.Errorf("item_id = %q, want item-uuid-5 (resolved from TASK-5)", gotItemID)
	}
}

func TestDispatch_AttachmentList_ItemRefResolutionFailureSurfacesError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/items/TASK-99", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})
	listCount := 0
	mux.HandleFunc("/api/v1/workspaces/docapp/attachments", func(_ http.ResponseWriter, _ *http.Request) {
		listCount++
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"item":      "TASK-99",
		}),
		[]string{"attachment", "list"}, nil,
	)
	if err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError when item ref doesn't resolve")
	}
	if listCount != 0 {
		t.Errorf("attachments endpoint must not be called after ref-resolution failure; got %d", listCount)
	}
}

func TestDispatch_AttachmentList_RequiresWorkspace(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not call handler when workspace missing"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{}),
		[]string{"attachment", "list"}, nil,
	)
	if err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError when workspace missing")
	}
}

// --- attachment show ---

func TestDispatch_AttachmentShow_ExtractsHeadersAsJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/attachments/abc-123", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("expected HEAD, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "12345")
		w.Header().Set("Content-Disposition", `attachment; filename="screenshot.png"`)
		w.Header().Set("ETag", `"sha256-abc"`)
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2026 07:28:00 GMT")
		w.WriteHeader(http.StatusOK)
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace":     "docapp",
			"attachment_id": "abc-123",
		}),
		[]string{"attachment", "show"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	payload, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("not structured: %#v", res.StructuredContent)
	}
	if payload["id"] != "abc-123" {
		t.Errorf("id = %v", payload["id"])
	}
	if payload["mime"] != "image/png" {
		t.Errorf("mime = %v", payload["mime"])
	}
	if payload["size"].(float64) != 12345 {
		t.Errorf("size = %v, want 12345", payload["size"])
	}
	if payload["filename"] != "screenshot.png" {
		t.Errorf("filename = %v", payload["filename"])
	}
	if payload["etag"] != `"sha256-abc"` {
		t.Errorf("etag = %v", payload["etag"])
	}
	if payload["last_modified"] != "Wed, 21 Oct 2026 07:28:00 GMT" {
		t.Errorf("last_modified = %v", payload["last_modified"])
	}
}

func TestDispatch_AttachmentShow_ForwardsVariantQuery(t *testing.T) {
	gotURL := ""
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/attachments/abc-123", func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.RequestURI()
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	_, _ = d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace":     "docapp",
			"attachment_id": "abc-123",
			"variant":       "thumb-md",
		}),
		[]string{"attachment", "show"}, nil,
	)
	if !strings.Contains(gotURL, "variant=thumb-md") {
		t.Errorf("variant should be forwarded as query param; got %q", gotURL)
	}
}

func TestDispatch_AttachmentShow_404SurfacesAsToolError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/attachments/missing", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace":     "docapp",
			"attachment_id": "missing",
		}),
		[]string{"attachment", "show"}, nil,
	)
	if err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError on 404; got %#v", res)
	}
}

func TestDispatch_AttachmentShow_RequiresArgs(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not call handler when args missing"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	for _, missing := range []string{"workspace", "attachment_id"} {
		t.Run("missing-"+missing, func(t *testing.T) {
			input := map[string]any{
				"workspace": "docapp", "attachment_id": "abc-123",
			}
			delete(input, missing)
			res, err := d.Dispatch(
				WithDispatchInput(context.Background(), input),
				[]string{"attachment", "show"}, nil,
			)
			if err != nil {
				t.Fatalf("Dispatch err: %v", err)
			}
			if !res.IsError {
				t.Errorf("expected IsError when %s missing", missing)
			}
		})
	}
}

// --- parseAttachmentFilename ---

func TestParseAttachmentFilename_StandardForm(t *testing.T) {
	got := parseAttachmentFilename(`attachment; filename="screenshot.png"`)
	if got != "screenshot.png" {
		t.Errorf("got %q, want screenshot.png", got)
	}
}

func TestParseAttachmentFilename_PrefersFilenameStarOverFilename(t *testing.T) {
	// RFC 5987 says filename* takes precedence — it's the
	// spec-compliant carrier for non-ASCII names.
	got := parseAttachmentFilename(`attachment; filename="ascii.txt"; filename*=UTF-8''r%C3%A9sum%C3%A9.pdf`)
	if got != "résumé.pdf" {
		t.Errorf("got %q, want résumé.pdf", got)
	}
}

func TestParseAttachmentFilename_HandlesQuotedAndUnquoted(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`attachment; filename="quoted.txt"`, "quoted.txt"},
		{`attachment; filename=unquoted.txt`, "unquoted.txt"},
		{``, ""},
		{`inline`, ""}, // no filename
	}
	for _, tc := range cases {
		got := parseAttachmentFilename(tc.in)
		if got != tc.want {
			t.Errorf("parse(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- noRemoteEquivalent: attachment upload/download/view ---

func TestDispatch_AttachmentUploadDownloadView_RejectedAsCLIOnly(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "attachment upload/download/view must not reach handler"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	for _, cmd := range []string{"attachment upload", "attachment download", "attachment view"} {
		t.Run(cmd, func(t *testing.T) {
			ctx := WithDispatchInput(context.Background(), map[string]any{
				"workspace": "docapp",
			})
			res, err := d.Dispatch(ctx, strings.Split(cmd, " "), nil)
			if err != nil {
				t.Fatalf("Dispatch err: %v", err)
			}
			if !res.IsError {
				t.Errorf("expected IsError; got %#v", res)
			}
			if !containsToolText(res, "no remote equivalent") {
				t.Errorf("error should call out CLI-only nature; got %#v", res)
			}
			// The rationale clause should mention the alternative
			// path so agents can pivot to fetching bytes via the URL.
			if !containsToolText(res, "attachment URL") {
				t.Errorf("rationale should point at attachment URL; got %#v", res)
			}
		})
	}
}

// --- noRemoteEquivalent rationale clause is per-entry ---

func TestNoRemoteEquivalent_RationaleIsPerEntry(t *testing.T) {
	// PR #350 refactor: noRemoteEquivalent went from
	// map[string]struct{} to map[string]string with a per-entry
	// rationale clause. Verify that github vs attachment commands
	// surface DIFFERENT rationale content (not the old generic
	// "operates on local pad client / config state" string).
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not reach handler"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	githubRes, _ := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{}),
		[]string{"github", "link"}, nil,
	)
	attachRes, _ := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{}),
		[]string{"attachment", "upload"}, nil,
	)
	if !containsToolText(githubRes, "git branch") {
		t.Errorf("github link rationale should mention git branch; got %#v", githubRes)
	}
	if !containsToolText(attachRes, "filesystem") {
		t.Errorf("attachment upload rationale should mention filesystem; got %#v", attachRes)
	}
}

// --- Integration smoke ---

func TestHTTPHandlerDispatcher_Integration_Attachments(t *testing.T) {
	srv, st := newPadServer(t)

	wsRec := doJSONReq(t, srv, http.MethodPost, "/api/v1/workspaces",
		map[string]any{"name": "DocApp"})
	if wsRec.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", wsRec.Code, wsRec.Body.String())
	}
	var ws models.Workspace
	if err := json.Unmarshal(wsRec.Body.Bytes(), &ws); err != nil {
		t.Fatalf("decode workspace: %v", err)
	}
	owner, err := st.CreateUser(models.UserCreate{Email: "dave@example.com", Name: "Dave", Password: "x"})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := st.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner: %v", err)
	}

	d := &HTTPHandlerDispatcher{Handler: srv, UserResolver: fixedUserResolver(owner)}

	// Empty workspace: list should return zero attachments without
	// 500ing.
	listRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": ws.Slug,
		}),
		[]string{"attachment", "list"}, nil,
	)
	if err != nil || listRes.IsError {
		t.Fatalf("attachment list: err=%v IsError=%v: %#v", err, listRes != nil && listRes.IsError, listRes)
	}
	payload, _ := listRes.StructuredContent.(map[string]any)
	if total, _ := payload["total"].(float64); total != 0 {
		t.Errorf("expected total=0 on empty workspace; got %v", total)
	}

	// Upload/download/view are CLI-only; verify they reject with a
	// stable message even when the workspace + auth are wired up.
	for _, cmd := range []string{"attachment upload", "attachment download", "attachment view"} {
		res, err := d.Dispatch(
			WithDispatchInput(context.Background(), map[string]any{"workspace": ws.Slug}),
			strings.Split(cmd, " "), nil,
		)
		if err != nil {
			t.Fatalf("%s: err=%v", cmd, err)
		}
		if !res.IsError || !containsToolText(res, "no remote equivalent") {
			t.Errorf("%s: expected noRemoteEquivalent rejection; got %#v", cmd, res)
		}
	}
}
