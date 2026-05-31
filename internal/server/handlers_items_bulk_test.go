package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
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

// TestBulkItems_Restore covers the undo path (TASK-1674): a bulk archive
// followed by a bulk restore brings the items back, resolving the
// archived (soft-deleted) rows that ResolveItem normally hides.
func TestBulkItems_Restore(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	a := createBulkTestItem(t, srv, ws, "A", `{"status":"open"}`)
	b := createBulkTestItem(t, srv, ws, "B", `{"status":"open"}`)

	// Archive both.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
		"ids": []string{a.Ref, b.Ref}, "op": "archive",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk archive: %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items", nil)
	var items []models.Item
	parseJSON(t, rr, &items)
	if len(items) != 0 {
		t.Fatalf("expected 0 live items after archive, got %d", len(items))
	}

	// Restore by id (the bulk response / undo passes ids).
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
		"ids": []string{a.ID, b.ID}, "op": "restore",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk restore: %d: %s", rr.Code, rr.Body.String())
	}
	var resp bulkItemsResponse
	parseJSON(t, rr, &resp)
	if len(resp.Updated) != 2 || len(resp.Failed) != 0 {
		t.Fatalf("expected 2 restored / 0 failed, got %+v", resp)
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items", nil)
	parseJSON(t, rr, &items)
	if len(items) != 2 {
		t.Errorf("expected 2 live items after restore, got %d", len(items))
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

// TestBulkItems_StatusMoveRunsOpenChildrenGuard confirms a bulk
// status move to a terminal value is rejected (per-row) while the
// item still has open children — same guard the single PATCH path runs.
func TestBulkItems_StatusMoveRunsOpenChildrenGuard(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	plan, _ := seedParentAndChildren(t, srv, ws, []string{"open"})

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
		"ids": []string{plan.Ref}, "op": "move", "status": "completed",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 envelope, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp bulkItemsResponse
	parseJSON(t, rr, &resp)
	if len(resp.Updated) != 0 || len(resp.Failed) != 1 {
		t.Fatalf("expected 0 updated / 1 failed, got %+v", resp)
	}
	if resp.Failed[0].Code != "open_children" {
		t.Errorf("expected open_children code, got %q (%s)", resp.Failed[0].Code, resp.Failed[0].Error)
	}

	// force=true escapes the guard.
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
		"ids": []string{plan.Ref}, "op": "move", "status": "completed", "force": true,
	})
	parseJSON(t, rr, &resp)
	if len(resp.Updated) != 1 {
		t.Fatalf("force should bypass guard: %+v", resp)
	}
}

// TestBulkItems_CollectionMoveRunsOpenChildrenGuard confirms a bulk
// collection move that also sets a terminal status runs the guard
// against the destination schema (Codex round-1 finding).
func TestBulkItems_CollectionMoveRunsOpenChildrenGuard(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	collResp := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections", map[string]interface{}{
		"name":   "Programs",
		"icon":   "package",
		"schema": `{"fields":[{"key":"status","label":"Status","type":"select","options":["active","completed"],"terminal_options":["completed"]}]}`,
	})
	if collResp.Code != http.StatusCreated {
		t.Fatalf("create programs: %d %s", collResp.Code, collResp.Body.String())
	}

	plan, _ := seedParentAndChildren(t, srv, ws, []string{"open"})

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
		"ids": []string{plan.Ref}, "op": "move", "collection": "programs", "status": "completed",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 envelope, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp bulkItemsResponse
	parseJSON(t, rr, &resp)
	if len(resp.Failed) != 1 || resp.Failed[0].Code != "open_children" {
		t.Fatalf("expected open_children failure, got %+v", resp)
	}

	// The plan must NOT have moved.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+plan.Ref, nil)
	var fresh models.Item
	parseJSON(t, rr, &fresh)
	if fresh.CollectionSlug != "plans" {
		t.Errorf("plan moved despite guard rejection — now in %q", fresh.CollectionSlug)
	}
}

// TestBulkItems_EmitsCollectionScopedBatchEvent asserts the batch SSE
// event is scoped to its collection (so the SSE visibility filter routes
// it correctly) and carries NO per-item IDs (no leak on a broadcast bus).
func TestBulkItems_EmitsCollectionScopedBatchEvent(t *testing.T) {
	srv := testServerWithEvents(t)
	ws := createWSWithCollections(t, srv)
	wsRow, err := srv.store.GetWorkspaceBySlug(ws)
	if err != nil || wsRow == nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	ch := srv.events.Subscribe(wsRow.ID)
	defer srv.events.Unsubscribe(ch)

	a := createBulkTestItem(t, srv, ws, "A", `{"status":"open","priority":"low"}`)
	b := createBulkTestItem(t, srv, ws, "B", `{"status":"open","priority":"low"}`)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
		"ids": []string{a.Ref, b.Ref}, "op": "set-priority", "priority": "high",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk: %d: %s", rr.Code, rr.Body.String())
	}

	var bulk *events.Event
	deadline := time.After(2 * time.Second)
