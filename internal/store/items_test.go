package store

import (
	"fmt"
	"testing"

	"github.com/xarmian/pad/internal/models"
)

// --- Collection helpers ---

func createTestCollection(t *testing.T, s *Store, workspaceID, name string) *models.Collection {
	t.Helper()
	col, err := s.CreateCollection(workspaceID, models.CollectionCreate{
		Name:   name,
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open","required":true}]}`,
	})
	if err != nil {
		t.Fatalf("failed to create collection: %v", err)
	}
	return col
}

func createTestItem(t *testing.T, s *Store, workspaceID, collectionID, title, content string) *models.Item {
	t.Helper()
	item, err := s.CreateItem(workspaceID, collectionID, models.ItemCreate{
		Title:   title,
		Content: content,
		Fields:  `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("failed to create item: %v", err)
	}
	return item
}

// --- Collection Tests ---

func TestCollectionCRUD(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	// Create
	col, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:        "Tasks",
		Icon:        "check-square",
		Description: "Track work items",
		Schema:      `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection error: %v", err)
	}
	if col.Name != "Tasks" {
		t.Errorf("expected name 'Tasks', got %q", col.Name)
	}
	if col.Slug != "tasks" {
		t.Errorf("expected slug 'tasks', got %q", col.Slug)
	}
	if col.Icon != "check-square" {
		t.Errorf("expected icon 'check-square', got %q", col.Icon)
	}

	// Get
	got, err := s.GetCollection(col.ID)
	if err != nil {
		t.Fatalf("GetCollection error: %v", err)
	}
	if got == nil || got.ID != col.ID {
		t.Error("GetCollection returned wrong collection")
	}

	// Get by slug
	got, err = s.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil {
		t.Fatalf("GetCollectionBySlug error: %v", err)
	}
	if got == nil || got.ID != col.ID {
		t.Error("GetCollectionBySlug returned wrong collection")
	}

	// Update
	newName := "My Tasks"
	newIcon := "list"
	updated, err := s.UpdateCollection(col.ID, models.CollectionUpdate{
		Name: &newName,
		Icon: &newIcon,
	})
	if err != nil {
		t.Fatalf("UpdateCollection error: %v", err)
	}
	if updated.Name != "My Tasks" {
		t.Errorf("expected updated name 'My Tasks', got %q", updated.Name)
	}
	if updated.Icon != "list" {
		t.Errorf("expected updated icon 'list', got %q", updated.Icon)
	}
	if updated.Slug != "my-tasks" {
		t.Errorf("expected slug 'my-tasks' after rename, got %q", updated.Slug)
	}

	// List
	list, err := s.ListCollections(ws.ID)
	if err != nil {
		t.Fatalf("ListCollections error: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("expected 1 collection, got %d", len(list))
	}

	// Delete
	err = s.DeleteCollection(col.ID)
	if err != nil {
		t.Fatalf("DeleteCollection error: %v", err)
	}

	// Should not appear in list
	list, _ = s.ListCollections(ws.ID)
	if len(list) != 0 {
		t.Error("deleted collection still appears in list")
	}

	// Should not be found by slug
	got, _ = s.GetCollectionBySlug(ws.ID, "my-tasks")
	if got != nil {
		t.Error("deleted collection still found by slug")
	}
}

func TestCollectionDeleteDefaultRefused(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	// Create a default collection
	col, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:      "Tasks",
		IsDefault: true,
	})
	if err != nil {
		t.Fatalf("CreateCollection error: %v", err)
	}

	// Attempt to delete — should fail
	err = s.DeleteCollection(col.ID)
	if err == nil {
		t.Fatal("expected error when deleting default collection")
	}
	if err.Error() != "cannot delete default collection" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCollectionListWithItemCounts(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	col := createTestCollection(t, s, ws.ID, "Tasks")
	createTestItem(t, s, ws.ID, col.ID, "Task 1", "content")
	createTestItem(t, s, ws.ID, col.ID, "Task 2", "content")

	list, err := s.ListCollections(ws.ID)
	if err != nil {
		t.Fatalf("ListCollections error: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(list))
	}
	if list[0].ItemCount != 2 {
		t.Errorf("expected item_count=2, got %d", list[0].ItemCount)
	}
}

func TestSeedDefaultCollections(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	err := s.SeedDefaultCollections(ws.ID)
	if err != nil {
		t.Fatalf("SeedDefaultCollections error: %v", err)
	}

	list, err := s.ListCollections(ws.ID)
	if err != nil {
		t.Fatalf("ListCollections error: %v", err)
	}
	if len(list) != 6 {
		t.Errorf("expected 6 default collections, got %d", len(list))
	}

	// Verify slugs
	slugs := make(map[string]bool)
	for _, c := range list {
		slugs[c.Slug] = true
		if !c.IsDefault {
			t.Errorf("expected collection %q to be default", c.Slug)
		}
	}
	for _, expected := range []string{"tasks", "ideas", "phases", "docs", "conventions", "playbooks"} {
		if !slugs[expected] {
			t.Errorf("expected default collection %q", expected)
		}
	}

	// Seed again — should be idempotent
	err = s.SeedDefaultCollections(ws.ID)
	if err != nil {
		t.Fatalf("SeedDefaultCollections (idempotent) error: %v", err)
	}
	list, _ = s.ListCollections(ws.ID)
	if len(list) != 6 {
		t.Errorf("expected 6 after re-seed, got %d", len(list))
	}
}

// --- Item Tests ---

func TestItemCRUD(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// Create
	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:   "My Task",
		Content: "Do something",
		Fields:  `{"status":"open"}`,
		Tags:    `["important"]`,
	})
	if err != nil {
		t.Fatalf("CreateItem error: %v", err)
	}
	if item.Title != "My Task" {
		t.Errorf("expected title 'My Task', got %q", item.Title)
	}
	if item.Slug != "my-task" {
		t.Errorf("expected slug 'my-task', got %q", item.Slug)
	}
	if item.CollectionSlug != col.Slug {
		t.Errorf("expected collection slug %q, got %q", col.Slug, item.CollectionSlug)
	}
	if item.CollectionName != col.Name {
		t.Errorf("expected collection name %q, got %q", col.Name, item.CollectionName)
	}

	// Get
	got, err := s.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem error: %v", err)
	}
	if got.Content != "Do something" {
		t.Errorf("expected content 'Do something', got %q", got.Content)
	}

	// Get by slug
	got, err = s.GetItemBySlug(ws.ID, "my-task")
	if err != nil {
		t.Fatalf("GetItemBySlug error: %v", err)
	}
	if got == nil || got.ID != item.ID {
		t.Error("GetItemBySlug returned wrong item")
	}

	// Update
	newContent := "Updated content"
	newTitle := "Updated Task"
	updated, err := s.UpdateItem(item.ID, models.ItemUpdate{
		Title:   &newTitle,
		Content: &newContent,
	})
	if err != nil {
		t.Fatalf("UpdateItem error: %v", err)
	}
	if updated.Content != "Updated content" {
		t.Errorf("expected updated content, got %q", updated.Content)
	}
	if updated.Slug != "updated-task" {
		t.Errorf("expected slug 'updated-task', got %q", updated.Slug)
	}

	// List
	items, err := s.ListItems(ws.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems error: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 item, got %d", len(items))
	}

	// Delete
	err = s.DeleteItem(item.ID)
	if err != nil {
		t.Fatalf("DeleteItem error: %v", err)
	}

	// Should not appear in list
	items, _ = s.ListItems(ws.ID, models.ItemListParams{})
	if len(items) != 0 {
		t.Error("deleted item still appears in list")
	}

	// Restore
	restored, err := s.RestoreItem(item.ID)
	if err != nil {
		t.Fatalf("RestoreItem error: %v", err)
	}
	if restored.Title != "Updated Task" {
		t.Errorf("expected restored title 'Updated Task', got %q", restored.Title)
	}

	items, _ = s.ListItems(ws.ID, models.ItemListParams{})
	if len(items) != 1 {
		t.Error("restored item not in list")
	}
}

