package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/xarmian/pad/internal/models"
)

// createWSWithCollections creates a workspace and returns its slug.
// The workspace will have default collections seeded automatically.
func createWSWithCollections(t *testing.T, srv *Server) string {
	t.Helper()
	slug := createWSForTest(t, srv)
	return slug
}

func TestCollectionCRUD(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// List collections — should have 4 defaults (tasks, ideas, phases, docs)
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list collections: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var colls []models.Collection
	parseJSON(t, rr, &colls)
	if len(colls) != 6 {
		t.Fatalf("expected 6 default collections, got %d", len(colls))
	}

	// Create a custom collection
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name":        "Bugs",
		"icon":        "bug",
		"description": "Bug tracker",
		"schema":      `{"fields":[{"key":"severity","label":"Severity","type":"select","options":["low","medium","high"]}]}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var coll models.Collection
	parseJSON(t, rr, &coll)
	if coll.Slug != "bugs" {
		t.Errorf("expected slug 'bugs', got %q", coll.Slug)
	}
	if coll.Name != "Bugs" {
		t.Errorf("expected name 'Bugs', got %q", coll.Name)
	}

	// Get collection by slug
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections/bugs", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get collection: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var fetched models.Collection
	parseJSON(t, rr, &fetched)
	if fetched.ID != coll.ID {
		t.Errorf("expected id %q, got %q", coll.ID, fetched.ID)
	}

	// Update collection
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/bugs", map[string]interface{}{
		"name": "Bug Reports",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("update collection: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var updated models.Collection
	parseJSON(t, rr, &updated)
	if updated.Name != "Bug Reports" {
		t.Errorf("expected name 'Bug Reports', got %q", updated.Name)
	}

	// Delete custom collection (should work)
	rr = doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/collections/bug-reports", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete collection: expected 204, got %d: %s", rr.Code, rr.Body.String())
	}

	// Delete default collection (should fail)
	rr = doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/collections/tasks", nil)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for deleting default collection, got %d: %s", rr.Code, rr.Body.String())
	}

	// Not found
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections/nonexistent", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestCollectionCreateValidation(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Missing name
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing name, got %d", rr.Code)
	}
}

func TestItemCRUD(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create item in tasks collection
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":   "Fix login bug",
		"content": "Users can't log in with special chars in password",
		"fields":  `{"status":"open","priority":"high"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var item models.Item
	parseJSON(t, rr, &item)
	if item.Title != "Fix login bug" {
		t.Errorf("expected title 'Fix login bug', got %q", item.Title)
	}
	if item.CollectionSlug != "tasks" {
		t.Errorf("expected collection slug 'tasks', got %q", item.CollectionSlug)
	}

	// Verify defaults were applied to fields
	var fields map[string]interface{}
	if err := json.Unmarshal([]byte(item.Fields), &fields); err != nil {
		t.Fatalf("failed to unmarshal fields: %v", err)
	}
	if fields["status"] != "open" {
		t.Errorf("expected status 'open', got %v", fields["status"])
	}
	if fields["priority"] != "high" {
		t.Errorf("expected priority 'high', got %v", fields["priority"])
	}

	// Get item by slug
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get item: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var fetched models.Item
	parseJSON(t, rr, &fetched)
	if fetched.ID != item.ID {
		t.Errorf("expected id %q, got %q", item.ID, fetched.ID)
	}

	// Update item fields
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"fields": `{"status":"in-progress","priority":"high"}`,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("update item fields: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var updatedItem models.Item
	parseJSON(t, rr, &updatedItem)
	var updatedFields map[string]interface{}
	json.Unmarshal([]byte(updatedItem.Fields), &updatedFields)
	if updatedFields["status"] != "in-progress" {
		t.Errorf("expected status 'in-progress', got %v", updatedFields["status"])
	}

	// Update item content
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"content": "Updated description of the login bug",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("update item content: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// List items in collection
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections/tasks/items", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list collection items: expected 200, got %d", rr.Code)
	}

	var collItems []models.Item
	parseJSON(t, rr, &collItems)
	if len(collItems) != 1 {
		t.Errorf("expected 1 item in tasks, got %d", len(collItems))
	}

	// List all items cross-collection
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list items: expected 200, got %d", rr.Code)
	}

	var allItems []models.Item
	parseJSON(t, rr, &allItems)
	if len(allItems) != 1 {
		t.Errorf("expected 1 total item, got %d", len(allItems))
	}

	// Delete (archive) item
	rr = doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete item: expected 204, got %d: %s", rr.Code, rr.Body.String())
	}

	// Should not appear in default list
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items", nil)
	parseJSON(t, rr, &allItems)
	if len(allItems) != 0 {
		t.Errorf("expected 0 items after archive, got %d", len(allItems))
	}

	// Restore item
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/restore", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("restore item: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Should appear again
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items", nil)
	parseJSON(t, rr, &allItems)
	if len(allItems) != 1 {
		t.Errorf("expected 1 item after restore, got %d", len(allItems))
	}
}

