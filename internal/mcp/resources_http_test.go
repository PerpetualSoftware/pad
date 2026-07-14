package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpsrv "github.com/mark3labs/mcp-go/server"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
)

// newHTTPResourceFetcher wires an HTTPResourceFetcher over a test mux with a
// fixed authenticated user, mirroring the dispatch_http_*_test.go setup. The
// mux stands in for pad-cloud's handler chain.
func newHTTPResourceFetcher(mux http.Handler) *HTTPResourceFetcher {
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	return NewHTTPResourceFetcher(d)
}

func TestSplitResourceArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		args    []string
		wantCmd string
		flags   map[string]string
		wantRef string // nthPositional(pos, 2)
	}{
		{
			name:    "item show",
			args:    []string{"item", "show", "TASK-5", "--workspace", "docapp", "--format", "json"},
			wantCmd: "item show",
			flags:   map[string]string{"workspace": "docapp", "format": "json"},
			wantRef: "TASK-5",
		},
		{
			name:    "item list --all bareword flag",
			args:    []string{"item", "list", "--all", "--workspace", "docapp", "--format", "json"},
			wantCmd: "item list",
			flags:   map[string]string{"all": "", "workspace": "docapp", "format": "json"},
		},
		{
			name:    "attachment download with stdout sink positional",
			args:    []string{"attachment", "download", "att-1", "-", "--workspace", "docapp", "--variant", "thumb-md"},
			wantCmd: "attachment download",
			flags:   map[string]string{"workspace": "docapp", "variant": "thumb-md"},
			wantRef: "att-1",
		},
		{
			name:    "bootstrap single token",
			args:    []string{"bootstrap", "--workspace", "docapp", "--format", "json"},
			wantCmd: "bootstrap",
			flags:   map[string]string{"workspace": "docapp", "format": "json"},
		},
		{
			name:    "workspace list no workspace flag",
			args:    []string{"workspace", "list", "--format", "json"},
			wantCmd: "workspace list",
			flags:   map[string]string{"format": "json"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			flags, pos := splitResourceArgs(tc.args)
			if got := resourceCmdKey(pos); got != tc.wantCmd {
				t.Errorf("resourceCmdKey = %q, want %q", got, tc.wantCmd)
			}
			for k, v := range tc.flags {
				if got, ok := flags[k]; !ok || got != v {
					t.Errorf("flags[%q] = %q (present=%v), want %q", k, got, ok, v)
				}
			}
			if tc.wantRef != "" {
				if got := nthPositional(pos, 2); got != tc.wantRef {
					t.Errorf("positional ref = %q, want %q", got, tc.wantRef)
				}
			}
		})
	}
}