func TestItemCodeContextIsHydratedOnRead(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Link PR",
		Fields: `{"status":"open","github_pr":{"number":7,"url":"https://github.com/xarmian/pad/pull/7","title":"Link PR","state":"OPEN","branch":"feat/link-pr","repo":"xarmian/pad","updated_at":"2026-04-02T14:00:00Z"}}`,
	})
	if err != nil {
		t.Fatalf("CreateItem error: %v", err)
	}

	got, err := s.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem error: %v", err)
	}
	if got == nil || got.CodeContext == nil {
		t.Fatal("expected code context on item read")
	}
	if got.CodeContext.Branch != "feat/link-pr" {
		t.Fatalf("expected branch feat/link-pr, got %q", got.CodeContext.Branch)
	}
	if got.CodeContext.PullRequest == nil || got.CodeContext.PullRequest.Number != 7 {
		t.Fatalf("expected PR #7, got %#v", got.CodeContext.PullRequest)
	}
}

func TestListItemsIncludesCodeContext(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	_, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Linked Item",
		Fields: `{"status":"open","github_pr":{"number":9,"url":"https://github.com/xarmian/pad/pull/9","title":"Linked Item","state":"MERGED","branch":"feat/linked-item","repo":"xarmian/pad","updated_at":"2026-04-02T14:10:00Z"}}`,
	})
	if err != nil {
		t.Fatalf("CreateItem error: %v", err)
	}

	items, err := s.ListItems(ws.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].CodeContext == nil {
		t.Fatal("expected list items to include code context")
	}
	if items[0].CodeContext.PullRequest == nil || items[0].CodeContext.PullRequest.State != "MERGED" {
		t.Fatalf("expected merged PR metadata, got %#v", items[0].CodeContext.PullRequest)
	}
}

