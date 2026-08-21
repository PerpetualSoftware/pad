package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/kernelevents"
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

// TestBulkItems_AdminBearer_RestrictedMemberBlockedOnHiddenItem pins
// BUG-1918 for handleBulkItems' own direct checkItemVisible call site
// (the per-ref loop, distinct from the requireItemVisible shim the
// single-item handlers use). Not part of the dispatcher's explicit test
// sweep for this bug, but it's a call site the fix touches directly, so
// it gets its own coverage: a bearer-authed admin who is a restricted
// member (collection_access="specific") gets the hidden-collection item
// reported as a per-ref "not found" failure, same as any other
// restricted member — not silently archived via the old unconditional
// admin bypass — while the visible-collection item still succeeds.
func TestBulkItems_AdminBearer_RestrictedMemberBlockedOnHiddenItem(t *testing.T) {
	f := newBearerGateItemFixture(t)

	rr := doRequestWithHeaders(f.srv, "POST", "/api/v1/workspaces/"+f.ws.Slug+"/items/bulk",
		map[string]any{"ids": []string{f.hiddenItem.Ref, f.visibleItem.Ref}, "op": "archive"},
		f.bearerHeaders())
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk bearer admin: expected 200 envelope, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp bulkItemsResponse
	parseJSON(t, rr, &resp)
	if len(resp.Updated) != 1 || resp.Updated[0].Ref != f.visibleItem.Ref {
		t.Fatalf("expected only the visible item archived, got %+v", resp.Updated)
	}
	if len(resp.Failed) != 1 || resp.Failed[0].Ref != f.hiddenItem.Ref {
		t.Fatalf("expected the hidden item to fail as not-found, got %+v", resp.Failed)
	}

	// Cookie admin — unrestricted, both succeed. Fresh items: the
	// visible-collection item above was already archived by the bearer
	// call, and ResolveItem (used for every op but "restore") hides
	// archived items, so reusing it here would fail for an unrelated
	// reason (already gone) rather than exercising the assertion.
	hidden2 := f.newItem(t, f.hiddenCollID, "Hidden item 2")
	visible2 := f.newItem(t, f.visibleCollID, "Visible item 2")
	rr = doRequestWithCookie(f.srv, "POST", "/api/v1/workspaces/"+f.ws.Slug+"/items/bulk",
		map[string]any{"ids": []string{hidden2.Ref, visible2.Ref}, "op": "archive"},
		f.sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk cookie admin: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var cookieResp bulkItemsResponse
	parseJSON(t, rr, &cookieResp)
	if len(cookieResp.Updated) != 2 {
		t.Fatalf("expected both items archived for cookie admin, got %+v", cookieResp)
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

// TestBulkItems_EveryVerbStampsOneBatchID drives all SEVEN bulk legs through
// the HTTP handler — set-priority, tag, untag, move-status, assign, archive,
// restore — and asserts every event the operation produced shares one batch
// id. (Six OPS; move appears once as a status-only move, which is the field
// path, and cross-collection move goes through the same helper.)
//
// This exists because the store-level test could not catch the bug codex round
// 1 found. That test called the five store methods directly with the option,
// so it proved the option WORKS; it said nothing about whether the handler
// passes it. THREE CALL SITES were missing it, affecting FOUR verbs —
// set-priority and move-status (bulkFieldUpdate), tag and untag
// (bulkTagUpdate), and cross-collection move (bulkMoveCollection) — so those
// member rows stayed unbatched while the header was still written: N
// individual wire deliveries plus a header claiming they were a batch.
//
// The lesson underneath, for the third time in this unit: threading a
// parameter into helper SIGNATURES is not the same work as passing it at every
// CALL. Only a test at the layer that owns the call sites can tell them apart.
func TestBulkItems_EveryVerbStampsOneBatchID(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
		// prep runs before the bulk request (e.g. archiving, so restore has
		// something to restore).
		prep func(t *testing.T, srv *Server, ws string, refs []string)
	}{
		{name: "set-priority", body: map[string]any{"op": "set-priority", "priority": "high"}},
		{name: "tag", body: map[string]any{"op": "tag", "tags": []string{"batched"}}},
		{
			name: "untag",
			body: map[string]any{"op": "untag", "tags": []string{"seed"}},
			prep: func(t *testing.T, srv *Server, ws string, refs []string) {
				// The tag has to EXIST for untag to change anything. Without
				// this the operation is a no-op, no member events are written,
				// and the leg asserts nothing about batching — which is how
				// the first version of this test "passed" for two verbs.
				rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
					"ids": refs, "op": "tag", "tags": []string{"seed"},
				})
				if rr.Code != http.StatusOK {
					t.Fatalf("prep tag: %d: %s", rr.Code, rr.Body.String())
				}
			},
		},
		{name: "move-status", body: map[string]any{"op": "move", "status": "done"}},
		{name: "assign", body: map[string]any{"op": "assign"}},
		{name: "archive", body: map[string]any{"op": "archive"}},
		{
			name: "restore",
			body: map[string]any{"op": "restore"},
			prep: func(t *testing.T, srv *Server, ws string, refs []string) {
				rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", map[string]any{
					"ids": refs, "op": "archive",
				})
				if rr.Code != http.StatusOK {
					t.Fatalf("prep archive: %d: %s", rr.Code, rr.Body.String())
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer(t)
			ws := createWSWithCollections(t, srv)

			if tc.name == "assign" {
				role, err := srv.store.CreateAgentRole(wsIDForSlug(t, srv, ws), models.AgentRoleCreate{Name: "Reviewer"})
				if err != nil {
					t.Fatalf("create role: %v", err)
				}
				tc.body["agent_role_id"] = role.ID
			}

			a := createBulkTestItem(t, srv, ws, "A", `{"status":"open","priority":"low"}`)
			b := createBulkTestItem(t, srv, ws, "B", `{"status":"open","priority":"low"}`)
			refs := []string{a.Ref, b.Ref}
			if tc.prep != nil {
				tc.prep(t, srv, ws, refs)
			}

			// Clear everything the setup emitted so the assertion sees only
			// this operation's rows.
			clearSeededOutbox(t, srv)

			body := map[string]any{"ids": refs}
			for k, v := range tc.body {
				body[k] = v
			}
			rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", body)
			if rr.Code != http.StatusOK {
				t.Fatalf("bulk %s: %d: %s", tc.name, rr.Code, rr.Body.String())
			}

			pending, err := srv.store.ListPendingOutboxEvents(100)
			if err != nil {
				t.Fatalf("list pending: %v", err)
			}
			if len(pending) < 2 {
				t.Fatalf("%s produced %d outbox rows; the batch assertion needs the members AND the header to prove anything", tc.name, len(pending))
			}

			var header int
			batches := map[string]int{}
			for _, ev := range pending {
				batches[ev.BatchID]++
				if ev.EventType == kernelevents.ItemBulkUpdated {
					header++
				}
			}
			if n := batches[""]; n != 0 {
				t.Errorf("%s left %d event(s) unbatched — they would be delivered individually alongside a header claiming they were a batch", tc.name, n)
			}
			if len(batches) != 1 {
				t.Errorf("%s produced %d distinct batch ids, want exactly 1: %v", tc.name, len(batches), batches)
			}
			if header != 1 {
				t.Errorf("%s produced %d batch headers, want exactly 1", tc.name, header)
			}
		})
	}
}