func TestListCollectionItemsResolvesRelationFieldFilterRefs(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	phaseResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/phases/items", map[string]interface{}{
		"title":  "Agent Workflow Intelligence",
		"fields": `{"status":"active"}`,
	})
	if phaseResp.Code != http.StatusCreated {
		t.Fatalf("create phase: expected 201, got %d: %s", phaseResp.Code, phaseResp.Body.String())
	}

	var phase models.Item
	parseJSON(t, phaseResp, &phase)
	if phase.Ref == "" {
		t.Fatal("expected phase ref to be populated")
	}

	taskResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Add relation filter resolution",
		"fields": `{"status":"open","phase":"` + phase.Ref + `"}`,
	})
	if taskResp.Code != http.StatusCreated {
		t.Fatalf("create task: expected 201, got %d: %s", taskResp.Code, taskResp.Body.String())
	}

	otherTaskResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Unrelated task",
		"fields": `{"status":"open"}`,
	})
	if otherTaskResp.Code != http.StatusCreated {
		t.Fatalf("create unrelated task: expected 201, got %d: %s", otherTaskResp.Code, otherTaskResp.Body.String())
	}

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections/tasks/items?phase="+phase.Ref, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list tasks by phase ref: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var items []models.Item
	parseJSON(t, rr, &items)
	if len(items) != 1 {
		t.Fatalf("expected 1 task for phase ref filter, got %d", len(items))
	}
	if items[0].Title != "Add relation filter resolution" {
		t.Fatalf("unexpected task returned: %q", items[0].Title)
	}
}

func TestListItemsResolvesRelationFieldFilterRefsAcrossCollections(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	phaseResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/phases/items", map[string]interface{}{
		"title":  "Open Source Launch",
		"fields": `{"status":"active"}`,
	})
	if phaseResp.Code != http.StatusCreated {
		t.Fatalf("create phase: expected 201, got %d: %s", phaseResp.Code, phaseResp.Body.String())
	}

	var phase models.Item
	parseJSON(t, phaseResp, &phase)

	taskResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Document release filters",
		"fields": `{"status":"open","phase":"` + phase.Ref + `"}`,
	})
	if taskResp.Code != http.StatusCreated {
		t.Fatalf("create task: expected 201, got %d: %s", taskResp.Code, taskResp.Body.String())
	}

	docResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/docs/items", map[string]interface{}{
		"title":  "Release Notes",
		"fields": `{"status":"draft","category":"launch"}`,
	})
	if docResp.Code != http.StatusCreated {
		t.Fatalf("create doc: expected 201, got %d: %s", docResp.Code, docResp.Body.String())
	}

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items?phase="+phase.Ref, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list items by phase ref: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var items []models.Item
	parseJSON(t, rr, &items)
	if len(items) != 1 {
		t.Fatalf("expected 1 item for cross-collection phase ref filter, got %d", len(items))
	}
	if items[0].CollectionSlug != "tasks" {
		t.Fatalf("expected task item, got collection %q", items[0].CollectionSlug)
	}
}

