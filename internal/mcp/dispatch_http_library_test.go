package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// --- library activate ---

// Destination collection slugs used by the activate tests. They are
// deliberately NOT "conventions"/"playbooks": with the canonical names, an
// implementation that ignored traits entirely and always posted to the
// hardcoded fallback would pass every assertion, so the test would prove
// nothing about the behaviour it exists to cover (CONVE-12 — an end state two
// mechanisms both produce is not evidence). Codex round 6 caught exactly that
// in the first version of these tests.
const (
	renamedConventionsSlug = "house-rules"
	renamedPlaybooksSlug   = "procedures"
)

// serveCollectionsFor registers the collections listing the activate path
// consults to resolve its destination from declared artifact kinds
// (TASK-2657). The real server always serves this endpoint; a fake that omits
// it makes activation look like a lookup failure, which the dispatcher
// deliberately refuses rather than falling back on.
//
// The listing reports RENAMED collections carrying the canonical declarations,
// which is the whole point: activation must follow the declaration, not the
// name.
func serveCollectionsFor(mux *http.ServeMux, workspace string) {
	mux.HandleFunc("/api/v1/workspaces/"+workspace+"/collections", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":"c1","slug":"` + renamedConventionsSlug + `","traits":"{\"artifact_kind\":{\"kind\":\"convention\"}}"},
			{"id":"c2","slug":"` + renamedPlaybooksSlug + `","traits":"{\"artifact_kind\":{\"kind\":\"playbook\"}}"}
		]`))
	})
}

// trapCanonicalSlugs fails the test if activation posts to the hardcoded
// fallback slugs. Without this, a regression to slug-hardcoding would surface
// only as a missing POST rather than as a named failure.
func trapCanonicalSlugs(t *testing.T, mux *http.ServeMux, workspace string) {
	t.Helper()
	for _, slug := range []string{"conventions", "playbooks"} {
		mux.HandleFunc("/api/v1/workspaces/"+workspace+"/collections/"+slug+"/items", func(_ http.ResponseWriter, _ *http.Request) {
			t.Errorf("activation posted to the canonical %q collection; it must follow the artifact_kind declaration to the renamed collection", slug)
		})
	}
}

func TestDispatch_LibraryActivate_ConventionByTitle(t *testing.T) {
	// Pick a known convention from the seed library — tied to the
	// convention_library.go constants.
	const wantTitle = "Conventional commit format"

	mux := http.NewServeMux()
	posted := ""
	mux.HandleFunc("/api/v1/workspaces/docapp/collections/"+renamedConventionsSlug+"/items", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("/api/v1/workspaces/docapp/collections/"+renamedPlaybooksSlug+"/items", func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("playbook endpoint should not be hit when convention matches")
	})

	serveCollectionsFor(mux, "docapp")
	trapCanonicalSlugs(t, mux, "docapp")
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
	mux.HandleFunc("/api/v1/workspaces/docapp/collections/"+renamedPlaybooksSlug+"/items", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		posted = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"pb-1","title":"Ship tasks"}`))
	})

	serveCollectionsFor(mux, "docapp")
	trapCanonicalSlugs(t, mux, "docapp")
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
