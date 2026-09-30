package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3307, through the real routes: a workspace named like one soft-deleted
// in the last 30 days was handed that workspace's slug, which the global
// UNIQUE(slug) refused, and the create answered 500.
func TestCreateWorkspace_SoftDeletedNameThroughTheRoutes(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	create := func() (int, string) {
		rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Gone Soon"})
		var ws struct {
			Slug string `json:"slug"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &ws)
		return rr.Code, ws.Slug
	}
	code, first := create()
	if code != http.StatusCreated || first == "" {
		t.Fatalf("precondition: first create answered %d", code)
	}
	if rr := doRequest(srv, "DELETE", "/api/v1/workspaces/"+first, nil); rr.Code >= 300 {
		t.Fatalf("precondition: delete answered %d: %s", rr.Code, rr.Body.String())
	}
	code, second := create()
	if code != http.StatusCreated {
		t.Fatalf("create with a soft-deleted workspace's name answered %d, want 201", code)
	}
	if second == first {
		t.Fatalf("the new workspace took the soft-deleted one's slug %q", first)
	}
}

// Contention the store gave up on is a 409, never a 500. The helper is what
// the create and JSON-import doors call; the bundle door builds the same
// status inline.
func TestWriteWorkspaceSlugContended(t *testing.T) {
	t.Parallel()
	rr := httptest.NewRecorder()
	wrapped := fmt.Errorf("create workspace: %w", &store.WorkspaceSlugContendedError{Slug: "x", Attempts: 10})
	if !writeWorkspaceSlugContended(rr, wrapped) {
		t.Fatal("the wrapped contention error was not recognised")
	}
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	if writeWorkspaceSlugContended(httptest.NewRecorder(), fmt.Errorf("something else")) {
		t.Fatal("CONTROL: an unrelated error was answered as contention")
	}
}