func TestItemCreateValidation(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Missing title
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"content": "No title",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing title, got %d", rr.Code)
	}

	// Invalid field value (bad select option)
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Test Task",
		"fields": `{"status":"invalid_status"}`,
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid field value, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify it returns a validation_error code
	var errResp map[string]map[string]string
	parseJSON(t, rr, &errResp)
	if errResp["error"]["code"] != "validation_error" {
		t.Errorf("expected error code 'validation_error', got %q", errResp["error"]["code"])
	}

	// Non-existent collection
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/nonexistent/items", map[string]interface{}{
		"title": "Test",
	})
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for non-existent collection, got %d", rr.Code)
	}

	// Item not found
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/nonexistent-slug", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestItemFieldDefaults(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create item without specifying fields — defaults should be applied
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title": "Default Fields Task",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var item models.Item
	parseJSON(t, rr, &item)

	var fields map[string]interface{}
	json.Unmarshal([]byte(item.Fields), &fields)

	// Tasks schema has status default="open" and priority default="medium"
	if fields["status"] != "open" {
		t.Errorf("expected default status 'open', got %v", fields["status"])
	}
	if fields["priority"] != "medium" {
		t.Errorf("expected default priority 'medium', got %v", fields["priority"])
	}
}

func TestItemListWithFilters(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create multiple items with different statuses
	doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Open Task",
		"fields": `{"status":"open","priority":"high"}`,
	})
	doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "In Progress Task",
		"fields": `{"status":"in-progress","priority":"medium"}`,
	})
	doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Done Task",
		"fields": `{"status":"done","priority":"low"}`,
	})

	// Filter by status
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items?status=open", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("filter by status: expected 200, got %d", rr.Code)
	}

	var filtered []models.Item
	parseJSON(t, rr, &filtered)
	if len(filtered) != 1 {
		t.Errorf("expected 1 open item, got %d", len(filtered))
	}

	// Filter by priority
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items?priority=high", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("filter by priority: expected 200, got %d", rr.Code)
	}

	parseJSON(t, rr, &filtered)
	if len(filtered) != 1 {
		t.Errorf("expected 1 high priority item, got %d", len(filtered))
	}

	// Limit and offset
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items?limit=2", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("limit: expected 200, got %d", rr.Code)
	}

	parseJSON(t, rr, &filtered)
	if len(filtered) != 2 {
		t.Errorf("expected 2 items with limit, got %d", len(filtered))
	}
}

func TestItemSearch(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":   "OAuth Migration",
		"content": "Migrate authentication to OAuth2 flow",
		"fields":  `{"status":"open"}`,
	})
	doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":   "Database Upgrade",
		"content": "Upgrade PostgreSQL to version 16",
		"fields":  `{"status":"open"}`,
	})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items?search=OAuth", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("search: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var results []models.Item
	parseJSON(t, rr, &results)
	if len(results) != 1 {
		t.Errorf("expected 1 search result, got %d", len(results))
	}
	if len(results) > 0 && results[0].Title != "OAuth Migration" {
		t.Errorf("expected 'OAuth Migration', got %q", results[0].Title)
	}
}

func TestItemLinks(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create two items
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Task A",
		"fields": `{"status":"open"}`,
	})
	var itemA models.Item
	parseJSON(t, rr, &itemA)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Task B",
		"fields": `{"status":"open"}`,
	})
	var itemB models.Item
	parseJSON(t, rr, &itemB)

	// Create link from A to B
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+itemA.Slug+"/links", map[string]interface{}{
		"target_id": itemB.ID,
		"link_type": "blocks",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create link: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var link models.ItemLink
	parseJSON(t, rr, &link)
	if link.SourceID != itemA.ID {
		t.Errorf("expected source_id %q, got %q", itemA.ID, link.SourceID)
	}
	if link.TargetID != itemB.ID {
		t.Errorf("expected target_id %q, got %q", itemB.ID, link.TargetID)
	}
	if link.LinkType != "blocks" {
		t.Errorf("expected link_type 'blocks', got %q", link.LinkType)
	}

	// Get links for item A
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+itemA.Slug+"/links", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get links: expected 200, got %d", rr.Code)
	}

	var links []models.ItemLink
	parseJSON(t, rr, &links)
	if len(links) != 1 {
		t.Errorf("expected 1 link, got %d", len(links))
	}

	// Get links for item B (should also see the link)
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+itemB.Slug+"/links", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get links B: expected 200, got %d", rr.Code)
	}

	parseJSON(t, rr, &links)
	if len(links) != 1 {
		t.Errorf("expected 1 link for B, got %d", len(links))
	}

	// Delete link
	rr = doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/links/"+link.ID, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete link: expected 204, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify link is gone
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+itemA.Slug+"/links", nil)
	parseJSON(t, rr, &links)
	if len(links) != 0 {
		t.Errorf("expected 0 links after delete, got %d", len(links))
	}

	// Create link with missing target
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+itemA.Slug+"/links", map[string]interface{}{
		"target_id": "",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing target_id, got %d", rr.Code)
	}

	// Delete non-existent link
	rr = doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/links/nonexistent-id", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for non-existent link, got %d", rr.Code)
	}
}