func TestItemStructuredMetadataIsHydratedOnRead(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Capture reasoning",
		Fields: `{"status":"open","implementation_notes":[{"id":"note-1","summary":"Added typed metadata","details":"Surface it on item responses","created_at":"2026-04-02T16:00:00Z","created_by":"agent"}],"decision_log":[{"id":"decision-1","decision":"Store notes in reserved field keys","rationale":"Avoid a new table for this first cut","created_at":"2026-04-02T16:05:00Z","created_by":"agent"}]}`,
	})
	if err != nil {
		t.Fatalf("CreateItem error: %v", err)
	}

	got, err := s.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem error: %v", err)
	}
	if len(got.ImplementationNotes) != 1 {
		t.Fatalf("expected 1 implementation note, got %#v", got.ImplementationNotes)
	}
	if got.ImplementationNotes[0].Summary != "Added typed metadata" {
		t.Fatalf("expected implementation note summary, got %q", got.ImplementationNotes[0].Summary)
	}
	if len(got.DecisionLog) != 1 {
		t.Fatalf("expected 1 decision log entry, got %#v", got.DecisionLog)
	}
	if got.DecisionLog[0].Decision != "Store notes in reserved field keys" {
		t.Fatalf("expected decision log entry, got %q", got.DecisionLog[0].Decision)
	}
}

func TestConventionMetadataIsHydratedOnRead(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Conventions")

	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Run tests before completion",
		Fields: `{"status":"active","convention":{"category":"quality","trigger":"on-task-complete","surfaces":["all"],"enforcement":"must","commands":["go test ./...","make install"]}}`,
	})
	if err != nil {
		t.Fatalf("CreateItem error: %v", err)
	}

	got, err := s.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem error: %v", err)
	}
	if got.Convention == nil {
		t.Fatal("expected convention metadata on item read")
	}
	if got.Convention.Category != "quality" {
		t.Fatalf("expected category quality, got %q", got.Convention.Category)
	}
	if len(got.Convention.Commands) != 2 || got.Convention.Commands[0] != "go test ./..." {
		t.Fatalf("expected commands to be hydrated, got %#v", got.Convention.Commands)
	}
}