loop:
	for {
		select {
		case ev := <-ch:
			if ev.Type == events.ItemsBulkUpdated {
				e := ev
				bulk = &e
				break loop
			}
		case <-deadline:
			break loop
		}
	}
	if bulk == nil {
		t.Fatal("no items_bulk_updated event published")
	}
	if bulk.Collection != "tasks" {
		t.Errorf("expected Collection=tasks, got %q", bulk.Collection)
	}
	if bulk.Count != 2 {
		t.Errorf("expected Count=2, got %d", bulk.Count)
	}
	if bulk.Op != "set-priority" {
		t.Errorf("expected Op=set-priority, got %q", bulk.Op)
	}
	// The wire payload must not leak per-item IDs.
	raw, _ := json.Marshal(bulk)
	if strings.Contains(string(raw), "item_ids") {
		t.Errorf("batch SSE event must not carry item_ids: %s", raw)
	}
}

// TestBulkItems_CollectionMoveNotifiesBothScopes asserts a bulk
// collection move emits a batch event for BOTH the source and target
// collections, so a member watching either lane reconciles.
func TestBulkItems_CollectionMoveNotifiesBothScopes(t *testing.T) {
	srv := testServerWithEvents(t)
	ws := createWSWithCollections(t, srv)
	wsRow, err := srv.store.GetWorkspaceBySlug(ws)
	if err != nil || wsRow == nil {
		t.Fatalf("resolve workspace: %v", err)
	}

	collResp := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections", map[string]interface{}{
		"name":   "Programs",
		"icon":   "package",
		"schema": `{"fields":[{"key":"status","label":"Status","type":"select","options":["active","completed"]}]}`,
	})
	if collResp.Code != http.StatusCreated {
		t.Fatalf("create programs: %d %s", collResp.Code, collResp.Body.String())
	}

	a := createBulkTestItem(t, srv, ws, "A", `{"status":"open"}`)

	ch := srv.events.Subscribe(wsRow.ID)
	defer srv.events.Unsubscribe(ch)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
		"ids": []string{a.Ref}, "op": "move", "collection": "programs",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk move: %d: %s", rr.Code, rr.Body.String())
	}

	gotScopes := map[string]bool{}
	deadline := time.After(2 * time.Second)
collect:
	for {
		select {
		case ev := <-ch:
			if ev.Type == events.ItemsBulkUpdated {
				gotScopes[ev.Collection] = true
				if gotScopes["tasks"] && gotScopes["programs"] {
					break collect
				}
			}
		case <-deadline:
			break collect
		}
	}
	if !gotScopes["tasks"] {
		t.Error("expected a batch event for source collection 'tasks'")
	}
	if !gotScopes["programs"] {
		t.Error("expected a batch event for target collection 'programs'")
	}

	// The bulk move must log a "moved" activity with from/to collection
	// slugs — /items-changes reads this to emit moved-out tombstones
	// (BUG-1675). A generic "updated" action would silently break it.
	acts, err := srv.store.ListDocumentActivity(a.ID, models.ActivityListParams{Limit: 20})
	if err != nil {
		t.Fatalf("list activity: %v", err)
	}
	foundMoved := false
	for _, act := range acts {
		if act.Action == "moved" &&
			strings.Contains(act.Metadata, `"from_collection":"tasks"`) &&
			strings.Contains(act.Metadata, `"to_collection":"programs"`) {
			foundMoved = true
		}
	}
	if !foundMoved {
		t.Errorf("expected a 'moved' activity with from=tasks to=programs; got %+v", acts)
	}
}

// TestBulkItems_CollectionMoveValidatesStatusOverride confirms a status
// override on a collection move is validated against the target schema —
// an out-of-options value is rejected (per-row), not written.
func TestBulkItems_CollectionMoveValidatesStatusOverride(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	collResp := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections", map[string]interface{}{
		"name":   "Programs",
		"icon":   "package",
		"schema": `{"fields":[{"key":"status","label":"Status","type":"select","options":["active","completed"]}]}`,
	})
	if collResp.Code != http.StatusCreated {
		t.Fatalf("create programs: %d %s", collResp.Code, collResp.Body.String())
	}

	a := createBulkTestItem(t, srv, ws, "A", `{"status":"open"}`)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
		"ids": []string{a.Ref}, "op": "move", "collection": "programs", "status": "bogus",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 envelope, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp bulkItemsResponse
	parseJSON(t, rr, &resp)
	if len(resp.Updated) != 0 || len(resp.Failed) != 1 {
		t.Fatalf("expected 0 updated / 1 failed for invalid status, got %+v", resp)
	}
	if resp.Failed[0].Code != "validation_error" {
		t.Errorf("expected validation_error, got %q (%s)", resp.Failed[0].Code, resp.Failed[0].Error)
	}

	// The item must NOT have moved.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+a.Slug, nil)
	var fresh models.Item
	parseJSON(t, rr, &fresh)
	if fresh.CollectionSlug != "tasks" {
		t.Errorf("item moved despite invalid status — now in %q", fresh.CollectionSlug)
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
