package store

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
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
	for _, expected := range []string{"tasks", "ideas", "plans", "docs", "conventions", "playbooks"} {
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

func TestSeedCollectionsFromTemplateAddsConventionsRoleField(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Template Test")

	if err := s.SeedCollectionsFromTemplate(ws.ID, "scrum"); err != nil {
		t.Fatalf("SeedCollectionsFromTemplate error: %v", err)
	}

	coll, err := s.GetCollectionBySlug(ws.ID, "conventions")
	if err != nil {
		t.Fatalf("GetCollectionBySlug error: %v", err)
	}
	if coll == nil {
		t.Fatal("expected conventions collection")
	}

	keys := schemaFieldKeys(t, coll.Schema)
	foundRole := false
	for _, key := range keys {
		if key == "role" {
			foundRole = true
			break
		}
	}
	if !foundRole {
		t.Fatalf("expected conventions schema to include role field, got %v", keys)
	}
}

// TestSeedCollectionsFromTemplateSeedsStarterPack verifies that the software
// templates' starter conventions + playbooks are materialized as items in
// the newly-created conventions/playbooks collections. This is what makes
// templates "batteries included" rather than empty shells.
func TestSeedCollectionsFromTemplateSeedsStarterPack(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Starter Pack")

	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed error: %v", err)
	}

	convColl, err := s.GetCollectionBySlug(ws.ID, "conventions")
	if err != nil || convColl == nil {
		t.Fatalf("conventions collection missing: %v", err)
	}
	convItems, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "conventions"})
	if err != nil {
		t.Fatalf("list conventions items: %v", err)
	}
	if len(convItems) == 0 {
		t.Errorf("expected starter conventions to be seeded, got 0 items")
	}

	playColl, err := s.GetCollectionBySlug(ws.ID, "playbooks")
	if err != nil || playColl == nil {
		t.Fatalf("playbooks collection missing: %v", err)
	}
	playItems, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "playbooks"})
	if err != nil {
		t.Fatalf("list playbooks items: %v", err)
	}
	if len(playItems) == 0 {
		t.Errorf("expected starter playbooks to be seeded, got 0 items")
	}
}

// TestSeedCollectionsFromTemplateHiring verifies end-to-end that the hiring
// template creates the right collections and seeds its starter pack.
func TestSeedCollectionsFromTemplateHiring(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Hiring")

	if err := s.SeedCollectionsFromTemplate(ws.ID, "hiring"); err != nil {
		t.Fatalf("seed hiring template: %v", err)
	}

	for _, slug := range []string{"requisitions", "candidates", "interview-loops", "feedback", "docs", "conventions", "playbooks"} {
		coll, err := s.GetCollectionBySlug(ws.ID, slug)
		if err != nil || coll == nil {
			t.Errorf("hiring workspace missing collection %q (err=%v)", slug, err)
		}
	}

	// Starter conventions land in the conventions collection.
	convs, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "conventions"})
	if err != nil {
		t.Fatalf("list conventions: %v", err)
	}
	if len(convs) == 0 {
		t.Error("expected hiring starter conventions to be seeded, got 0")
	}

	// Starter playbooks land in the playbooks collection.
	plays, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "playbooks"})
	if err != nil {
		t.Fatalf("list playbooks: %v", err)
	}
	if len(plays) == 0 {
		t.Error("expected hiring starter playbooks to be seeded, got 0")
	}

	// Seed items land in their named collections.
	reqs, _ := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "requisitions"})
	if len(reqs) == 0 {
		t.Error("expected hiring seed Requisition, got 0")
	}
	cands, _ := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "candidates"})
	if len(cands) == 0 {
		t.Error("expected hiring seed Candidate, got 0")
	}

	// Explicit prefixes land on the collections (issue IDs like REQ-1,
	// CAND-1 look nicer than REQUI-1 / CANDI-1 derived from the collection
	// name). Verifying here also catches any regression in the prefix
	// pipeline from template → CollectionCreate.
	for slug, want := range map[string]string{
		"requisitions":    "REQ",
		"candidates":      "CAND",
		"interview-loops": "LOOP",
		"feedback":        "FB",
	} {
		coll, err := s.GetCollectionBySlug(ws.ID, slug)
		if err != nil || coll == nil {
			continue
		}
		if coll.Prefix != want {
			t.Errorf("collection %q prefix = %q, want %q", slug, coll.Prefix, want)
		}
	}
}

// TestSeedCollectionsFromTemplateInterviewing verifies end-to-end that the
// interviewing template creates its collections and seeds the starter pack.
func TestSeedCollectionsFromTemplateInterviewing(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Interviewing")

	if err := s.SeedCollectionsFromTemplate(ws.ID, "interviewing"); err != nil {
		t.Fatalf("seed interviewing template: %v", err)
	}

	for _, slug := range []string{"applications", "interviews", "companies", "contacts", "docs", "conventions", "playbooks"} {
		coll, err := s.GetCollectionBySlug(ws.ID, slug)
		if err != nil || coll == nil {
			t.Errorf("interviewing workspace missing collection %q (err=%v)", slug, err)
		}
	}

	convs, _ := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "conventions"})
	if len(convs) == 0 {
		t.Error("expected interviewing starter conventions to be seeded, got 0")
	}
	plays, _ := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "playbooks"})
	if len(plays) == 0 {
		t.Error("expected interviewing starter playbooks to be seeded, got 0")
	}

	apps, _ := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "applications"})
	if len(apps) == 0 {
		t.Error("expected interviewing seed Application, got 0")
	}
	cos, _ := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "companies"})
	if len(cos) == 0 {
		t.Error("expected interviewing seed Company, got 0")
	}

	// Explicit prefixes
	for slug, want := range map[string]string{
		"applications": "APP",
		"interviews":   "INT",
		"companies":    "CO",
		"contacts":     "CON",
	} {
		coll, err := s.GetCollectionBySlug(ws.ID, slug)
		if err != nil || coll == nil {
			continue
		}
		if coll.Prefix != want {
			t.Errorf("collection %q prefix = %q, want %q", slug, coll.Prefix, want)
		}
	}
}

// TestSeedCollectionsFromTemplateRecoversPartialInit verifies that a retry
// after a partial seed (some items missing) fills in the missing items
// rather than treating the workspace as already-seeded. This guards the
// idempotency-by-title design — the freshlyCreated-only design trapped
// partially-initialized workspaces because the second pass saw existing
// collections and skipped all items.
func TestSeedCollectionsFromTemplateRecoversPartialInit(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Recover")

	// Simulate a partial init: seed the collections manually without any items.
	if err := s.SeedCollectionsFromTemplate(ws.ID, ""); err != nil {
		t.Fatalf("initial (no-template) seed error: %v", err)
	}
	// Collections exist but conventions collection has 0 items.
	convColl, _ := s.GetCollectionBySlug(ws.ID, "conventions")
	before, _ := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "conventions"})
	_ = convColl
	if len(before) != 0 {
		t.Fatalf("expected 0 conventions after empty-template seed, got %d", len(before))
	}

	// Retry with an explicit template — should fill in the starter pack
	// even though the collections already exist.
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("retry seed error: %v", err)
	}
	after, _ := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "conventions"})
	if len(after) == 0 {
		t.Errorf("expected starter conventions to be seeded on retry, got 0")
	}
}

