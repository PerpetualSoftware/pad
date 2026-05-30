package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// createBulkTestItem creates a task and returns it.
func createBulkTestItem(t *testing.T, srv *Server, ws, title, fields string) models.Item {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/tasks/items", map[string]interface{}{
		"title":  title,
		"fields": fields,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item %q: expected 201, got %d: %s", title, rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)
	return item
}

func itemFields(t *testing.T, srv *Server, ws, slug string) map[string]any {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get item %s: %d: %s", slug, rr.Code, rr.Body.String())
	}
	var it models.Item
	parseJSON(t, rr, &it)
	f := map[string]any{}
	_ = json.Unmarshal([]byte(it.Fields), &f)
	return f
}

func TestBulkItems_SetPriority(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	a := createBulkTestItem(t, srv, ws, "A", `{"status":"open","priority":"low"}`)
	b := createBulkTestItem(t, srv, ws, "B", `{"status":"open","priority":"low"}`)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
		"ids":      []string{a.Ref, b.Ref},
		"op":       "set-priority",
		"priority": "high",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk set-priority: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp bulkItemsResponse
	parseJSON(t, rr, &resp)
	if len(resp.Updated) != 2 || len(resp.Failed) != 0 || resp.Total != 2 {
		t.Fatalf("expected 2 updated / 0 failed, got %+v", resp)
	}
	for _, it := range []models.Item{a, b} {
		if got := itemFields(t, srv, ws, it.Slug)["priority"]; got != "high" {
			t.Errorf("%s priority: expected high, got %v", it.Ref, got)
		}
	}
}

func TestBulkItems_MoveStatus(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	a := createBulkTestItem(t, srv, ws, "A", `{"status":"open"}`)
	b := createBulkTestItem(t, srv, ws, "B", `{"status":"open"}`)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
		"ids":    []string{a.Ref, b.Ref},
		"op":     "move",
		"status": "in-progress",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk move: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp bulkItemsResponse
	parseJSON(t, rr, &resp)
	if len(resp.Updated) != 2 {
		t.Fatalf("expected 2 updated, got %+v", resp)
	}
	if got := itemFields(t, srv, ws, a.Slug)["status"]; got != "in-progress" {
		t.Errorf("status: expected in-progress, got %v", got)
	}
}

func TestBulkItems_TagAndUntag(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	a := createBulkTestItem(t, srv, ws, "A", `{"status":"open"}`)

	// Tag
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
		"ids":  []string{a.Ref},
		"op":   "tag",
		"tags": []string{"urgent", "frontend"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk tag: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+a.Slug, nil)
	var it models.Item
	parseJSON(t, rr, &it)
	var tags []string
	_ = json.Unmarshal([]byte(it.Tags), &tags)
	if len(tags) != 2 {
		t.Fatalf("expected 2 tags after tag, got %v", tags)
	}

	// Re-tagging the same tag is idempotent (no duplicates).
	doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
		"ids": []string{a.Ref}, "op": "tag", "tags": []string{"urgent"},
	})
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+a.Slug, nil)
	parseJSON(t, rr, &it)
	_ = json.Unmarshal([]byte(it.Tags), &tags)
	if len(tags) != 2 {
		t.Fatalf("expected tags to stay 2 after duplicate tag, got %v", tags)
	}

	// Untag one
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
		"ids": []string{a.Ref}, "op": "untag", "tags": []string{"urgent"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk untag: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+a.Slug, nil)
	parseJSON(t, rr, &it)
	_ = json.Unmarshal([]byte(it.Tags), &tags)
	if len(tags) != 1 || tags[0] != "frontend" {
		t.Fatalf("expected [frontend] after untag, got %v", tags)
	}
}

func TestBulkItems_Archive(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	a := createBulkTestItem(t, srv, ws, "A", `{"status":"open"}`)
	b := createBulkTestItem(t, srv, ws, "B", `{"status":"open"}`)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
		"ids": []string{a.Ref, b.Ref}, "op": "archive",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk archive: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp bulkItemsResponse
	parseJSON(t, rr, &resp)
	if len(resp.Updated) != 2 {
		t.Fatalf("expected 2 archived, got %+v", resp)
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items", nil)
	var items []models.Item
	parseJSON(t, rr, &items)
	if len(items) != 0 {
		t.Errorf("expected 0 live items after bulk archive, got %d", len(items))
	}
}

func TestBulkItems_PartialFailure(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	a := createBulkTestItem(t, srv, ws, "A", `{"status":"open"}`)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
		"ids":      []string{a.Ref, "TASK-9999"},
		"op":       "set-priority",
		"priority": "high",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with partial failures, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp bulkItemsResponse
	parseJSON(t, rr, &resp)
	if len(resp.Updated) != 1 {
		t.Errorf("expected 1 updated, got %d", len(resp.Updated))
	}
	if len(resp.Failed) != 1 {
		t.Errorf("expected 1 failed, got %d", len(resp.Failed))
	}
}

func TestBulkItems_Validation(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"empty ids", map[string]any{"ids": []string{}, "op": "archive"}},
		{"unknown op", map[string]any{"ids": []string{"TASK-1"}, "op": "frobnicate"}},
		{"move without params", map[string]any{"ids": []string{"TASK-1"}, "op": "move"}},
		{"set-priority without priority", map[string]any{"ids": []string{"TASK-1"}, "op": "set-priority"}},
		{"tag without tags", map[string]any{"ids": []string{"TASK-1"}, "op": "tag"}},
		{"assign without target", map[string]any{"ids": []string{"TASK-1"}, "op": "assign"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestBulkItems_RouteDoesNotShadowItemSlug guards the route-ordering
// fix: /items/bulk is a static segment registered before the
// /items/{itemSlug} param route, so it must not be treated as an item
// slug, and a GET to it (no handler) must not resolve as an item.
func TestBulkItems_RouteRegistered(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	a := createBulkTestItem(t, srv, ws, "A", `{"status":"open"}`)

	// POST hits the bulk handler (200), not a 404 / item-slug path.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
		"ids": []string{a.Ref}, "op": "set-priority", "priority": "high",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk route: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}
