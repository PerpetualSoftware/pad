package server

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestStandupKeepsAnOlderCompletionFromASecondCollection(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name":   "Ships",
		"slug":   "ships",
		"prefix": "SHIP",
		"schema": `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"],"default":"open"}]}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create ships collection: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Both work collections must share the query group to reproduce starvation.
	for _, collSlug := range []string{"tasks", "ships"} {
		rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections/"+collSlug, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("get %s collection: expected 200, got %d: %s", collSlug, rr.Code, rr.Body.String())
		}
		var coll models.Collection
		parseJSON(t, rr, &coll)
		field, values := models.CollectionCompletedWorkValues(coll.Schema, coll.Settings)
		if field != "status" || !slices.Contains(values, "done") || !models.CollectionTracksWorkJSON(coll.Settings) {
			t.Fatalf("%s must track work in the (status, done) group: field=%q values=%v settings=%s", collSlug, field, values, coll.Settings)
		}
	}

	for i := 0; i < 21; i++ {
		createItem(t, srv, slug, "tasks", map[string]interface{}{
			"title":  fmt.Sprintf("Fresh task %d", i),
			"fields": `{"status":"done"}`,
		})
	}
	older := createItem(t, srv, slug, "ships", map[string]interface{}{
		"title":  "Older completion in another collection",
		"fields": `{"status":"done"}`,
	})
	backdateItemUpdatedAt(t, srv, older.ID, 2)

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/standup?days=7", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("standup: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp StandupResponse
	parseJSON(t, rr, &resp)
	if len(resp.Completed) != 22 {
		t.Errorf("expected all 22 completions, got %d", len(resp.Completed))
	}
	for _, item := range resp.Completed {
		if item.Ref == older.Ref {
			return
		}
	}
	t.Errorf("missing older completion %s from the second collection", older.Ref)
}

func TestStandupReturnsEveryCompletionPastOnePage(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	idsByRef := make(map[string]string)
	for i := 0; i < 25; i++ {
		item := createItem(t, srv, slug, "tasks", map[string]interface{}{
			"title":  fmt.Sprintf("Fresh task %d", i),
			"fields": `{"status":"done"}`,
		})
		idsByRef[item.Ref] = item.ID
	}

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/standup", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("standup: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp StandupResponse
	parseJSON(t, rr, &resp)
	if len(resp.Completed) != 25 {
		t.Errorf("expected all 25 completions, got %d", len(resp.Completed))
	}
	seen := make(map[string]bool)
	for _, item := range resp.Completed {
		// Standup exposes refs; resolve each to the created item's ID.
		id, ok := idsByRef[item.Ref]
		if !ok {
			t.Fatalf("unexpected completed item %s", item.Ref)
		}
		if seen[id] {
			t.Errorf("duplicate completed item ID %s", id)
		}
		seen[id] = true
	}
}

func TestStandupPagingStopsAtTheCutoff(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	fresh := make(map[string]bool)
	for i := 0; i < 21; i++ {
		item := createItem(t, srv, slug, "tasks", map[string]interface{}{
			"title":  fmt.Sprintf("Fresh task %d", i),
			"fields": `{"status":"done"}`,
		})
		fresh[item.Ref] = true
	}
	for i := 0; i < 3; i++ {
		item := createItem(t, srv, slug, "tasks", map[string]interface{}{
			"title":  fmt.Sprintf("Old task %d", i),
			"fields": `{"status":"done"}`,
		})
		backdateItemUpdatedAt(t, srv, item.ID, 10)
	}

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/standup?days=7", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("standup: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp StandupResponse
	parseJSON(t, rr, &resp)
	if len(resp.Completed) != 21 {
		t.Errorf("expected exactly 21 fresh completions, got %d", len(resp.Completed))
	}
	for _, item := range resp.Completed {
		if !fresh[item.Ref] {
			t.Errorf("unexpected or duplicate completed item %s", item.Ref)
		}
		delete(fresh, item.Ref)
	}
	if len(fresh) != 0 {
		t.Errorf("missing fresh completions: %v", fresh)
	}
}