// TestSeedCollectionsFromTemplateIdempotentWithSeedItems verifies that re-running
// the seed function across a pre-existing workspace does NOT duplicate seed items.
// This invariant is what lets the server's startup auto-upgrade safely iterate
// every workspace without creating duplicate convention/playbook items each boot.
func TestSeedCollectionsFromTemplateIdempotentWithSeedItems(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Idempotent")

	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("first seed error: %v", err)
	}

	before, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "conventions"})
	if err != nil {
		t.Fatalf("list conventions: %v", err)
	}
	initialCount := len(before)
	if initialCount == 0 {
		t.Fatalf("expected starter conventions after first seed")
	}

	// Re-seed — simulates the server's startup auto-upgrade running again.
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("second seed error: %v", err)
	}

	after, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "conventions"})
	if err != nil {
		t.Fatalf("list conventions after re-seed: %v", err)
	}
	if len(after) != initialCount {
		t.Errorf("re-seed duplicated conventions: before=%d, after=%d", initialCount, len(after))
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
		Fields: `{"status":"open","github_pr":{"number":7,"url":"https://github.com/PerpetualSoftware/pad/pull/7","title":"Link PR","state":"OPEN","branch":"feat/link-pr","repo":"PerpetualSoftware/pad","updated_at":"2026-04-02T14:00:00Z"}}`,
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
		Fields: `{"status":"open","github_pr":{"number":9,"url":"https://github.com/PerpetualSoftware/pad/pull/9","title":"Linked Item","state":"MERGED","branch":"feat/linked-item","repo":"PerpetualSoftware/pad","updated_at":"2026-04-02T14:10:00Z"}}`,
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

// TestItemLinks_HidesSoftDeletedEndpoints exercises BUG-734: when an item that
// is the source or target of a link gets soft-deleted, GetItemLinks should not
// surface the link from the surviving endpoint's perspective. Restoring the
// deleted item should resurrect the link automatically — the row is preserved
// on disk; only the query layer filters it.
func TestItemLinks_HidesSoftDeletedEndpoints(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	plan := createTestItem(t, s, ws.ID, col.ID, "Plan", "")
	implementer := createTestItem(t, s, ws.ID, col.ID, "Implementer task", "")

	// implementer --implements--> plan
	if _, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{
		TargetID: plan.ID,
		LinkType: "implements",
	}, implementer.ID); err != nil {
		t.Fatalf("CreateItemLink: %v", err)
	}

	// Sanity: link visible from both endpoints.
	if links, _ := s.GetItemLinks(plan.ID); len(links) != 1 {
		t.Fatalf("expected 1 link from plan side before delete, got %d", len(links))
	}
	if links, _ := s.GetItemLinks(implementer.ID); len(links) != 1 {
		t.Fatalf("expected 1 link from implementer side before delete, got %d", len(links))
	}

	// Soft-delete the implementer (the BUG-734 scenario: source side gone).
	if err := s.DeleteItem(implementer.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	// From the plan's perspective, the dangling implementer must not surface.
	links, err := s.GetItemLinks(plan.ID)
	if err != nil {
		t.Fatalf("GetItemLinks after delete: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("expected 0 links from plan side after implementer deleted, got %d (orphan leak — BUG-734)", len(links))
	}

	// Restore the implementer — the link row was never deleted, so the
	// relationship should reappear automatically.
	if _, err := s.RestoreItem(implementer.ID); err != nil {
		t.Fatalf("RestoreItem: %v", err)
	}
	links, err = s.GetItemLinks(plan.ID)
	if err != nil {
		t.Fatalf("GetItemLinks after restore: %v", err)
	}
	if len(links) != 1 {
		t.Errorf("expected 1 link from plan side after restore, got %d (link should be preserved across soft-delete/restore)", len(links))
	}

	// Now soft-delete the plan side instead (target side gone) and verify the
	// implementer's view also drops the dangling link.
	if err := s.DeleteItem(plan.ID); err != nil {
		t.Fatalf("DeleteItem plan: %v", err)
	}
	links, err = s.GetItemLinks(implementer.ID)
	if err != nil {
		t.Fatalf("GetItemLinks after target delete: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("expected 0 links from implementer side after plan deleted, got %d (target-side orphan leak)", len(links))
	}
}

// TestGetParentForItem_HidesSoftDeletedParent ensures lineage / breadcrumb
// queries don't surface a soft-deleted ancestor. See BUG-734.
func TestGetParentForItem_HidesSoftDeletedParent(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	parent := createTestItem(t, s, ws.ID, col.ID, "Parent", "")
	child := createTestItem(t, s, ws.ID, col.ID, "Child", "")

	if _, err := s.SetParentLink(ws.ID, child.ID, parent.ID, "user"); err != nil {
		t.Fatalf("SetParentLink: %v", err)
	}

	// Before delete: parent visible.
	if link, err := s.GetParentForItem(child.ID); err != nil {
		t.Fatalf("GetParentForItem: %v", err)
	} else if link == nil {
		t.Fatal("expected parent link before delete, got nil")
	}

	// Soft-delete parent.
	if err := s.DeleteItem(parent.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	// After delete: must read as no parent (don't render a deleted breadcrumb).
	link, err := s.GetParentForItem(child.ID)
	if err != nil {
		t.Fatalf("GetParentForItem after delete: %v", err)
	}
	if link != nil {
		t.Errorf("expected nil parent link after soft-delete, got %+v", link)
	}

	// After restore: parent visible again.
	if _, err := s.RestoreItem(parent.ID); err != nil {
		t.Fatalf("RestoreItem: %v", err)
	}
	if link, err := s.GetParentForItem(child.ID); err != nil {
		t.Fatalf("GetParentForItem after restore: %v", err)
	} else if link == nil {
		t.Error("expected parent link to reappear after restore")
	}
}

// TestGetParentMap_ExcludesSoftDeletedEndpoints covers the dashboard
// orphan-detection path: a task whose parent has been soft-deleted should
// NOT appear in GetParentMap, so handlers_dashboard.go correctly flags the
// task as orphaned. See BUG-734 / Codex review on PR #259.
func TestGetParentMap_ExcludesSoftDeletedEndpoints(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	parent := createTestItem(t, s, ws.ID, col.ID, "Parent", "")
	child := createTestItem(t, s, ws.ID, col.ID, "Child", "")
	if _, err := s.SetParentLink(ws.ID, child.ID, parent.ID, "user"); err != nil {
		t.Fatalf("SetParentLink: %v", err)
	}

	// Sanity: child→parent mapping present.
	m, err := s.GetParentMap(ws.ID)
	if err != nil {
		t.Fatalf("GetParentMap: %v", err)
	}
	if m[child.ID] != parent.ID {
		t.Fatalf("expected parent map %s→%s, got %s→%s", child.ID, parent.ID, child.ID, m[child.ID])
	}

	// Soft-delete the parent. The child must now look "parentless" so the
	// dashboard orphan detector flags it.
	if err := s.DeleteItem(parent.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	m, err = s.GetParentMap(ws.ID)
	if err != nil {
		t.Fatalf("GetParentMap after parent delete: %v", err)
	}
	if _, hasEntry := m[child.ID]; hasEntry {
		t.Errorf("expected child to drop from parent map after parent soft-deleted (orphan-detection regression)")
	}

	// Restoring the parent should bring the mapping back.
	if _, err := s.RestoreItem(parent.ID); err != nil {
		t.Fatalf("RestoreItem: %v", err)
	}
	m, err = s.GetParentMap(ws.ID)
	if err != nil {
		t.Fatalf("GetParentMap after parent restore: %v", err)
	}
	if m[child.ID] != parent.ID {
		t.Errorf("expected parent map to be restored to %s→%s, got %s→%s", child.ID, parent.ID, child.ID, m[child.ID])
	}

	// Soft-deleting the child side should also drop the entry.
	if err := s.DeleteItem(child.ID); err != nil {
		t.Fatalf("DeleteItem child: %v", err)
	}
	m, err = s.GetParentMap(ws.ID)
	if err != nil {
		t.Fatalf("GetParentMap after child delete: %v", err)
	}
	if _, hasEntry := m[child.ID]; hasEntry {
		t.Errorf("expected child to drop from parent map after the child itself was soft-deleted")
	}
}

// TestListItems_ParentFilter_FTS_RespectsSoftDeletedParent covers the
// `parent=<UUID>&search=<q>` combination. The search path routes through
// listItemsFTS, which the non-FTS parent filter doesn't touch; the FTS
// path needs to enforce the same deleted-parent rejection. See BUG-734 /
// Codex review on PR #259 (3rd pass).
func TestListItems_ParentFilter_FTS_RespectsSoftDeletedParent(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	parent := createTestItem(t, s, ws.ID, col.ID, "Parent", "")
	// Use a distinctive title so the FTS match is unambiguous.
	child := createTestItem(t, s, ws.ID, col.ID, "Distinctivekeyword child", "")
	if _, err := s.SetParentLink(ws.ID, child.ID, parent.ID, "user"); err != nil {
		t.Fatalf("SetParentLink: %v", err)
	}

	// Sanity: search + parent finds the child while parent is live.
	items, err := s.ListItems(ws.ID, models.ItemListParams{
		ParentLinkID: parent.ID,
		Search:       "Distinctivekeyword",
	})
	if err != nil {
		t.Fatalf("ListItems (FTS+parent): %v", err)
	}
	if len(items) != 1 || items[0].ID != child.ID {
		t.Fatalf("expected to find 1 child via FTS+parent before delete, got %d", len(items))
	}

	// Soft-delete the parent. The FTS path must also reject the now-deleted
	// parent, otherwise `?parent=<deleted-uuid>&search=foo` continues to leak
	// active children of an archived parent (the gap Codex flagged).
	if err := s.DeleteItem(parent.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	items, err = s.ListItems(ws.ID, models.ItemListParams{
		ParentLinkID: parent.ID,
		Search:       "Distinctivekeyword",
	})
	if err != nil {
		t.Fatalf("ListItems (FTS+parent) after delete: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 children via FTS+parent after parent soft-deleted, got %d (FTS-path parent-filter regression)", len(items))
	}

	// Restore brings the child back through the FTS+parent path.
	if _, err := s.RestoreItem(parent.ID); err != nil {
		t.Fatalf("RestoreItem: %v", err)
	}
	items, err = s.ListItems(ws.ID, models.ItemListParams{
		ParentLinkID: parent.ID,
		Search:       "Distinctivekeyword",
	})
	if err != nil {
		t.Fatalf("ListItems (FTS+parent) after restore: %v", err)
	}
	if len(items) != 1 || items[0].ID != child.ID {
		t.Errorf("expected child to reappear via FTS+parent after restoring parent, got %d", len(items))
	}
}

// TestListItems_ParentFilter_RespectsSoftDeletedParent ensures the
// `parent=<UUID>` query filter doesn't return children of a soft-deleted
// parent. Slug/ref filters already reject deleted parents upstream via
// GetItem/GetItemBySlug, but raw-UUID input bypasses that path. See
// BUG-734 / Codex review on PR #259.
func TestListItems_ParentFilter_RespectsSoftDeletedParent(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	parent := createTestItem(t, s, ws.ID, col.ID, "Parent", "")
	child := createTestItem(t, s, ws.ID, col.ID, "Child", "")
	if _, err := s.SetParentLink(ws.ID, child.ID, parent.ID, "user"); err != nil {
		t.Fatalf("SetParentLink: %v", err)
	}

	// Sanity: child is reachable via the parent filter.
	items, err := s.ListItems(ws.ID, models.ItemListParams{ParentLinkID: parent.ID})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 1 || items[0].ID != child.ID {
		t.Fatalf("expected to find 1 child via parent filter before delete, got %+v", items)
	}

	// Soft-delete the parent. Filter must now return no children — no
	// caller should be able to list children of a deleted parent by UUID.
	if err := s.DeleteItem(parent.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	items, err = s.ListItems(ws.ID, models.ItemListParams{ParentLinkID: parent.ID})
	if err != nil {
		t.Fatalf("ListItems after parent delete: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 children after parent soft-deleted, got %d (parent-filter regression)", len(items))
	}

	// Restoring the parent should bring the child back into the filter.
	if _, err := s.RestoreItem(parent.ID); err != nil {
		t.Fatalf("RestoreItem: %v", err)
	}
	items, err = s.ListItems(ws.ID, models.ItemListParams{ParentLinkID: parent.ID})
	if err != nil {
		t.Fatalf("ListItems after restore: %v", err)
	}
	if len(items) != 1 || items[0].ID != child.ID {
		t.Errorf("expected 1 child after restoring parent, got %d", len(items))
	}
}

// TestListItems_FTS_HyphenatedSearchTerm pins BUG-818 (FTS5 boolean parser
// regression on bare hyphens) AND BUG-842 (PG plainto_tsquery missing
// hyphenated-word matches). Each subtest indexes a distinct title and
// searches for a hyphenated form; both backends MUST return the match.
//
// The two cases exercise the two failure modes:
//   - `task-five` against `task-five-distinctive`: PG indexes the full
//     asciihword as `task-five-distinct`; a naive plainto_tsquery on the
//     query produces `task-fiv` (stemmed asciihword for the partial
//     query) which is NOT in the vector. Fixed by ORing in a
//     hyphen-as-space variant — see sanitizePGFTSQuery.
//   - `BUG-842` against `BUG-842 fix the cleanup race`: PG indexes the
//     `-842` token (negative number lexeme); a hyphen-as-space variant
//     would search for `842` and miss. The OR-combined query keeps the
//     raw form alive too, so `-842` matches.
func TestListItems_FTS_HyphenatedSearchTerm(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// Two distinctive titles, each exercising one tokenization mode.
	wantFive := createTestItem(t, s, ws.ID, col.ID, "task-five-distinctive", "")
	wantBug := createTestItem(t, s, ws.ID, col.ID, "BUG-842 fix the cleanup race", "")
	// Plain non-matching item.
	createTestItem(t, s, ws.ID, col.ID, "unrelated thing", "")

	cases := []struct {
		query string
		want  *models.Item
	}{
		{"task-five-distinctive", wantFive}, // full slug-ish
		{"task-five", wantFive},             // partial hyphenated — BUG-842 PG case
		{"BUG-842", wantBug},                // word-NUMBER hyphen — would regress under naive sanitize
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			items, err := s.ListItems(ws.ID, models.ItemListParams{Search: tc.query})
			if err != nil {
				t.Fatalf("ListItems(search=%q) errored (FTS5 boolean parser regression?): %v", tc.query, err)
			}
			if len(items) == 0 {
				t.Fatalf("ListItems(search=%q) returned 0 items, expected to find %s", tc.query, tc.want.Title)
			}
			found := false
			for _, it := range items {
				if it.ID == tc.want.ID {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("ListItems(search=%q) didn't include %s, got %d items", tc.query, tc.want.Title, len(items))
			}
		})
	}
}

// TestSearchItems_HyphenatedQuery is the BUG-818 regression test on the
// /api/v1/search code path, which routes through Store.SearchItems instead
// of Store.listItemsFTS.
func TestSearchItems_HyphenatedQuery(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	want := createTestItem(t, s, ws.ID, col.ID, "alpha-beta-gamma marker", "")

	results, err := s.SearchItems(ws.ID, "alpha-beta-gamma")
	if err != nil {
		t.Fatalf("SearchItems errored on hyphenated query: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("SearchItems returned 0 results, expected to find %s", want.Title)
	}
	found := false
	for _, r := range results {
		if r.Item.ID == want.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("SearchItems didn't include %s, got %d results", want.Title, len(results))
	}
}

// TestListDocuments_HyphenatedQuery covers BUG-818 on the documents FTS path.
// Same root cause as items: hyphenated queries hit FTS5's boolean parser and
// 500 unless sanitized. Surfaces /documents?q=task-5 and the web UI doc list.
func TestListDocuments_HyphenatedQuery(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	want, err := s.CreateDocument(ws.ID, models.DocumentCreate{
		Title:   "release-notes-q2",
		Content: "Quarterly release notes for the Q2 2026 milestone.",
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if _, err := s.CreateDocument(ws.ID, models.DocumentCreate{
		Title:   "unrelated topic",
		Content: "Nothing matching.",
	}); err != nil {
		t.Fatalf("CreateDocument unrelated: %v", err)
	}

	docs, err := s.ListDocuments(ws.ID, models.DocumentListParams{Query: "release-notes-q2"})
	if err != nil {
		t.Fatalf("ListDocuments(query=hyphenated) errored (FTS5 boolean parser regression?): %v", err)
	}
	if len(docs) == 0 {
		t.Fatalf("ListDocuments returned 0 docs, expected to find %s", want.Title)
	}
	found := false
	for _, d := range docs {
		if d.ID == want.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListDocuments didn't include %s, got %d docs", want.Title, len(docs))
	}
}

// TestStartupInvariants_AllFTSTriggersExist asserts every trigger in the
// canonical expectedFTSTriggers list is present after migrations run on a
// fresh DB. This is a forward-looking guard — any future migration that
// inadvertently breaks one of these will fail this test, before it can
// silently drift on production DBs the way BUG-822 did.
func TestStartupInvariants_AllFTSTriggersExist(t *testing.T) {
	s := testStore(t)

	if s.dialect.Driver() != DriverSQLite {
		t.Skip("FTS triggers are SQLite-specific; Postgres uses a different model")
	}

	for _, want := range expectedFTSTriggers {
		var name string
		err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='trigger' AND tbl_name=? AND name=?`,
			want.table, want.name,
		).Scan(&name)
		if err != nil {
			t.Errorf("expected trigger %q on table %q to exist after migrations, got error: %v",
				want.name, want.table, err)
		}
	}
}

// TestExpectedFTSTriggers_MatchesActual catches drift in the *opposite*
// direction from TestStartupInvariants_AllFTSTriggersExist: if a future
// migration adds a new trigger on items / comments / documents and the
// author forgets to add it to expectedFTSTriggers, the invariant check
// won't know to monitor it. This test compares the actual set of triggers
// on those tables against expectedFTSTriggers and fails if anything is
// missing from the list.
//
// If a new non-FTS trigger is legitimately added to one of these tables,
// either add it to expectedFTSTriggers (if it serves an FTS-like role) or
// extend the exclusion below.
func TestExpectedFTSTriggers_MatchesActual(t *testing.T) {
	s := testStore(t)

	if s.dialect.Driver() != DriverSQLite {
		t.Skip("FTS triggers are SQLite-specific")
	}

	expected := map[string]bool{}
	for _, e := range expectedFTSTriggers {
		expected[e.table+"/"+e.name] = true
	}

	rows, err := s.db.Query(`
		SELECT name, tbl_name FROM sqlite_master
		WHERE type='trigger' AND tbl_name IN ('items', 'comments', 'documents')
	`)
	if err != nil {
		t.Fatalf("query triggers: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name, table string
		if err := rows.Scan(&name, &table); err != nil {
			t.Fatalf("scan: %v", err)
		}
		key := table + "/" + name
		if !expected[key] {
			t.Errorf("found trigger %q on table %q that's not in expectedFTSTriggers — "+
				"update the list in store.go (or exclude in this test if it's not an FTS-style trigger)",
				name, table)
		}
	}
}

// TestStartupInvariants_LogsOnMissingTrigger verifies the alarm actually
// fires: drop a trigger, run validateFTSInvariants, capture the slog
// output, assert a warning was emitted naming the missing trigger.
//
// Without this test, the validator could regress (e.g. a typo in the
// SELECT, a dialect check that always returns early) and the BUG-822
// class of drift would go undetected again.
func TestStartupInvariants_LogsOnMissingTrigger(t *testing.T) {
	s := testStore(t)

	if s.dialect.Driver() != DriverSQLite {
		t.Skip("FTS trigger invariant check runs only on SQLite")
	}

	// Drop a known trigger to simulate the BUG-822 broken state.
	target := "documents_ai"
	if _, err := s.db.Exec("DROP TRIGGER " + target); err != nil {
		t.Fatalf("DROP TRIGGER %s: %v", target, err)
	}

	// Capture slog output via a custom handler that records records into
	// a slice we can inspect after.
	var captured []slog.Record
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(&recordCapturingHandler{records: &captured}))

	s.validateFTSInvariants()

	// Look for a warning record that mentions the trigger name we dropped.
	found := false
	for _, r := range captured {
		if r.Level != slog.LevelWarn {
			continue
		}
		mentioned := false
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "trigger" && a.Value.String() == target {
				mentioned = true
				return false
			}
			return true
		})
		if mentioned {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a slog.Warn record naming trigger=%q after dropping it, got %d records", target, len(captured))
		for _, r := range captured {
			t.Logf("  %s: %s", r.Level, r.Message)
		}
	}
}

// recordCapturingHandler is a minimal slog.Handler used by
// TestStartupInvariants_LogsOnMissingTrigger to capture records without
// emitting them to stderr. Not safe for concurrent use; tests are
// single-threaded.
type recordCapturingHandler struct {
	records *[]slog.Record
	attrs   []slog.Attr
}

func (h *recordCapturingHandler) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}
func (h *recordCapturingHandler) Handle(_ context.Context, r slog.Record) error {
	// slog.Record has internal shared state; clone before retaining so we
	// don't depend on the caller refraining from mutating it after Handle.
	r = r.Clone()
	for _, a := range h.attrs {
		r.AddAttrs(a)
	}
	*h.records = append(*h.records, r)
	return nil
}
func (h *recordCapturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &clone
}
func (h *recordCapturingHandler) WithGroup(_ string) slog.Handler {
	return h
}

// TestMigration046_DocumentsFTSTriggersExist verifies that after all
// migrations run on a fresh DB, the three documents_* triggers are present.
// This regression-protects BUG-822 — production DBs ended up missing these
// triggers (likely from a transient quirk during migration 025), and
// migration 046 restores them.
func TestMigration046_DocumentsFTSTriggersExist(t *testing.T) {
	s := testStore(t)

	// SQLite-only — Postgres uses a different tsvector trigger setup and
	// is unaffected.
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("documents_ai/au/ad triggers are SQLite-FTS5 specific")
	}

	for _, want := range []string{"documents_ai", "documents_au", "documents_ad"} {
		var name string
		err := s.db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='trigger' AND tbl_name='documents' AND name=?",
			want,
		).Scan(&name)
		if err != nil {
			t.Errorf("expected trigger %q to exist after migrations, got error: %v", want, err)
		}
	}
}

// TestMigration046_RebuildRecoversUnindexedDocs pins the *recovery* half of
// migration 046 — the part that rescues already-broken DBs. We simulate the
// production state Codex flagged: triggers were missing, so a document was
// inserted into the documents table but never made it into documents_fts.
// Migration 046's `INSERT INTO documents_fts(documents_fts) VALUES('rebuild')`
// step is what makes such a document searchable again. Without this test,
// removing the rebuild step would not break either of the other two BUG-822
// tests — flagged in Codex review.
func TestMigration046_RebuildRecoversUnindexedDocs(t *testing.T) {
	s := testStore(t)

	if s.dialect.Driver() != DriverSQLite {
		t.Skip("FTS5 rebuild idiom is SQLite-specific")
	}

	ws := createTestWorkspace(t, s, "Test")

	// Simulate the BUG-822 broken state: drop the FTS triggers so subsequent
	// inserts don't propagate into documents_fts.
	for _, name := range []string{"documents_ai", "documents_au", "documents_ad"} {
		if _, err := s.db.Exec("DROP TRIGGER IF EXISTS " + name); err != nil {
			t.Fatalf("DROP TRIGGER %s: %v", name, err)
		}
	}

	// Insert via the normal store path; trigger is gone so it won't reach FTS.
	doc, err := s.CreateDocument(ws.ID, models.DocumentCreate{
		Title: "Bug822recoverable distinctive",
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	// Pin the broken state — the doc must NOT be searchable yet, otherwise
	// the test isn't actually exercising the recovery path.
	docs, err := s.ListDocuments(ws.ID, models.DocumentListParams{Query: "Bug822recoverable"})
	if err != nil {
		t.Fatalf("ListDocuments (pre-rebuild): %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("test setup invalid: expected doc to be invisible to FTS pre-rebuild, got %d results", len(docs))
	}

	// Run just the rebuild step from migration 046 — the recovery action.
	if _, err := s.db.Exec(`INSERT INTO documents_fts(documents_fts) VALUES ('rebuild')`); err != nil {
		t.Fatalf("FTS rebuild: %v", err)
	}

	// Now the previously-unindexed doc must be findable.
	docs, err = s.ListDocuments(ws.ID, models.DocumentListParams{Query: "Bug822recoverable"})
	if err != nil {
		t.Fatalf("ListDocuments (post-rebuild): %v", err)
	}
	if len(docs) != 1 || docs[0].ID != doc.ID {
		t.Errorf("expected doc to become searchable post-rebuild, got %d results", len(docs))
	}
}

// TestCreateDocument_IsSearchableImmediately is the BUG-822 regression test:
// a freshly-created document must be findable via FTS without any manual
// rebuild. This was failing on production DBs whose documents_* triggers
// were missing — Store.CreateDocument inserted into documents but the
// after-insert trigger never fired to populate documents_fts, leaving the
// new doc invisible to ListDocuments(Query=...).
func TestCreateDocument_IsSearchableImmediately(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	doc, err := s.CreateDocument(ws.ID, models.DocumentCreate{
		Title:   "uniquesearchableword scratch",
		Content: "body",
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	docs, err := s.ListDocuments(ws.ID, models.DocumentListParams{Query: "uniquesearchableword"})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if len(docs) == 0 {
		t.Fatalf("expected to find newly-created doc by FTS, got 0 results (the BUG-822 regression)")
	}
	found := false
	for _, d := range docs {
		if d.ID == doc.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("FTS results don't include the new doc, got %d unrelated results", len(docs))
	}
}

// TestListDocuments_FTS_TagFilter pins BUG-820: when a search query is set,
// the FTS branch must still re-apply Tag filters (the documents analog of
// BUG-812). Before the fix, /documents?q=foo&tag=bar returned all docs
// matching "foo" regardless of tag.
func TestListDocuments_FTS_TagFilter(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	tagged, err := s.CreateDocument(ws.ID, models.DocumentCreate{
		Title: "FTSdocfilter alpha",
		Tags:  `["urgent"]`,
	})
	if err != nil {
		t.Fatalf("CreateDocument tagged: %v", err)
	}
	if _, err := s.CreateDocument(ws.ID, models.DocumentCreate{
		Title: "FTSdocfilter beta",
		Tags:  `[]`,
	}); err != nil {
		t.Fatalf("CreateDocument untagged: %v", err)
	}

	// Sanity: search alone returns both.
	docs, err := s.ListDocuments(ws.ID, models.DocumentListParams{Query: "FTSdocfilter"})
	if err != nil {
		t.Fatalf("ListDocuments sanity: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("sanity expected 2 docs via search, got %d", len(docs))
	}

	// Search + tag must narrow to the tagged doc only.
	docs, err = s.ListDocuments(ws.ID, models.DocumentListParams{Query: "FTSdocfilter", Tag: "urgent"})
	if err != nil {
		t.Fatalf("ListDocuments search+tag: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != tagged.ID {
		t.Errorf("expected exactly the tagged doc, got %d docs", len(docs))
	}
}

// TestListDocuments_FTS_PinnedFilter pins BUG-820 for the pinned filter:
// /documents?q=foo&pinned=true used to ignore the pin bit when the FTS
// branch took over. Documents analog of BUG-812.
func TestListDocuments_FTS_PinnedFilter(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	pinned, err := s.CreateDocument(ws.ID, models.DocumentCreate{
		Title:  "FTSpindoc alpha",
		Pinned: true,
	})
	if err != nil {
		t.Fatalf("CreateDocument pinned: %v", err)
	}
	if _, err := s.CreateDocument(ws.ID, models.DocumentCreate{
		Title:  "FTSpindoc beta",
		Pinned: false,
	}); err != nil {
		t.Fatalf("CreateDocument unpinned: %v", err)
	}

	pinTrue := true
	docs, err := s.ListDocuments(ws.ID, models.DocumentListParams{Query: "FTSpindoc", Pinned: &pinTrue})
	if err != nil {
		t.Fatalf("ListDocuments search+pinned=true: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != pinned.ID {
		t.Errorf("expected exactly the pinned doc via search+pinned=true, got %d docs", len(docs))
	}

	// And the inverse: pinned=false narrows to the other one.
	pinFalse := false
	docs, err = s.ListDocuments(ws.ID, models.DocumentListParams{Query: "FTSpindoc", Pinned: &pinFalse})
	if err != nil {
		t.Fatalf("ListDocuments search+pinned=false: %v", err)
	}
	if len(docs) != 1 || docs[0].Pinned {
		t.Errorf("expected exactly the unpinned doc via search+pinned=false, got %d docs", len(docs))
	}
}

// TestFTS_WhitespaceOnlyQuery_DoesNotCrash exercises the whitespace-only
// guard on each FTS entry point. sanitizeFTSQuery turns "   " into "" and
// SQLite FTS5 errors on a MATCH against an empty string, so the
// routing/guard has to short-circuit before binding.
// See BUG-818 / Codex follow-up.
func TestFTS_WhitespaceOnlyQuery_DoesNotCrash(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	createTestItem(t, s, ws.ID, col.ID, "anything", "")
	if _, err := s.CreateDocument(ws.ID, models.DocumentCreate{Title: "anything"}); err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	for _, q := range []string{"   ", "\t", "\n  \t"} {
		if _, err := s.ListItems(ws.ID, models.ItemListParams{Search: q}); err != nil {
			t.Errorf("ListItems(search=%q) errored: %v", q, err)
		}
		if _, err := s.SearchItems(ws.ID, q); err != nil {
			t.Errorf("SearchItems(%q) errored: %v", q, err)
		}
		if _, err := s.ListDocuments(ws.ID, models.DocumentListParams{Query: q}); err != nil {
			t.Errorf("ListDocuments(query=%q) errored: %v", q, err)
		}
	}
}

// TestSanitizePGFTSQuery is a unit test for the BUG-842 helper that
// produces the second leg of the OR-combined PG FTS query (raw +
// hyphen-as-space). See internal/store/search.go.
func TestSanitizePGFTSQuery(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain word", "hello", "hello"},
		{"hyphenated phrase", "task-five", "task five"},
		{"BUG-842 form", "BUG-842", "BUG 842"},
		{"multiple hyphens", "task-five-distinctive", "task five distinctive"},
		{"multi-token with hyphens", "task-five other-thing", "task five other thing"},
		{"no hyphens preserved", "foo bar baz", "foo bar baz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizePGFTSQuery(tc.in)
			if got != tc.want {
				t.Errorf("sanitizePGFTSQuery(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizeFTSQuery is a unit test for the helper that wraps each token in
// double quotes so SQLite FTS5 treats special characters as literals. See
// internal/store/search.go and BUG-818.
func TestSanitizeFTSQuery(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"plain word", "hello", `"hello"`},
		{"hyphenated phrase", "task-5", `"task-5"`},
		{"multiple tokens", "foo bar", `"foo" "bar"`},
		{"FTS5 boolean operator", "foo AND bar", `"foo" "AND" "bar"`},
		{"NOT operator", "foo NOT bar", `"foo" "NOT" "bar"`},
		{"parens", "(foo OR bar)", `"(foo" "OR" "bar)"`},
		{"embedded quotes are stripped", `"foo"bar`, `"foobar"`},
		{"surrounding whitespace trimmed", "  hello  ", `"hello"`},
		{"unicode token preserved", "café-au-lait", `"café-au-lait"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeFTSQuery(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeFTSQuery(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// --- BUG-812: listItemsFTS filter parity ---
//
// listItemsFTS used to silently drop most non-collection filters when the
// search param was set, so combining `?search=...&tag=...&assigned_user=...`
// (etc.) returned more items than the caller asked for. These tests pin the
// fix: each filter, applied alongside an FTS search query that matches both
// items, must narrow the result to only the matching item.

func TestListItems_FTS_TagFilter(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	tagged, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Searchhitkeyword tagged",
		Fields: `{"status":"open"}`,
		Tags:   `["urgent"]`,
	})
	if err != nil {
		t.Fatalf("CreateItem tagged: %v", err)
	}
	if _, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Searchhitkeyword untagged",
		Fields: `{"status":"open"}`,
		Tags:   `[]`,
	}); err != nil {
		t.Fatalf("CreateItem untagged: %v", err)
	}

	// Sanity: search alone returns both.
	items, err := s.ListItems(ws.ID, models.ItemListParams{Search: "Searchhitkeyword"})
	if err != nil {
		t.Fatalf("ListItems sanity: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("sanity expected 2 items via search, got %d", len(items))
	}

	// Search + tag must narrow to the tagged item only.
	items, err = s.ListItems(ws.ID, models.ItemListParams{Search: "Searchhitkeyword", Tag: "urgent"})
	if err != nil {
		t.Fatalf("ListItems search+tag: %v", err)
	}
	if len(items) != 1 || items[0].ID != tagged.ID {
		t.Errorf("expected exactly the tagged item, got %d items", len(items))
	}
}

func TestListItems_FTS_ParentIDFilter(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	parent := createTestItem(t, s, ws.ID, col.ID, "Parent item", "")
	parentID := parent.ID

	withParent, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:    "Searchhitkeyword child",
		Fields:   `{"status":"open"}`,
		ParentID: &parentID,
	})
	if err != nil {
		t.Fatalf("CreateItem with parent: %v", err)
	}
	if _, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Searchhitkeyword sibling-without-parent",
		Fields: `{"status":"open"}`,
	}); err != nil {
		t.Fatalf("CreateItem without parent: %v", err)
	}

	items, err := s.ListItems(ws.ID, models.ItemListParams{Search: "Searchhitkeyword", ParentID: parent.ID})
	if err != nil {
		t.Fatalf("ListItems search+parentID: %v", err)
	}
	if len(items) != 1 || items[0].ID != withParent.ID {
		t.Errorf("expected exactly the child of parent, got %d items", len(items))
	}
}

func TestListItems_FTS_AssignedUserFilter(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	user := createTestUser(t, s, "fts-assignee@test.com", "FTS Assignee", "password123")
	if err := s.AddWorkspaceMember(ws.ID, user.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	uid := user.ID

	assigned, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:          "Searchhitkeyword assigned",
		Fields:         `{"status":"open"}`,
		AssignedUserID: &uid,
	})
	if err != nil {
		t.Fatalf("CreateItem assigned: %v", err)
	}
	if _, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Searchhitkeyword unassigned",
		Fields: `{"status":"open"}`,
	}); err != nil {
		t.Fatalf("CreateItem unassigned: %v", err)
	}

	items, err := s.ListItems(ws.ID, models.ItemListParams{Search: "Searchhitkeyword", AssignedUserID: user.ID})
	if err != nil {
		t.Fatalf("ListItems search+assignee: %v", err)
	}
	if len(items) != 1 || items[0].ID != assigned.ID {
		t.Errorf("expected exactly the assigned item, got %d items", len(items))
	}
}

func TestListItems_FTS_AgentRoleFilter(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	role, err := s.CreateAgentRole(ws.ID, models.AgentRoleCreate{
		Name: "Implementer",
		Slug: "implementer",
	})
	if err != nil {
		t.Fatalf("CreateAgentRole: %v", err)
	}
	rid := role.ID

	withRole, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:       "Searchhitkeyword role-bearing",
		Fields:      `{"status":"open"}`,
		AgentRoleID: &rid,
	})
	if err != nil {
		t.Fatalf("CreateItem with role: %v", err)
	}
	if _, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Searchhitkeyword no-role",
		Fields: `{"status":"open"}`,
	}); err != nil {
		t.Fatalf("CreateItem without role: %v", err)
	}

	// Filter by role ID — exercises the i.agent_role_id = ? branch.
	items, err := s.ListItems(ws.ID, models.ItemListParams{Search: "Searchhitkeyword", AgentRoleID: role.ID})
	if err != nil {
		t.Fatalf("ListItems search+role-by-id: %v", err)
	}
	if len(items) != 1 || items[0].ID != withRole.ID {
		t.Errorf("expected the role-bearing item via role-ID filter, got %d items", len(items))
	}

	// Filter by role slug — exercises the OR ar.slug = ? branch.
	items, err = s.ListItems(ws.ID, models.ItemListParams{Search: "Searchhitkeyword", AgentRoleID: "implementer"})
	if err != nil {
		t.Fatalf("ListItems search+role-by-slug: %v", err)
	}
	if len(items) != 1 || items[0].ID != withRole.ID {
		t.Errorf("expected the role-bearing item via role-slug filter, got %d items", len(items))
	}
}

