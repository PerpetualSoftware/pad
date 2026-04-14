package store

import (
	"database/sql"
	"testing"

	"github.com/xarmian/pad/internal/models"
)

func TestStarItem(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	user := createTestUser(t, s, "alice@test.com", "Alice", "password123")
	item := createTestItem(t, s, ws.ID, col.ID, "Fix bug", "")

	// Star an item
	if err := s.StarItem(user.ID, item.ID); err != nil {
		t.Fatalf("StarItem: %v", err)
	}

	// Verify it's starred
	starred, err := s.IsItemStarred(user.ID, item.ID)
	if err != nil {
		t.Fatalf("IsItemStarred: %v", err)
	}
	if !starred {
		t.Error("expected item to be starred")
	}

	// Re-starring is idempotent (no error)
	if err := s.StarItem(user.ID, item.ID); err != nil {
		t.Fatalf("StarItem (idempotent): %v", err)
	}
}

func TestUnstarItem(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	user := createTestUser(t, s, "alice@test.com", "Alice", "password123")
	item := createTestItem(t, s, ws.ID, col.ID, "Fix bug", "")

	// Star then unstar
	s.StarItem(user.ID, item.ID)

	if err := s.UnstarItem(user.ID, item.ID); err != nil {
		t.Fatalf("UnstarItem: %v", err)
	}

	starred, err := s.IsItemStarred(user.ID, item.ID)
	if err != nil {
		t.Fatalf("IsItemStarred: %v", err)
	}
	if starred {
		t.Error("expected item to not be starred after unstar")
	}

	// Unstarring a non-starred item returns sql.ErrNoRows
	err = s.UnstarItem(user.ID, item.ID)
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestIsItemStarred(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	user := createTestUser(t, s, "alice@test.com", "Alice", "password123")
	item := createTestItem(t, s, ws.ID, col.ID, "Fix bug", "")

	// Not starred initially
	starred, err := s.IsItemStarred(user.ID, item.ID)
	if err != nil {
		t.Fatalf("IsItemStarred: %v", err)
	}
	if starred {
		t.Error("expected item to not be starred initially")
	}

	// Star it
	s.StarItem(user.ID, item.ID)

	starred, err = s.IsItemStarred(user.ID, item.ID)
	if err != nil {
		t.Fatalf("IsItemStarred: %v", err)
	}
	if !starred {
		t.Error("expected item to be starred")
	}
}

func TestAreItemsStarred(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	user := createTestUser(t, s, "alice@test.com", "Alice", "password123")
	item1 := createTestItem(t, s, ws.ID, col.ID, "Task 1", "")
	item2 := createTestItem(t, s, ws.ID, col.ID, "Task 2", "")
	item3 := createTestItem(t, s, ws.ID, col.ID, "Task 3", "")

	// Star only item1 and item3
	s.StarItem(user.ID, item1.ID)
	s.StarItem(user.ID, item3.ID)

	result, err := s.AreItemsStarred(user.ID, []string{item1.ID, item2.ID, item3.ID})
	if err != nil {
		t.Fatalf("AreItemsStarred: %v", err)
	}

	if !result[item1.ID] {
		t.Error("expected item1 to be starred")
	}
	if result[item2.ID] {
		t.Error("expected item2 to not be starred")
	}
	if !result[item3.ID] {
		t.Error("expected item3 to be starred")
	}

	// Empty input returns empty map
	result, err = s.AreItemsStarred(user.ID, []string{})
	if err != nil {
		t.Fatalf("AreItemsStarred (empty): %v", err)
	}
	if len(result) != 0 {
		t.Error("expected empty result for empty input")
	}
}

func TestListStarredItems(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	user := createTestUser(t, s, "alice@test.com", "Alice", "password123")
	item1 := createTestItem(t, s, ws.ID, col.ID, "Task 1", "")
	item2 := createTestItem(t, s, ws.ID, col.ID, "Task 2", "")

	// Star both items
	s.StarItem(user.ID, item1.ID)
	s.StarItem(user.ID, item2.ID)

	items, err := s.ListStarredItems(user.ID, ws.ID, true)
	if err != nil {
		t.Fatalf("ListStarredItems: %v", err)
	}

	if len(items) != 2 {
		t.Fatalf("expected 2 starred items, got %d", len(items))
	}

	// Items should be enriched with collection info
	for _, item := range items {
		if item.CollectionSlug == "" {
			t.Error("expected collection slug to be populated")
		}
		if item.CollectionName == "" {
			t.Error("expected collection name to be populated")
		}
	}
}

func TestListStarredItemsExcludesTerminal(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	user := createTestUser(t, s, "alice@test.com", "Alice", "password123")
	item1 := createTestItem(t, s, ws.ID, col.ID, "Open task", "")
	item2 := createTestItem(t, s, ws.ID, col.ID, "Done task", "")

	// Mark item2 as done (terminal status)
	doneFields := `{"status":"done"}`
	s.UpdateItem(item2.ID, models.ItemUpdate{Fields: &doneFields})

	// Star both
	s.StarItem(user.ID, item1.ID)
	s.StarItem(user.ID, item2.ID)

	// includeTerminal=true returns both
	all, err := s.ListStarredItems(user.ID, ws.ID, true)
	if err != nil {
		t.Fatalf("ListStarredItems (all): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 items with includeTerminal=true, got %d", len(all))
	}

	// includeTerminal=false excludes the done item
	active, err := s.ListStarredItems(user.ID, ws.ID, false)
	if err != nil {
		t.Fatalf("ListStarredItems (active): %v", err)
	}
	if len(active) != 1 {
		t.Errorf("expected 1 item with includeTerminal=false, got %d", len(active))
	}
	if len(active) > 0 && active[0].Title != "Open task" {
		t.Errorf("expected 'Open task', got %q", active[0].Title)
	}
}

func TestStarsArePerUser(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	alice := createTestUser(t, s, "alice@test.com", "Alice", "password123")
	bob := createTestUser(t, s, "bob@test.com", "Bob", "password123")
	item := createTestItem(t, s, ws.ID, col.ID, "Shared task", "")

	// Alice stars it
	s.StarItem(alice.ID, item.ID)

	// Bob hasn't starred it
	starred, _ := s.IsItemStarred(bob.ID, item.ID)
	if starred {
		t.Error("expected item to not be starred for Bob")
	}

	// Alice sees it starred
	starred, _ = s.IsItemStarred(alice.ID, item.ID)
	if !starred {
		t.Error("expected item to be starred for Alice")
	}

	// Each user's starred list is independent
	aliceItems, _ := s.ListStarredItems(alice.ID, ws.ID, true)
	bobItems, _ := s.ListStarredItems(bob.ID, ws.ID, true)
	if len(aliceItems) != 1 {
		t.Errorf("expected 1 starred item for Alice, got %d", len(aliceItems))
	}
	if len(bobItems) != 0 {
		t.Errorf("expected 0 starred items for Bob, got %d", len(bobItems))
	}
}

func TestCountStarredItems(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	user := createTestUser(t, s, "alice@test.com", "Alice", "password123")

	// Count starts at 0
	count, err := s.CountStarredItems(user.ID, ws.ID)
	if err != nil {
		t.Fatalf("CountStarredItems: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}

	// Star 3 items
	for i := 0; i < 3; i++ {
		item := createTestItem(t, s, ws.ID, col.ID, "Task", "")
		s.StarItem(user.ID, item.ID)
	}

	count, err = s.CountStarredItems(user.ID, ws.ID)
	if err != nil {
		t.Fatalf("CountStarredItems: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3, got %d", count)
	}
}

func TestDeleteStarsForItem(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	alice := createTestUser(t, s, "alice@test.com", "Alice", "password123")
	bob := createTestUser(t, s, "bob@test.com", "Bob", "password123")
	item := createTestItem(t, s, ws.ID, col.ID, "Task", "")

	// Both users star the item
	s.StarItem(alice.ID, item.ID)
	s.StarItem(bob.ID, item.ID)

	// Delete all stars for the item
	if err := s.DeleteStarsForItem(item.ID); err != nil {
		t.Fatalf("DeleteStarsForItem: %v", err)
	}

	// Neither user should see it starred
	starred, _ := s.IsItemStarred(alice.ID, item.ID)
	if starred {
		t.Error("expected item to not be starred for Alice after bulk delete")
	}
	starred, _ = s.IsItemStarred(bob.ID, item.ID)
	if starred {
		t.Error("expected item to not be starred for Bob after bulk delete")
	}
}