func TestGetItemIncludesDerivedClosureForSupersededItems(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Replacement Task",
		"fields": `{"status":"done"}`,
	})
	var replacement models.Item
	parseJSON(t, rr, &replacement)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Legacy Task",
		"fields": `{"status":"open"}`,
	})
	var legacy models.Item
	parseJSON(t, rr, &legacy)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+replacement.Slug+"/links", map[string]interface{}{
		"target_id": legacy.ID,
		"link_type": models.ItemLinkTypeSupersedes,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create supersedes link: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+legacy.Slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get legacy item: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var fetched models.Item
	parseJSON(t, rr, &fetched)
	if fetched.DerivedClosure == nil {
		t.Fatal("expected derived closure for superseded item")
	}
	if fetched.DerivedClosure.Kind != "superseded_by" {
		t.Fatalf("expected superseded_by closure, got %q", fetched.DerivedClosure.Kind)
	}
	if !fetched.DerivedClosure.IsClosed {
		t.Fatal("expected derived closure to mark item closed")
	}
	if len(fetched.DerivedClosure.RelatedItems) != 1 {
		t.Fatalf("expected 1 related item, got %d", len(fetched.DerivedClosure.RelatedItems))
	}
	if fetched.DerivedClosure.RelatedItems[0].Ref != "TASK-1" {
		t.Fatalf("expected related ref TASK-1, got %q", fetched.DerivedClosure.RelatedItems[0].Ref)
	}
}

func TestGetItemIncludesDerivedClosureWhenSplitChildrenAreDone(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Parent Task",
		"fields": `{"status":"open"}`,
	})
	var parent models.Item
	parseJSON(t, rr, &parent)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Child One",
		"fields": `{"status":"done"}`,
	})
	var childOne models.Item
	parseJSON(t, rr, &childOne)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Child Two",
		"fields": `{"status":"done"}`,
	})
	var childTwo models.Item
	parseJSON(t, rr, &childTwo)

	for _, child := range []models.Item{childOne, childTwo} {
		rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+child.Slug+"/links", map[string]interface{}{
			"target_id": parent.ID,
			"link_type": models.ItemLinkTypeSplitFrom,
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create split_from link for %s: expected 201, got %d: %s", child.Title, rr.Code, rr.Body.String())
		}
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+parent.Slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get parent item: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var fetched models.Item
	parseJSON(t, rr, &fetched)
	if fetched.DerivedClosure == nil {
		t.Fatal("expected derived closure for split parent")
	}
	if fetched.DerivedClosure.Kind != "split_into" {
		t.Fatalf("expected split_into closure, got %q", fetched.DerivedClosure.Kind)
	}
	if len(fetched.DerivedClosure.RelatedItems) != 2 {
		t.Fatalf("expected 2 related split children, got %d", len(fetched.DerivedClosure.RelatedItems))
	}
}

func TestGetItemIncludesDerivedClosureForImplementedItems(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Implementation Task",
		"fields": `{"status":"done"}`,
	})
	var implementer models.Item
	parseJSON(t, rr, &implementer)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/ideas/items", map[string]interface{}{
		"title":  "Search UX Idea",
		"fields": `{"status":"planned"}`,
	})
	var idea models.Item
	parseJSON(t, rr, &idea)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+implementer.Slug+"/links", map[string]interface{}{
		"target_id": idea.ID,
		"link_type": models.ItemLinkTypeImplements,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create implements link: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+idea.Slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get implemented item: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var fetched models.Item
	parseJSON(t, rr, &fetched)
	if fetched.DerivedClosure == nil {
		t.Fatal("expected derived closure for implemented item")
	}
	if fetched.DerivedClosure.Kind != "implemented_by" {
		t.Fatalf("expected implemented_by closure, got %q", fetched.DerivedClosure.Kind)
	}
	if len(fetched.DerivedClosure.RelatedItems) != 1 {
		t.Fatalf("expected 1 implementing item, got %d", len(fetched.DerivedClosure.RelatedItems))
	}
	if fetched.DerivedClosure.RelatedItems[0].CollectionSlug != "tasks" {
		t.Fatalf("expected implementing item collection slug tasks, got %q", fetched.DerivedClosure.RelatedItems[0].CollectionSlug)
	}
}