func TestListItems_FTS_FieldFilter(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	high, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Searchhitkeyword high-priority",
		Fields: `{"status":"open","priority":"high"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem high: %v", err)
	}
	medium, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Searchhitkeyword medium-priority",
		Fields: `{"status":"open","priority":"medium"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem medium: %v", err)
	}
	if _, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Searchhitkeyword low-priority",
		Fields: `{"status":"open","priority":"low"}`,
	}); err != nil {
		t.Fatalf("CreateItem low: %v", err)
	}

	// Single-value field filter — narrows to 1.
	items, err := s.ListItems(ws.ID, models.ItemListParams{
		Search: "Searchhitkeyword",
		Fields: map[string]string{"priority": "high"},
	})
	if err != nil {
		t.Fatalf("ListItems search+field=high: %v", err)
	}
	if len(items) != 1 || items[0].ID != high.ID {
		t.Errorf("expected exactly the high-priority item, got %d items", len(items))
	}

	// Comma-separated — narrows to 2 (high + medium).
	items, err = s.ListItems(ws.ID, models.ItemListParams{
		Search: "Searchhitkeyword",
		Fields: map[string]string{"priority": "high,medium"},
	})
	if err != nil {
		t.Fatalf("ListItems search+field=high,medium: %v", err)
	}
	gotIDs := map[string]bool{}
	for _, it := range items {
		gotIDs[it.ID] = true
	}
	if len(items) != 2 || !gotIDs[high.ID] || !gotIDs[medium.ID] {
		t.Errorf("expected high+medium via IN clause, got %d items: %+v", len(items), gotIDs)
	}

	// Invalid field key must be silently ignored (isValidFieldKey rejects),
	// not crash or return zero results. The non-FTS path has the same
	// guarantee.
	items, err = s.ListItems(ws.ID, models.ItemListParams{
		Search: "Searchhitkeyword",
		Fields: map[string]string{"bad key with spaces": "anything"},
	})
	if err != nil {
		t.Fatalf("ListItems search+invalid-field-key: %v", err)
	}
	// 3 because the invalid key is dropped — search alone returns all 3.
	if len(items) != 3 {
		t.Errorf("expected invalid field key to be ignored (3 search hits), got %d items", len(items))
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

// --- Workspace-Global Item Numbering Tests ---

func TestItemNumbersAreWorkspaceGlobal(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	tasks := createTestCollection(t, s, ws.ID, "Tasks")
	ideas := createTestCollection(t, s, ws.ID, "Ideas")

	// Create items across two collections — numbers should be globally sequential
	t1 := createTestItem(t, s, ws.ID, tasks.ID, "Task 1", "")
	i1 := createTestItem(t, s, ws.ID, ideas.ID, "Idea 1", "")
	t2 := createTestItem(t, s, ws.ID, tasks.ID, "Task 2", "")
	i2 := createTestItem(t, s, ws.ID, ideas.ID, "Idea 2", "")

	if *t1.ItemNumber != 1 {
		t.Errorf("expected Task 1 to be #1, got #%d", *t1.ItemNumber)
	}
	if *i1.ItemNumber != 2 {
		t.Errorf("expected Idea 1 to be #2, got #%d", *i1.ItemNumber)
	}
	if *t2.ItemNumber != 3 {
		t.Errorf("expected Task 2 to be #3, got #%d", *t2.ItemNumber)
	}
	if *i2.ItemNumber != 4 {
		t.Errorf("expected Idea 2 to be #4, got #%d", *i2.ItemNumber)
	}
}

func TestMoveItemPreservesNumber(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	tasks := createTestCollection(t, s, ws.ID, "Tasks")
	bugs := createTestCollection(t, s, ws.ID, "Bugs")

	item := createTestItem(t, s, ws.ID, tasks.ID, "Fix something", "")
	originalNumber := *item.ItemNumber

	// Move from Tasks to Bugs
	moved, err := s.MoveItem(item.ID, bugs.ID, `{"status":"open"}`)
	if err != nil {
		t.Fatalf("MoveItem error: %v", err)
	}

	if moved.CollectionID != bugs.ID {
		t.Error("item should be in bugs collection after move")
	}
	if *moved.ItemNumber != originalNumber {
		t.Errorf("item number should be preserved after move: expected %d, got %d", originalNumber, *moved.ItemNumber)
	}

	// Verify the ref changed prefix but kept the number
	if moved.Ref != fmt.Sprintf("%s-%d", bugs.Prefix, originalNumber) {
		t.Errorf("expected ref %s-%d, got %s", bugs.Prefix, originalNumber, moved.Ref)
	}
}

func TestOldRefResolvesAfterMove(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	plans := createTestCollection(t, s, ws.ID, "Plans")
	tasks := createTestCollection(t, s, ws.ID, "Tasks")

	item := createTestItem(t, s, ws.ID, plans.ID, "My Plan", "")
	originalNumber := *item.ItemNumber

	// Item is currently PLAN-1
	found, err := s.GetItemByRef(ws.ID, "PLAN", originalNumber)
	if err != nil {
		t.Fatalf("GetItemByRef error: %v", err)
	}
	if found == nil || found.ID != item.ID {
		t.Fatal("expected to find item by PLAN ref before move")
	}

	// Move to Tasks — becomes TASK-1
	moved, err := s.MoveItem(item.ID, tasks.ID, `{"status":"open"}`)
	if err != nil {
		t.Fatalf("MoveItem error: %v", err)
	}
	if moved.Ref != fmt.Sprintf("TASK-%d", originalNumber) {
		t.Fatalf("expected ref TASK-%d after move, got %s", originalNumber, moved.Ref)
	}

	// Old ref PLAN-1 should STILL resolve to the same item (fallback by number)
	found, err = s.GetItemByRef(ws.ID, "PLAN", originalNumber)
	if err != nil {
		t.Fatalf("GetItemByRef (old ref) error: %v", err)
	}
	if found == nil {
		t.Fatal("old ref PLAN-N should still resolve after move")
	}
	if found.ID != item.ID {
		t.Error("old ref resolved to wrong item")
	}

	// New ref TASK-1 should also work
	found, err = s.GetItemByRef(ws.ID, "TASK", originalNumber)
	if err != nil {
		t.Fatalf("GetItemByRef (new ref) error: %v", err)
	}
	if found == nil || found.ID != item.ID {
		t.Fatal("new ref TASK-N should resolve after move")
	}
}

func TestWorkspaceNumberingIsolation(t *testing.T) {
	s := testStore(t)
	ws1 := createTestWorkspace(t, s, "Workspace 1")
	ws2 := createTestWorkspace(t, s, "Workspace 2")
	col1 := createTestCollection(t, s, ws1.ID, "Tasks")
	col2 := createTestCollection(t, s, ws2.ID, "Tasks")

	// Each workspace has its own counter starting at 1
	item1 := createTestItem(t, s, ws1.ID, col1.ID, "WS1 Task", "")
	item2 := createTestItem(t, s, ws2.ID, col2.ID, "WS2 Task", "")

	if *item1.ItemNumber != 1 {
		t.Errorf("expected WS1 item to be #1, got #%d", *item1.ItemNumber)
	}
	if *item2.ItemNumber != 1 {
		t.Errorf("expected WS2 item to be #1, got #%d", *item2.ItemNumber)
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

func TestWorkspaceHasAgentActivity(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Connect Banner")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// Empty workspace — false.
	has, err := s.WorkspaceHasAgentActivity(ws.ID, nil, nil)
	if err != nil {
		t.Fatalf("WorkspaceHasAgentActivity: %v", err)
	}
	if has {
		t.Fatal("expected false on empty workspace")
	}

	// Non-agent items (web, skill) shouldn't flip it on.
	for _, src := range []string{"web", "skill"} {
		_, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
			Title:  fmt.Sprintf("From %s", src),
			Fields: `{"status":"open"}`,
			Source: src,
		})
		if err != nil {
			t.Fatalf("create %s item: %v", src, err)
		}
	}
	has, err = s.WorkspaceHasAgentActivity(ws.ID, nil, nil)
	if err != nil {
		t.Fatalf("WorkspaceHasAgentActivity (non-agent): %v", err)
	}
	if has {
		t.Fatal("expected false when only web/skill items exist")
	}

	// One CLI item — true.
	cliItem, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "From CLI",
		Fields: `{"status":"open"}`,
		Source: "cli",
	})
	if err != nil {
		t.Fatalf("create cli item: %v", err)
	}
	has, err = s.WorkspaceHasAgentActivity(ws.ID, nil, nil)
	if err != nil {
		t.Fatalf("WorkspaceHasAgentActivity (with cli): %v", err)
	}
	if !has {
		t.Fatal("expected true after a cli-sourced item exists")
	}

	// Soft-deleting the only CLI item should flip it back off.
	if err := s.DeleteItem(cliItem.ID); err != nil {
		t.Fatalf("delete cli item: %v", err)
	}
	has, err = s.WorkspaceHasAgentActivity(ws.ID, nil, nil)
	if err != nil {
		t.Fatalf("WorkspaceHasAgentActivity (after delete): %v", err)
	}
	if has {
		t.Fatal("expected false after the only cli item was deleted")
	}

	// An item with source='mcp' (reserved for future MCP-distinct
	// attribution — not currently emitted by any code path; today MCP
	// activity persists as source='cli' per dispatch_http_test.go) should
	// also flip the signal on, so the query stays correct if attribution
	// is later split.
	mcpItem, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "From MCP",
		Fields: `{"status":"open"}`,
		Source: "mcp",
	})
	if err != nil {
		t.Fatalf("create mcp item: %v", err)
	}
	has, err = s.WorkspaceHasAgentActivity(ws.ID, nil, nil)
	if err != nil {
		t.Fatalf("WorkspaceHasAgentActivity (with mcp): %v", err)
	}
	if !has {
		t.Fatal("expected true after a mcp-sourced item exists")
	}
	// Clean up so the workspace-isolation case below reflects only the
	// other-ws CLI item it inserts.
	if err := s.DeleteItem(mcpItem.ID); err != nil {
		t.Fatalf("delete mcp item: %v", err)
	}

	// Workspace isolation — a cli item in another workspace must not leak.
	otherWS := createTestWorkspace(t, s, "Other")
	otherCol := createTestCollection(t, s, otherWS.ID, "Tasks")
	if _, err := s.CreateItem(otherWS.ID, otherCol.ID, models.ItemCreate{
		Title:  "Other CLI",
		Fields: `{"status":"open"}`,
		Source: "cli",
	}); err != nil {
		t.Fatalf("create other-ws cli item: %v", err)
	}
	has, err = s.WorkspaceHasAgentActivity(ws.ID, nil, nil)
	if err != nil {
		t.Fatalf("WorkspaceHasAgentActivity (after other-ws cli): %v", err)
	}
	if has {
		t.Fatal("a cli item in a different workspace must not flip ours on")
	}
}

