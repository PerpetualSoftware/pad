package store

import (
	"database/sql"
	"errors"
	"testing"
)

func TestCreateWatch_AndGet(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	user := createTestUser(t, s, "alice@test.com", "Alice", "password123")
	item := createTestItem(t, s, ws.ID, col.ID, "Fix bug", "")

	w, err := s.CreateWatch(ws.ID, user.ID, item.ID, "status=done")
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	if w.WorkspaceID != ws.ID || w.UserID != user.ID || w.ItemID != item.ID {
		t.Fatalf("unexpected watch: %+v", w)
	}
	if w.Predicate != "status=done" {
		t.Fatalf("expected predicate 'status=done', got %q", w.Predicate)
	}

	got, err := s.GetWatchByUserItem(user.ID, item.ID)
	if err != nil {
		t.Fatalf("GetWatchByUserItem: %v", err)
	}
	if got == nil || got.ID != w.ID {
		t.Fatalf("expected to find the created watch, got %+v", got)
	}
}

func TestCreateWatch_RepeatUpsertsPredicate(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	user := createTestUser(t, s, "alice@test.com", "Alice", "password123")
	item := createTestItem(t, s, ws.ID, col.ID, "Fix bug", "")

	first, err := s.CreateWatch(ws.ID, user.ID, item.ID, "")
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}

	second, err := s.CreateWatch(ws.ID, user.ID, item.ID, "status=done")
	if err != nil {
		t.Fatalf("CreateWatch (repeat): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected the same watch row (upsert), got a new ID: %q vs %q", first.ID, second.ID)
	}
	if second.Predicate != "status=done" {
		t.Fatalf("expected predicate to be replaced, got %q", second.Predicate)
	}

	all, err := s.ListWatchesForUser(user.ID)
	if err != nil {
		t.Fatalf("ListWatchesForUser: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected exactly 1 watch row (no duplicate), got %d", len(all))
	}
}

func TestGetWatchByUserItem_NotFound(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	user := createTestUser(t, s, "alice@test.com", "Alice", "password123")
	item := createTestItem(t, s, ws.ID, col.ID, "Fix bug", "")

	got, err := s.GetWatchByUserItem(user.ID, item.ID)
	if err != nil {
		t.Fatalf("GetWatchByUserItem: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestListWatchesForUser_EnrichedAndScopedToUser(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	alice := createTestUser(t, s, "alice@test.com", "Alice", "password123")
	bob := createTestUser(t, s, "bob@test.com", "Bob", "password123")
	item := createTestItem(t, s, ws.ID, col.ID, "Fix bug", "")

	if _, err := s.CreateWatch(ws.ID, alice.ID, item.ID, ""); err != nil {
		t.Fatalf("CreateWatch(alice): %v", err)
	}
	if _, err := s.CreateWatch(ws.ID, bob.ID, item.ID, ""); err != nil {
		t.Fatalf("CreateWatch(bob): %v", err)
	}

	aliceWatches, err := s.ListWatchesForUser(alice.ID)
	if err != nil {
		t.Fatalf("ListWatchesForUser(alice): %v", err)
	}
	if len(aliceWatches) != 1 {
		t.Fatalf("expected alice to see exactly her own watch, got %d", len(aliceWatches))
	}
	got := aliceWatches[0]
	if got.ItemTitle != "Fix bug" || got.ItemSlug != item.Slug || got.WorkspaceSlug != ws.Slug {
		t.Fatalf("expected enriched item/workspace fields, got %+v", got)
	}
	if got.ItemRef == "" {
		t.Fatalf("expected a computed item ref, got empty string")
	}
}

func TestListWatchesForUser_EmptyIsNotNil(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	user := createTestUser(t, s, "alice@test.com", "Alice", "password123")

	got, err := s.ListWatchesForUser(user.ID)
	if err != nil {
		t.Fatalf("ListWatchesForUser: %v", err)
	}
	if got == nil {
		t.Fatal("expected an empty (non-nil) slice")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 watches, got %d", len(got))
	}
}

func TestDeleteWatch(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	user := createTestUser(t, s, "alice@test.com", "Alice", "password123")
	item := createTestItem(t, s, ws.ID, col.ID, "Fix bug", "")

	if _, err := s.CreateWatch(ws.ID, user.ID, item.ID, ""); err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	if err := s.DeleteWatch(user.ID, item.ID); err != nil {
		t.Fatalf("DeleteWatch: %v", err)
	}

	got, err := s.GetWatchByUserItem(user.ID, item.ID)
	if err != nil {
		t.Fatalf("GetWatchByUserItem: %v", err)
	}
	if got != nil {
		t.Fatalf("expected watch to be gone, got %+v", got)
	}
}

func TestDeleteWatch_NotFoundReturnsErrNoRows(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	user := createTestUser(t, s, "alice@test.com", "Alice", "password123")

	err := s.DeleteWatch(user.ID, "nonexistent-item")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestWatch_CascadesOnItemDelete(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	user := createTestUser(t, s, "alice@test.com", "Alice", "password123")
	item := createTestItem(t, s, ws.ID, col.ID, "Fix bug", "")

	if _, err := s.CreateWatch(ws.ID, user.ID, item.ID, ""); err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}

	// Workspace purge hard-deletes items; watches must not survive as
	// dangling rows (see workspace_purge.go's explicit "watches" delete,
	// and the item_id ON DELETE CASCADE FK as a backstop for any future
	// single-item hard-delete path).
	if err := s.PurgeWorkspaceData(ws.ID); err == nil {
		t.Fatalf("expected PurgeWorkspaceData to refuse a live (non-soft-deleted) workspace")
	}
	if err := s.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	if err := s.PurgeWorkspaceData(ws.ID); err != nil {
		t.Fatalf("PurgeWorkspaceData: %v", err)
	}

	got, err := s.GetWatchByUserItem(user.ID, item.ID)
	if err != nil {
		t.Fatalf("GetWatchByUserItem: %v", err)
	}
	if got != nil {
		t.Fatalf("expected watch to be purged with its workspace, got %+v", got)
	}
}