func TestHTTPResourceFetcher_ItemShow_ReturnsRawBody(t *testing.T) {
	t.Parallel()
	const body = `{"ref":"TASK-5","title":"Fix it","fields":"{\"status\":\"open\"}","content":"# Body\n"}`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/items/TASK-5", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		_, _ = w.Write([]byte(body))
	})
	f := newHTTPResourceFetcher(mux)

	got, err := f.Fetch(context.Background(),
		[]string{"item", "show", "TASK-5", "--workspace", "docapp", "--format", "json"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != body {
		t.Errorf("Fetch = %q, want %q", got, body)
	}
}

func TestHTTPResourceFetcher_ItemList_ProjectsToSummaries(t *testing.T) {
	t.Parallel()
	// Raw endpoint returns []models.Item; the fetcher must project to the
	// token-light ItemSummary shape (content -> content_preview, fields as a
	// nested object) exactly like `pad item list --format json`.
	items := []models.Item{{Ref: "TASK-9", Title: "Do the thing", Content: "Full body text", Fields: `{"status":"open"}`}}
	raw, _ := json.Marshal(items)

	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/items", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write(raw)
	})
	f := newHTTPResourceFetcher(mux)

	got, err := f.Fetch(context.Background(),
		[]string{"item", "list", "--all", "--workspace", "docapp", "--format", "json"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// --all must NOT surface soft-deleted items: the CLI's --all lifts only
	// the terminal-status filter, never include_archived. Setting it would
	// diverge from stdio and leak deleted items into the resource.
	if strings.Contains(gotQuery, "include_archived") {
		t.Errorf("query = %q, must NOT set include_archived (would leak soft-deleted items vs stdio)", gotQuery)
	}
	// --all lifts the non_terminal filter, so it must not be present either.
	if strings.Contains(gotQuery, "non_terminal") {
		t.Errorf("query = %q, --all must not set non_terminal", gotQuery)
	}
	if !strings.Contains(gotQuery, fmt.Sprintf("limit=%d", itemListResourceLimit)) {
		t.Errorf("query = %q, want limit=%d", gotQuery, itemListResourceLimit)
	}

	var summaries []map[string]any
	if err := json.Unmarshal([]byte(got), &summaries); err != nil {
		t.Fatalf("decode summaries: %v (body=%s)", err, got)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries len = %d, want 1", len(summaries))
	}
	s := summaries[0]
	if s["ref"] != "TASK-9" {
		t.Errorf("ref = %v, want TASK-9", s["ref"])
	}
	if _, hasContent := s["content"]; hasContent {
		t.Errorf("summary should not carry full content; got %v", s["content"])
	}
	if s["content_preview"] != "Full body text" {
		t.Errorf("content_preview = %v, want %q", s["content_preview"], "Full body text")
	}
	// fields is a nested object in the summary shape, not a JSON string.
	if _, ok := s["fields"].(map[string]any); !ok {
		t.Errorf("fields = %T, want nested object", s["fields"])
	}
}

func TestHTTPResourceFetcher_JSONPassThrough(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		path string
		body string
	}{
		{"dashboard", []string{"project", "dashboard", "--workspace", "docapp", "--format", "json"},
			"/api/v1/workspaces/docapp/dashboard", `{"summary":{"total":3}}`},
		{"collections", []string{"collection", "list", "--workspace", "docapp", "--format", "json"},
			"/api/v1/workspaces/docapp/collections", `[{"slug":"tasks"}]`},
		{"bootstrap", []string{"bootstrap", "--workspace", "docapp", "--format", "json"},
			"/api/v1/workspaces/docapp/agent/bootstrap", `{"workspace":{"slug":"docapp"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			mux.HandleFunc(tc.path, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			f := newHTTPResourceFetcher(mux)
			got, err := f.Fetch(context.Background(), tc.args)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if got != tc.body {
				t.Errorf("Fetch = %q, want verbatim %q", got, tc.body)
			}
		})
	}
}

func TestHTTPResourceFetcher_WorkspaceList_Projects(t *testing.T) {
	t.Parallel()
	// Full workspace records in; the fetcher projects to {slug,name,updated_at}
	// and drops server-only fields. The CLI's `default` flag is CWD-derived
	// (local only) and must not appear on the remote transport.
	wss := []models.Workspace{{Slug: "docapp", Name: "Pad"}}
	raw, _ := json.Marshal(wss)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(raw)
	})
	f := newHTTPResourceFetcher(mux)

	got, err := f.Fetch(context.Background(), []string{"workspace", "list", "--format", "json"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(got), &entries); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, got)
	}
	if len(entries) != 1 || entries[0]["slug"] != "docapp" || entries[0]["name"] != "Pad" {
		t.Fatalf("entries = %v, want [{slug:docapp,name:Pad}]", entries)
	}
	if _, hasDefault := entries[0]["default"]; hasDefault {
		t.Errorf("remote workspace list must not carry the CWD-only `default` flag")
	}
	if _, hasID := entries[0]["id"]; hasID {
		t.Errorf("workspace list should project away server-only fields; got id=%v", entries[0]["id"])
	}
}

func TestHTTPResourceFetcher_WorkspaceList_FiltersByConsentAllowList(t *testing.T) {
	t.Parallel()
	// The /api/v1/workspaces endpoint returns every membership with no
	// allow-list scoping, so the resource must filter by the OAuth token's
	// consented workspaces or it leaks slugs of unconsented ones.
	wss := []models.Workspace{{Slug: "alpha", Name: "Alpha"}, {Slug: "beta", Name: "Beta"}}
	raw, _ := json.Marshal(wss)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(raw)
	})
	f := newHTTPResourceFetcher(mux)

	// Token consented only for alpha: beta must be dropped.
	ctx := server.WithTokenAllowedWorkspaces(context.Background(), []string{"alpha"})
	got, err := f.Fetch(ctx, []string{"workspace", "list", "--format", "json"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(got), &entries); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, got)
	}
	if len(entries) != 1 || entries[0]["slug"] != "alpha" {
		t.Fatalf("entries = %v, want only [alpha] (beta is unconsented)", entries)
	}

	// No allow-list (PAT / stdio): both workspaces returned (no filter).
	got2, err := f.Fetch(context.Background(), []string{"workspace", "list", "--format", "json"})
	if err != nil {
		t.Fatalf("Fetch (no allow-list): %v", err)
	}
	var all []map[string]any
	if err := json.Unmarshal([]byte(got2), &all); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("without allow-list want both workspaces, got %d", len(all))
	}
}

func TestHTTPResourceFetcher_AttachmentShow_SynthesizesFromHeaders(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/attachments/att-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %s, want HEAD", r.Method)
		}
		if got := r.URL.Query().Get("variant"); got != "thumb-md" {
			t.Errorf("variant = %q, want thumb-md", got)
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "512")
		w.Header().Set("Content-Disposition", `inline; filename="pic.png"`)
		w.Header().Set("ETag", `"abc"`)
		w.WriteHeader(http.StatusOK)
	})
	f := newHTTPResourceFetcher(mux)

	got, err := f.Fetch(context.Background(),
		[]string{"attachment", "show", "att-1", "--workspace", "docapp", "--variant", "thumb-md", "--format", "json"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(got), &meta); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, got)
	}
	if meta["mime"] != "image/png" {
		t.Errorf("mime = %v, want image/png", meta["mime"])
	}
	if meta["size"] != float64(512) {
		t.Errorf("size = %v, want 512", meta["size"])
	}
	if meta["filename"] != "pic.png" {
		t.Errorf("filename = %v, want pic.png", meta["filename"])
	}
	if meta["etag"] != `"abc"` {
		t.Errorf("etag = %v, want %q", meta["etag"], `"abc"`)
	}
}

func TestHTTPResourceFetcher_AttachmentDownload_ReturnsBytes(t *testing.T) {
	t.Parallel()
	payload := []byte("\x89PNG\r\n\x1a\n small image bytes")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/attachments/att-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(payload)
	})
	f := newHTTPResourceFetcher(mux)

	got, err := f.FetchBytes(context.Background(),
		[]string{"attachment", "download", "att-1", "-", "--workspace", "docapp", "--variant", "thumb-md"})
	if err != nil {
		t.Fatalf("FetchBytes: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("FetchBytes = %q, want %q", got, payload)
	}
}

func TestHTTPResourceFetcher_AttachmentDownload_CapsOversized(t *testing.T) {
	t.Parallel()
	big := make([]byte, attachmentResourceMaxBytes+100)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/attachments/att-1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(big)
	})
	f := newHTTPResourceFetcher(mux)

	_, err := f.FetchBytes(context.Background(),
		[]string{"attachment", "download", "att-1", "-", "--workspace", "docapp", "--variant", "thumb-md"})
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("FetchBytes err = %v, want cap-exceeded error", err)
	}
}

// TestCappedResponseWriter_WithServeContent exercises the writer against
// http.ServeContent — the exact code path handleGetAttachment takes on the
// seekable (FSStore) attachment blob — to confirm the cap holds through
// ServeContent's header + Write choreography, not just a bare w.Write.
func TestCappedResponseWriter_WithServeContent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		size         int
		wantExceeded bool
	}{
		{"under cap", 128, false},
		{"exact cap", attachmentResourceMaxBytes, false},
		{"over cap", attachmentResourceMaxBytes + 100, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			content := make([]byte, tc.size)
			w := newCappedResponseWriter(attachmentResourceMaxBytes + 1)
			req := httptest.NewRequest(http.MethodGet, "/blob", nil)
			http.ServeContent(w, req, "img.png", time.Time{}, bytes.NewReader(content))
			if w.status != http.StatusOK {
				t.Errorf("status = %d, want 200", w.status)
			}
			if w.exceededCap() != tc.wantExceeded {
				t.Errorf("exceeded = %v, want %v (served %d bytes)", w.exceededCap(), tc.wantExceeded, tc.size)
			}
			if !tc.wantExceeded && w.body.Len() != tc.size {
				t.Errorf("body len = %d, want %d", w.body.Len(), tc.size)
			}
		})
	}
}

func TestHTTPResourceFetcher_NoUser_Errors(t *testing.T) {
	t.Parallel()
	d := &HTTPHandlerDispatcher{
		Handler:      http.NewServeMux(),
		UserResolver: func(context.Context) *models.User { return nil },
	}
	f := NewHTTPResourceFetcher(d)
	if _, err := f.Fetch(context.Background(),
		[]string{"bootstrap", "--workspace", "docapp", "--format", "json"}); err == nil {
		t.Fatal("Fetch expected error when no user in context")
	}
	if _, err := f.FetchBytes(context.Background(),
		[]string{"attachment", "download", "att-1", "-", "--workspace", "docapp", "--variant", "thumb-md"}); err == nil {
		t.Fatal("FetchBytes expected error when no user in context")
	}
}

// TestHTTPResourceFetcher_ReadScopeAllowsResourceReads locks in the
// security-parity claim: resource reads flow through the dispatcher's
// buildAuthedRequest perimeter, so a read-scoped OAuth token is sufficient
// (and, since every resource read is a GET/HEAD, the write gates never fire).
func TestHTTPResourceFetcher_ReadScopeAllowsResourceReads(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/agent/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"workspace":{"slug":"docapp"}}`))
	})
	f := newHTTPResourceFetcher(mux)
	ctx := server.WithTokenScopes(context.Background(), `["read"]`)
	got, err := f.Fetch(ctx, []string{"bootstrap", "--workspace", "docapp", "--format", "json"})
	if err != nil {
		t.Fatalf("read-scoped resource fetch failed: %v", err)
	}
	if !strings.Contains(got, "docapp") {
		t.Errorf("unexpected body: %s", got)
	}
}