// TestWorkspaceHasAgentActivityVisibility covers the visibility filter
// so a guest with restricted access can't infer the existence of
// agent-sourced items in collections they don't have visibility into.
// Codex flagged this as a P2 leak during PR #284 review — without
// filtering, has_agent_activity reflected the whole workspace regardless
// of caller visibility.
func TestWorkspaceHasAgentActivityVisibility(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Visibility")
	visibleColl := createTestCollection(t, s, ws.ID, "Visible")
	hiddenColl := createTestCollection(t, s, ws.ID, "Hidden")

	// CLI item in the HIDDEN collection only.
	hiddenCLI, err := s.CreateItem(ws.ID, hiddenColl.ID, models.ItemCreate{
		Title:  "Hidden CLI",
		Fields: `{"status":"open"}`,
		Source: "cli",
	})
	if err != nil {
		t.Fatalf("create hidden cli item: %v", err)
	}

	// Web item in the VISIBLE collection (so the visible set isn't empty).
	if _, err := s.CreateItem(ws.ID, visibleColl.ID, models.ItemCreate{
		Title:  "Visible Web",
		Fields: `{"status":"open"}`,
		Source: "web",
	}); err != nil {
		t.Fatalf("create visible web item: %v", err)
	}

	// Unfiltered (full-visibility caller) sees the CLI item.
	has, err := s.WorkspaceHasAgentActivity(ws.ID, nil, nil)
	if err != nil {
		t.Fatalf("unfiltered: %v", err)
	}
	if !has {
		t.Fatal("unfiltered call must see the hidden CLI item")
	}

	// Caller with only the VISIBLE collection in scope must NOT see the
	// CLI item that lives in a hidden collection.
	has, err = s.WorkspaceHasAgentActivity(ws.ID, []string{visibleColl.ID}, nil)
	if err != nil {
		t.Fatalf("visible-coll only: %v", err)
	}
	if has {
		t.Fatal("must not surface CLI items in collections outside the caller's visible set")
	}

	// Item-level grant on the hidden CLI item should expose it via the
	// guest-item path even when the collection isn't in scope.
	has, err = s.WorkspaceHasAgentActivity(ws.ID, []string{visibleColl.ID}, []string{hiddenCLI.ID})
	if err != nil {
		t.Fatalf("with item grant: %v", err)
	}
	if !has {
		t.Fatal("an explicit item grant for the CLI item must surface it")
	}

	// Non-nil empty collectionIDs + no item grants = no visibility =
	// short-circuit false (matches ListItems' early-exit semantics).
	has, err = s.WorkspaceHasAgentActivity(ws.ID, []string{}, nil)
	if err != nil {
		t.Fatalf("empty visibility: %v", err)
	}
	if has {
		t.Fatal("an empty visibility set must short-circuit to false")
	}
}
