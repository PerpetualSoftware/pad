package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestArchivedItem_Get200WithMarker_Update409 pins BUG-1791. An archived
// (soft-deleted) item still appears in include-archived list results, so the
// API must let a caller fetch it read-only (GET 200 + deleted_at) and, when
// they try to mutate it, return a clear "archived" conflict instead of a bare
// 404 that reads as data loss / index corruption.
func TestArchivedItem_Get200WithMarker_Update409(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title": "Doomed task",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var created models.Item
	parseJSON(t, rr, &created)

	// Archive (soft-delete) it.
	rr = doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/items/"+created.Slug, nil)
	if rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
		t.Fatalf("delete item: expected 200/204, got %d: %s", rr.Code, rr.Body.String())
	}

	// GET must return the archived item (200) with deleted_at populated, so an
	// agent can read it and see that it is archived.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+created.Slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get archived item: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got models.Item
	parseJSON(t, rr, &got)
	if got.DeletedAt == nil {
		t.Errorf("expected deleted_at populated on archived GET, got nil (body=%s)", rr.Body.String())
	}

	// UPDATE must return a clear archived conflict (409, code "archived").
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+created.Slug, map[string]interface{}{})
	if rr.Code != http.StatusConflict {
		t.Fatalf("update archived item: expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
	var errResp map[string]map[string]string
	parseJSON(t, rr, &errResp)
	if errResp["error"]["code"] != "archived" {
		t.Errorf("expected error code 'archived', got %q (body=%s)", errResp["error"]["code"], rr.Body.String())
	}

	// MOVE must likewise return the archived conflict, not a bare 404
	// (Codex round 1: the move path is a peer mutation and was missed).
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+created.Slug+"/move", map[string]interface{}{
		"target_collection": "ideas",
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("move archived item: expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
	parseJSON(t, rr, &errResp)
	if errResp["error"]["code"] != "archived" {
		t.Errorf("move: expected error code 'archived', got %q (body=%s)", errResp["error"]["code"], rr.Body.String())
	}
}