func TestItemListByCollection(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	tasks := createTestCollection(t, s, ws.ID, "Tasks")
	ideas := createTestCollection(t, s, ws.ID, "Ideas")

	createTestItem(t, s, ws.ID, tasks.ID, "Task 1", "")
	createTestItem(t, s, ws.ID, tasks.ID, "Task 2", "")
	createTestItem(t, s, ws.ID, ideas.ID, "Idea 1", "")

	// Filter by collection
	items, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "tasks"})
	if err != nil {
		t.Fatalf("ListItems error: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(items))
	}

	items, err = s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "ideas"})
	if err != nil {
		t.Fatalf("ListItems error: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 idea, got %d", len(items))
	}
}

func TestItemListByFieldFilter(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Open Task",
		Fields: `{"status":"open"}`,
	})
	s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Done Task",
		Fields: `{"status":"done"}`,
	})
	s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Another Open",
		Fields: `{"status":"open"}`,
	})

	// Filter: status=open
	items, err := s.ListItems(ws.ID, models.ItemListParams{
		Fields: map[string]string{"status": "open"},
	})
	if err != nil {
		t.Fatalf("ListItems with field filter error: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("expected 2 open items, got %d", len(items))
	}

	// Filter: status=done
	items, err = s.ListItems(ws.ID, models.ItemListParams{
		Fields: map[string]string{"status": "done"},
	})
	if err != nil {
		t.Fatalf("ListItems with field filter error: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 done item, got %d", len(items))
	}
}

func TestItemListByTag(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Tagged",
		Tags:  `["bug","urgent"]`,
	})
	s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Not Tagged",
		Tags:  `["feature"]`,
	})

	items, err := s.ListItems(ws.ID, models.ItemListParams{Tag: "bug"})
	if err != nil {
		t.Fatalf("ListItems tag filter error: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 tagged item, got %d", len(items))
	}
}

func TestItemListPagination(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	for i := 0; i < 5; i++ {
		s.CreateItem(ws.ID, col.ID, models.ItemCreate{
			Title: fmt.Sprintf("Task %d", i),
		})
	}

	// First page
	items, err := s.ListItems(ws.ID, models.ItemListParams{Limit: 2})
	if err != nil {
		t.Fatalf("ListItems error: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("expected 2 items on first page, got %d", len(items))
	}

	// Second page
	items, err = s.ListItems(ws.ID, models.ItemListParams{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("ListItems error: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("expected 2 items on second page, got %d", len(items))
	}
}

func TestItemFTSSearch(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	createTestItem(t, s, ws.ID, col.ID, "Fix auth bug", "OAuth2 authentication is broken")
	createTestItem(t, s, ws.ID, col.ID, "Add pagination", "Implement cursor-based pagination")

	// Search by content
	results, err := s.SearchItems(ws.ID, "authentication")
	if err != nil {
		t.Fatalf("SearchItems error: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 search result, got %d", len(results))
	}
	if len(results) > 0 && results[0].Item.Title != "Fix auth bug" {
		t.Errorf("expected 'Fix auth bug', got %q", results[0].Item.Title)
	}

	// Search by title
	results, err = s.SearchItems(ws.ID, "pagination")
	if err != nil {
		t.Fatalf("SearchItems error: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 search result, got %d", len(results))
	}
}

func TestItemFTSSearchViaListItems(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	createTestItem(t, s, ws.ID, col.ID, "Fix auth bug", "OAuth2 authentication is broken")
	createTestItem(t, s, ws.ID, col.ID, "Add pagination", "Implement cursor-based pagination")

	items, err := s.ListItems(ws.ID, models.ItemListParams{Search: "authentication"})
	if err != nil {
		t.Fatalf("ListItems search error: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 item, got %d", len(items))
	}
}

func TestItemSlugUniqueness(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item1, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "My Task"})
	if err != nil {
		t.Fatalf("CreateItem error: %v", err)
	}
	item2, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "My Task"})
	if err != nil {
		t.Fatalf("CreateItem error: %v", err)
	}

	if item1.Slug == item2.Slug {
		t.Error("duplicate slugs should not be allowed")
	}
	if item2.Slug != "my-task-2" {
		t.Errorf("expected slug 'my-task-2', got %q", item2.Slug)
	}
}

