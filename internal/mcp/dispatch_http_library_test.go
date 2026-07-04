package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// --- library activate ---

func TestDispatch_LibraryActivate_ConventionByTitle(t *testing.T) {
	// Pick a known convention from the seed library — tied to the
	// convention_library.go constants.
	const wantTitle = "Conventional commit format"

	mux := http.NewServeMux()
	posted := ""
	mux.HandleFunc("/api/v1/workspaces/docapp/collections/conventions/items", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST; got %s", r.Method)
		}
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		posted = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"item-1","title":"Conventional commit format"}`))
	})
	// Playbook endpoint MUST NOT be hit (we found a convention).
	mux.HandleFunc("/api/v1/workspaces/docapp/collections/playbooks/items", func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("playbook endpoint should not be hit when convention matches")
	})

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"title":     wantTitle,
		}),
		[]string{"library", "activate"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(posted), &got); err != nil {
		t.Fatalf("decode posted body: %v\n%s", err, posted)
	}
	if got["title"] != wantTitle {
		t.Errorf("posted title = %v, want %v", got["title"], wantTitle)
	}
	// Convention fields must include the canonical metadata.
	fieldsStr, _ := got["fields"].(string)
	var fields map[string]any
	if err := json.Unmarshal([]byte(fieldsStr), &fields); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	if fields["status"] != "active" {
		t.Errorf("status = %v, want active", fields["status"])
	}
	if fields["category"] != "git" {
		t.Errorf("category = %v, want git (from seed library)", fields["category"])
	}
	if fields["trigger"] != "on-commit" {
		t.Errorf("trigger = %v, want on-commit", fields["trigger"])
	}
}

func TestDispatch_LibraryActivate_PlaybookByTitle_FallsThroughConventionLookup(t *testing.T) {
	// A playbook title — the dispatcher should NOT find it in
	// conventions, then fall through to the playbook library.
	//
	// PLAN-1397 (TASK-1403) retired the pre-PLAN-1377 trigger-only
	// entries; "Ship tasks" is the headline invokable that still
	// resolves through the library and still carries trigger + scope
	// in its activation payload (plus invocation_slug + arguments,
	// which this test doesn't assert on — those are covered separately).
	const wantTitle = "Ship tasks"

	mux := http.NewServeMux()
	posted := ""
	mux.HandleFunc("/api/v1/workspaces/docapp/collections/playbooks/items", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		posted = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"pb-1","title":"Ship tasks"}`))
	})

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"title":     wantTitle,
		}),
		[]string{"library", "activate"}, nil,
	)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(posted), &got); err != nil {
		t.Fatalf("decode posted body: %v\n%s", err, posted)
	}
	if got["title"] != wantTitle {
		t.Errorf("posted title = %v", got["title"])
	}
	fieldsStr, _ := got["fields"].(string)
	var fields map[string]any
	if err := json.Unmarshal([]byte(fieldsStr), &fields); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	if fields["status"] != "active" {
		t.Errorf("status = %v, want active", fields["status"])
	}
	if _, ok := fields["trigger"]; !ok {
		t.Errorf("playbook fields should include trigger: %v", fields)
	}
	if _, ok := fields["scope"]; !ok {
		t.Errorf("playbook fields should include scope: %v", fields)
	}
}

func TestDispatch_LibraryActivate_NotFoundReturnsError(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not POST when title is unmatched"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	res, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": "docapp",
			"title":     "NoSuchLibraryEntryEverShouldExist",
		}),
		[]string{"library", "activate"}, nil,
	)
	if err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError when title not in either library")
	}
	if !containsToolText(res, "not found in convention or playbook library") {
		t.Errorf("error should mention library lookup; got %#v", res)
	}
}

func TestDispatch_LibraryActivate_RequiresWorkspaceAndTitle(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not POST when args missing"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	for _, missing := range []string{"workspace", "title"} {
		t.Run("missing-"+missing, func(t *testing.T) {
			input := map[string]any{
				"workspace": "docapp", "title": "Anything",
			}
			delete(input, missing)
			res, err := d.Dispatch(
				WithDispatchInput(context.Background(), input),
				[]string{"library", "activate"}, nil,
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

// --- Integration smoke ---

// TestHTTPHandlerDispatcher_Integration_ProjectIntelAndLibrary exercises
// project standup/changelog (proxied to the REST project-intel
// endpoints as of TASK-1916) and library activate against a real
// in-process server + store, guarding against 500s on a workspace with
// no data yet.
func TestHTTPHandlerDispatcher_Integration_ProjectIntelAndLibrary(t *testing.T) {
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

	// project standup against a fresh workspace — should not 500
	// even though there's no completed work.
	standupRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{"workspace": ws.Slug}),
		[]string{"project", "standup"}, nil,
	)
	if err != nil || standupRes.IsError {
		t.Fatalf("standup: err=%v IsError=%v: %#v", err, standupRes != nil && standupRes.IsError, standupRes)
	}

	// project changelog same — empty workspace, no error.
	clRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{"workspace": ws.Slug}),
		[]string{"project", "changelog"}, nil,
	)
	if err != nil || clRes.IsError {
		t.Fatalf("changelog: err=%v IsError=%v: %#v", err, clRes != nil && clRes.IsError, clRes)
	}

	// library activate with a known seed convention. The default
	// "startup" template seeds Conventions; this should succeed.
	actRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": ws.Slug,
			"title":     "Conventional commit format",
		}),
		[]string{"library", "activate"}, nil,
	)
	if err != nil || actRes.IsError {
		t.Fatalf("library activate: err=%v IsError=%v: %#v", err, actRes != nil && actRes.IsError, actRes)
	}
	created, _ := actRes.StructuredContent.(map[string]any)
	if title, _ := created["title"].(string); title != "Conventional commit format" {
		t.Errorf("activated item title = %v", title)
	}
}