// wsIDForSlug resolves a workspace slug to its id for the store-level calls a
// few fixtures need.
func wsIDForSlug(t *testing.T, srv *Server, slug string) string {
	t.Helper()
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("resolve workspace %q: %v", slug, err)
	}
	return ws.ID
}

// clearSeededOutbox marks everything currently pending as dispatched, so a
// test can assert on the rows one later operation produced.
//
// It claims first, because acking is conditioned on holding the claim (a stale
// pass must not be able to stamp rows someone else owns). Claim-then-ack is
// what the drain itself does.
func clearSeededOutbox(t *testing.T, srv *Server) {
	t.Helper()
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	claimed, err := srv.store.ClaimPendingOutboxEvents("test-clear", 1000, future)
	if err != nil {
		t.Fatalf("claim seeded outbox: %v", err)
	}
	if len(claimed) == 0 {
		return
	}
	ids := make([]string, 0, len(claimed))
	for _, ev := range claimed {
		ids = append(ids, ev.ID)
	}
	if err := srv.store.MarkOutboxDispatched(claimed[0].ClaimToken, ids); err != nil {
		t.Fatalf("clear seeded outbox: %v", err)
	}
}

// TestBulkEventDelta_MatchesTheMutation pins the batch header's delta to what
// the mutation actually committed (codex round 7).
//
// The delta is the one field of the batch payload the drain cannot derive, so
// nothing downstream can correct it: whatever this function says is what a
// webhook consumer believes happened.
func TestBulkEventDelta_MatchesTheMutation(t *testing.T) {
	strptr := func(s string) *string { return &s }

	t.Run("tag trims the way bulkTagUpdate does", func(t *testing.T) {
		got := bulkEventDelta(&bulkItemsRequest{Op: "tag", Tags: []string{" spaced ", "   ", "plain"}})
		tags, _ := got["tags"].([]string)
		if len(tags) != 2 || tags[0] != "spaced" || tags[1] != "plain" {
			t.Errorf("tags = %v, want the trimmed, non-empty set the mutation applies", tags)
		}
	})

	t.Run("untag matches raw, because the mutation does", func(t *testing.T) {
		got := bulkEventDelta(&bulkItemsRequest{Op: "untag", Tags: []string{" spaced "}})
		tags, _ := got["tags"].([]string)
		if len(tags) != 1 || tags[0] != " spaced " {
			t.Errorf("tags = %v, want the raw value — untag removes by exact match", tags)
		}
	})

	t.Run("a non-empty id beats the clear flag", func(t *testing.T) {
		got := bulkEventDelta(&bulkItemsRequest{
			Op:                "assign",
			AssignedUserID:    strptr("user-1"),
			ClearAssignedUser: true,
		})
		if got["assigned_user_id"] != "user-1" {
			t.Errorf("assigned_user_id = %v, want user-1 — the store's SET clause gives the id precedence, so announcing a clear would describe the opposite of the committed row", got["assigned_user_id"])
		}
	})

	t.Run("an explicit empty id clears, like the flag", func(t *testing.T) {
		got := bulkEventDelta(&bulkItemsRequest{Op: "assign", AgentRoleID: strptr("")})
		v, present := got["agent_role_id"]
		if !present || v != nil {
			t.Errorf("agent_role_id = %v (present %v), want an explicit null", v, present)
		}
	})

	t.Run("move carries both when both were sent", func(t *testing.T) {
		got := bulkEventDelta(&bulkItemsRequest{Op: "move", Collection: "done", Status: "closed"})
		if got["collection"] != "done" || got["status"] != "closed" {
			t.Errorf("delta = %v, want both halves of a move-with-status", got)
		}
	})

	t.Run("archive has no delta", func(t *testing.T) {
		if got := bulkEventDelta(&bulkItemsRequest{Op: "archive"}); got != nil {
			t.Errorf("delta = %v, want nil — the operation IS the delta and it is on the envelope as op", got)
		}
	})
}