func TestCollectionSlugUniqueness(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	col1, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks"})
	if err != nil {
		t.Fatalf("CreateCollection error: %v", err)
	}
	col2, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks"})
	if err != nil {
		t.Fatalf("CreateCollection error: %v", err)
	}

	if col1.Slug == col2.Slug {
		t.Error("duplicate collection slugs should not be allowed")
	}
	if col2.Slug != "tasks-2" {
		t.Errorf("expected slug 'tasks-2', got %q", col2.Slug)
	}
}

// --- Item Link Tests ---

func TestItemLinks(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item1 := createTestItem(t, s, ws.ID, col.ID, "Task A", "")
	item2 := createTestItem(t, s, ws.ID, col.ID, "Task B", "")

	// Create link
	link, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{
		TargetID: item2.ID,
		LinkType: "blocks",
	}, item1.ID)
	if err != nil {
		t.Fatalf("CreateItemLink error: %v", err)
	}
	if link.SourceID != item1.ID {
		t.Errorf("expected source_id %q, got %q", item1.ID, link.SourceID)
	}
	if link.TargetID != item2.ID {
		t.Errorf("expected target_id %q, got %q", item2.ID, link.TargetID)
	}
	if link.LinkType != "blocks" {
		t.Errorf("expected link_type 'blocks', got %q", link.LinkType)
	}
	if link.SourceTitle != "Task A" {
		t.Errorf("expected source title 'Task A', got %q", link.SourceTitle)
	}
	if link.TargetTitle != "Task B" {
		t.Errorf("expected target title 'Task B', got %q", link.TargetTitle)
	}

	// Get links for item1
	links, err := s.GetItemLinks(item1.ID)
	if err != nil {
		t.Fatalf("GetItemLinks error: %v", err)
	}
	if len(links) != 1 {
		t.Errorf("expected 1 link, got %d", len(links))
	}

	// Get links for item2 (should appear as target)
	links, err = s.GetItemLinks(item2.ID)
	if err != nil {
		t.Fatalf("GetItemLinks error: %v", err)
	}
	if len(links) != 1 {
		t.Errorf("expected 1 link for target, got %d", len(links))
	}

	// Delete link
	err = s.DeleteItemLink(link.ID)
	if err != nil {
		t.Fatalf("DeleteItemLink error: %v", err)
	}

	links, _ = s.GetItemLinks(item1.ID)
	if len(links) != 0 {
		t.Error("deleted link still appears")
	}
}

func TestItemLinkDefaultType(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item1 := createTestItem(t, s, ws.ID, col.ID, "Task A", "")
	item2 := createTestItem(t, s, ws.ID, col.ID, "Task B", "")

	// Create link without explicit type
	link, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{
		TargetID: item2.ID,
	}, item1.ID)
	if err != nil {
		t.Fatalf("CreateItemLink error: %v", err)
	}
	if link.LinkType != "related" {
		t.Errorf("expected default link_type 'related', got %q", link.LinkType)
	}
}

func TestItemVersionCreation(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item := createTestItem(t, s, ws.ID, col.ID, "My Task", "Version 1")

	// First content update should create a version
	v2 := "Version 2"
	_, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &v2})
	if err != nil {
		t.Fatalf("UpdateItem error: %v", err)
	}

	// Versions are stored in item_versions
	versions, err := s.ListItemVersions(item.ID)
	if err != nil {
		t.Fatalf("ListItemVersions error: %v", err)
	}
	// Initial version from create + one from update
	if len(versions) < 1 {
		t.Errorf("expected at least 1 version, got %d", len(versions))
	}
}