func TestGetItemIncludesCodeContext(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Linked Task",
		"fields": `{"status":"open"}`,
	})
	var item models.Item
	parseJSON(t, rr, &item)

	fields := `{"status":"open","github_pr":{"number":40,"url":"https://github.com/xarmian/pad/pull/40","title":"Surface lineage relationships and derived closure for TASK-122","state":"MERGED","branch":"feat/task-122-lineage-display","repo":"xarmian/pad","updated_at":"2026-04-02T14:46:09Z"}}`
	updated, err := srv.store.UpdateItem(item.ID, models.ItemUpdate{Fields: &fields})
	if err != nil {
		t.Fatalf("update item fields: %v", err)
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+updated.Slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get item: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var fetched models.Item
	parseJSON(t, rr, &fetched)
	if fetched.CodeContext == nil {
		t.Fatal("expected code context in item response")
	}
	if fetched.CodeContext.Branch != "feat/task-122-lineage-display" {
		t.Fatalf("expected branch metadata, got %q", fetched.CodeContext.Branch)
	}
	if fetched.CodeContext.PullRequest == nil || fetched.CodeContext.PullRequest.Number != 40 {
		t.Fatalf("expected PR metadata, got %#v", fetched.CodeContext.PullRequest)
	}
}

func TestGetItemIncludesStructuredNotes(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Capture reasoning",
		"fields": `{"status":"open"}`,
	})
	var item models.Item
	parseJSON(t, rr, &item)

	fields := `{"status":"open","implementation_notes":[{"id":"note-1","summary":"Add typed item metadata","details":"Expose the new arrays as top-level API fields","created_at":"2026-04-02T16:30:00Z","created_by":"agent"}],"decision_log":[{"id":"decision-1","decision":"Store notes in reserved field keys","rationale":"This keeps the first cut backward-compatible","created_at":"2026-04-02T16:35:00Z","created_by":"agent"}]}`
	updated, err := srv.store.UpdateItem(item.ID, models.ItemUpdate{Fields: &fields})
	if err != nil {
		t.Fatalf("update item fields: %v", err)
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+updated.Slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get item: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var fetched models.Item
	parseJSON(t, rr, &fetched)
	if len(fetched.ImplementationNotes) != 1 {
		t.Fatalf("expected 1 implementation note, got %#v", fetched.ImplementationNotes)
	}
	if fetched.ImplementationNotes[0].Summary != "Add typed item metadata" {
		t.Fatalf("expected implementation note summary, got %q", fetched.ImplementationNotes[0].Summary)
	}
	if len(fetched.DecisionLog) != 1 {
		t.Fatalf("expected 1 decision log entry, got %#v", fetched.DecisionLog)
	}
	if fetched.DecisionLog[0].Decision != "Store notes in reserved field keys" {
		t.Fatalf("expected decision log entry, got %q", fetched.DecisionLog[0].Decision)
	}
}

func TestGetItemIncludesConventionMetadata(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/conventions/items", map[string]interface{}{
		"title":  "Run tests before completing tasks",
		"fields": `{"status":"active"}`,
	})
	var item models.Item
	parseJSON(t, rr, &item)

	fields := `{"status":"active","convention":{"category":"quality","trigger":"on-task-complete","surfaces":["all"],"enforcement":"must","commands":["go test ./...","make install"]}}`
	updated, err := srv.store.UpdateItem(item.ID, models.ItemUpdate{Fields: &fields})
	if err != nil {
		t.Fatalf("update item fields: %v", err)
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+updated.Slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get item: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var fetched models.Item
	parseJSON(t, rr, &fetched)
	if fetched.Convention == nil {
		t.Fatal("expected convention metadata in item response")
	}
	if fetched.Convention.Category != "quality" {
		t.Fatalf("expected category quality, got %q", fetched.Convention.Category)
	}
	if fetched.Convention.Enforcement != "must" {
		t.Fatalf("expected enforcement must, got %q", fetched.Convention.Enforcement)
	}
	if len(fetched.Convention.Commands) != 2 {
		t.Fatalf("expected command references, got %#v", fetched.Convention.Commands)
	}
}