// TestBulkEventDelta_TagsAreDeduped: bulkTagUpdate skips a tag already in its
// `seen` set, so ["foo", " foo "] adds ONE tag. A delta echoing both would
// advertise a change the mutation did not make (codex round 8).
func TestBulkEventDelta_TagsAreDeduped(t *testing.T) {
	got := bulkEventDelta(&bulkItemsRequest{Op: "tag", Tags: []string{"foo", " foo ", "bar"}})
	tags, _ := got["tags"].([]string)
	if len(tags) != 2 || tags[0] != "foo" || tags[1] != "bar" {
		t.Errorf("tags = %v, want [foo bar] — trimmed and de-duplicated, as the mutation applies them", tags)
	}
}

// TestBulkEventDelta_UntagDedupsWithoutTrimming: untag builds a removal SET
// from the RAW values, so duplicates collapse but whitespace is significant —
// the mirror image of tag, and the delta has to match each side's own rule
// (codex round 9).
func TestBulkEventDelta_UntagDedupsWithoutTrimming(t *testing.T) {
	got := bulkEventDelta(&bulkItemsRequest{Op: "untag", Tags: []string{"foo", "foo", " foo "}})
	tags, _ := got["tags"].([]string)
	if len(tags) != 2 || tags[0] != "foo" || tags[1] != " foo " {
		t.Errorf("tags = %q, want [\"foo\" \" foo \"] — deduped, untrimmed", tags)
	}
}