func TestHTTPResourceFetcher_HTTPError_Propagates(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ghost/items/TASK-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found"}`))
	})
	f := newHTTPResourceFetcher(mux)
	_, err := f.Fetch(context.Background(),
		[]string{"item", "show", "TASK-1", "--workspace", "ghost", "--format", "json"})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("Fetch err = %v, want HTTP 404 error", err)
	}
}

// TestHTTPResourceFetcher_AttachmentResourceRoundTrip proves the full
// readAttachment path works over the HTTP fetcher: RegisterResources wires
// the same handler used by stdio, and a ReadResource call drives the HEAD
// metadata preflight + capped GET + MIME sniff + base64 encoding end to end.
func TestHTTPResourceFetcher_AttachmentResourceRoundTrip(t *testing.T) {
	t.Parallel()
	// A minimal valid PNG header so http.DetectContentType sniffs image/png.
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/attachments/att-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprint(len(png)))
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(png)
	})

	srv := mcpsrv.NewMCPServer("test", "0", mcpsrv.WithResourceCapabilities(false, false))
	RegisterResources(srv, newHTTPResourceFetcher(mux), nil)

	reqJSON := []byte(`{
		"jsonrpc": "2.0", "id": 1, "method": "resources/read",
		"params": {"uri": "pad://workspace/docapp/attachments/att-1"}
	}`)
	resp := srv.HandleMessage(context.Background(), reqJSON)
	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	// Sniffed image/png MIME + base64 of the served PNG bytes.
	wantBlob := base64.StdEncoding.EncodeToString(png)
	for _, want := range []string{`"mimeType":"image/png"`, `"blob":"` + wantBlob + `"`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("resource response missing %s: %s", want, encoded)
		}
	}
}