func TestItemVersions(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create item with content
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/docs/items", map[string]interface{}{
		"title":   "Architecture Doc",
		"content": "Initial architecture overview",
		"fields":  `{"status":"draft"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var item models.Item
	parseJSON(t, rr, &item)

	// List versions
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/versions", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list versions: expected 200, got %d", rr.Code)
	}

	var versions []models.Version
	parseJSON(t, rr, &versions)
	if len(versions) != 1 {
		t.Errorf("expected 1 initial version, got %d", len(versions))
	}
}

func TestDashboard(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create some items for the dashboard to report on
	doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Task 1",
		"fields": `{"status":"open","priority":"high"}`,
	})
	doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Task 2",
		"fields": `{"status":"done","priority":"medium"}`,
	})
	doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/ideas/items", map[string]interface{}{
		"title":  "Idea 1",
		"fields": `{"status":"new"}`,
	})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/dashboard", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp DashboardResponse
	parseJSON(t, rr, &resp)

	// Verify summary
	if resp.Summary.TotalItems != 3 {
		t.Errorf("expected 3 total items, got %d", resp.Summary.TotalItems)
	}

	taskCounts, ok := resp.Summary.ByCollection["tasks"]
	if !ok {
		t.Fatal("expected 'tasks' in by_collection summary")
	}
	if taskCounts["open"] != 1 {
		t.Errorf("expected 1 open task, got %d", taskCounts["open"])
	}
	if taskCounts["done"] != 1 {
		t.Errorf("expected 1 done task, got %d", taskCounts["done"])
	}

	ideaCounts, ok := resp.Summary.ByCollection["ideas"]
	if !ok {
		t.Fatal("expected 'ideas' in by_collection summary")
	}
	if ideaCounts["new"] != 1 {
		t.Errorf("expected 1 new idea, got %d", ideaCounts["new"])
	}

	// Verify structure has correct field types (even if empty)
	if resp.ActivePhases == nil {
		t.Error("expected active_phases to be non-nil")
	}
	if resp.Attention == nil {
		t.Error("expected attention to be non-nil")
	}
	if resp.RecentActivity == nil {
		t.Error("expected recent_activity to be non-nil")
	}
	if resp.SuggestedNext == nil {
		t.Error("expected suggested_next to be non-nil")
	}
}

func TestDashboardEmptyWorkspace(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/dashboard", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp DashboardResponse
	parseJSON(t, rr, &resp)

	if resp.Summary.TotalItems != 0 {
		t.Errorf("expected 0 total items, got %d", resp.Summary.TotalItems)
	}
}

func TestItemUpdateFieldValidation(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create valid item
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Valid Task",
		"fields": `{"status":"open"}`,
	})
	var item models.Item
	parseJSON(t, rr, &item)

	// Update with invalid status
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"fields": `{"status":"invalid_option"}`,
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid field update, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestItemCrossCollectionListing(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create items in different collections
	doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Task Item",
		"fields": `{"status":"open"}`,
	})
	doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/ideas/items", map[string]interface{}{
		"title":  "Idea Item",
		"fields": `{"status":"new"}`,
	})
	doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/docs/items", map[string]interface{}{
		"title":  "Doc Item",
		"fields": `{"status":"draft"}`,
	})

	// Cross-collection listing
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list all items: expected 200, got %d", rr.Code)
	}

	var items []models.Item
	parseJSON(t, rr, &items)
	if len(items) != 3 {
		t.Errorf("expected 3 items across all collections, got %d", len(items))
	}

	// Verify each item has collection info
	for _, item := range items {
		if item.CollectionSlug == "" {
			t.Errorf("item %q missing collection_slug", item.Title)
		}
		if item.CollectionName == "" {
			t.Errorf("item %q missing collection_name", item.Title)
		}
	}
}
