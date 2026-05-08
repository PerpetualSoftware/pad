package store

import (
	"bytes"
	"testing"
	"time"
)

// TestYjsUpdatesAppendAndLoad verifies AppendYjsUpdate returns monotonic
// IDs and LoadYjsUpdatesSince filters strictly above the cursor.
func TestYjsUpdatesAppendAndLoad(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Task A", "")

	id1, err := s.AppendYjsUpdate(item.ID, []byte{1, 2, 3}, "1")
	if err != nil {
		t.Fatalf("AppendYjsUpdate #1: %v", err)
	}
	if id1 == 0 {
		t.Fatalf("expected non-zero id, got %d", id1)
	}

	id2, err := s.AppendYjsUpdate(item.ID, []byte{4, 5, 6, 7}, "1")
	if err != nil {
		t.Fatalf("AppendYjsUpdate #2: %v", err)
	}
	if id2 <= id1 {
		t.Fatalf("expected id2 (%d) > id1 (%d)", id2, id1)
	}

	// sinceID = 0 returns everything in order.
	all, err := s.LoadYjsUpdatesSince(item.ID, 0)
	if err != nil {
		t.Fatalf("LoadYjsUpdatesSince(0): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 updates, got %d", len(all))
	}
	if all[0].ID != id1 || all[1].ID != id2 {
		t.Fatalf("ordering mismatch: ids=[%d,%d] want [%d,%d]", all[0].ID, all[1].ID, id1, id2)
	}
	if !bytes.Equal(all[0].UpdateData, []byte{1, 2, 3}) {
		t.Fatalf("payload mismatch on row 1: got %v", all[0].UpdateData)
	}
	if !bytes.Equal(all[1].UpdateData, []byte{4, 5, 6, 7}) {
		t.Fatalf("payload mismatch on row 2: got %v", all[1].UpdateData)
	}
	if all[0].SchemaVersion != "1" {
		t.Fatalf("schema version mismatch: got %q", all[0].SchemaVersion)
	}
	if all[0].CreatedAt.IsZero() {
		t.Fatalf("created_at should be parsed, got zero time")
	}

	// sinceID = id1 returns only the second row.
	since, err := s.LoadYjsUpdatesSince(item.ID, id1)
	if err != nil {
		t.Fatalf("LoadYjsUpdatesSince(id1): %v", err)
	}
	if len(since) != 1 || since[0].ID != id2 {
		t.Fatalf("want [%d] since id1, got %+v", id2, since)
	}

	// sinceID >= newest returns empty (caught up).
	caughtUp, err := s.LoadYjsUpdatesSince(item.ID, id2)
	if err != nil {
		t.Fatalf("LoadYjsUpdatesSince(id2): %v", err)
	}
	if len(caughtUp) != 0 {
		t.Fatalf("want 0 updates after newest, got %d", len(caughtUp))
	}
}

// TestYjsUpdatesValidation rejects empty itemID, empty data, or empty
// schemaVersion. We do this in Go rather than relying on the NOT NULL
// constraint so callers fail fast with a clear error message.
func TestYjsUpdatesValidation(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Task A", "")

	if _, err := s.AppendYjsUpdate("", []byte{1}, "1"); err == nil {
		t.Errorf("expected error for empty itemID")
	}
	if _, err := s.AppendYjsUpdate(item.ID, nil, "1"); err == nil {
		t.Errorf("expected error for nil data")
	}
	if _, err := s.AppendYjsUpdate(item.ID, []byte{}, "1"); err == nil {
		t.Errorf("expected error for empty data")
	}
	if _, err := s.AppendYjsUpdate(item.ID, []byte{1}, ""); err == nil {
		t.Errorf("expected error for empty schemaVersion")
	}
	if _, err := s.LoadYjsUpdatesSince("", 0); err == nil {
		t.Errorf("expected error for empty itemID on load")
	}
	if _, err := s.PruneYjsUpdatesBefore("", time.Now()); err == nil {
		t.Errorf("expected error for empty itemID on prune")
	}
}

// TestYjsUpdatesPrune removes only rows older than the cutoff for the
// given item, leaving newer rows + other items untouched.
func TestYjsUpdatesPrune(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Task A", "")
	other := createTestItem(t, s, ws.ID, col.ID, "Task B", "")

	// Append three rows for `item`. created_at is set to now() inside
	// AppendYjsUpdate, so they all share approximately the same timestamp.
	for i := 0; i < 3; i++ {
		if _, err := s.AppendYjsUpdate(item.ID, []byte{byte(i)}, "1"); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	// One row for `other` to verify we don't cross-prune.
	if _, err := s.AppendYjsUpdate(other.ID, []byte{99}, "1"); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	// Cutoff strictly in the future → all three of `item`'s rows should
	// match created_at < cutoff and be pruned.
	n, err := s.PruneYjsUpdatesBefore(item.ID, time.Now().Add(1*time.Hour))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 3 {
		t.Fatalf("want 3 pruned, got %d", n)
	}

	left, err := s.LoadYjsUpdatesSince(item.ID, 0)
	if err != nil {
		t.Fatalf("LoadYjsUpdatesSince after prune: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("want 0 rows after full prune, got %d", len(left))
	}

	// `other`'s row must be untouched.
	otherLeft, err := s.LoadYjsUpdatesSince(other.ID, 0)
	if err != nil {
		t.Fatalf("LoadYjsUpdatesSince other: %v", err)
	}
	if len(otherLeft) != 1 {
		t.Fatalf("cross-pruned other; want 1 row, got %d", len(otherLeft))
	}
}

// TestYjsUpdatesCascadeOnItemDelete confirms ON DELETE CASCADE wipes
// op-log rows when their parent item is deleted. Without this, item
// deletion would leave orphaned binary blobs on disk indefinitely.
func TestYjsUpdatesCascadeOnItemDelete(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Task A", "")

	if _, err := s.AppendYjsUpdate(item.ID, []byte{1, 2, 3}, "1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Sanity: row exists.
	pre, err := s.LoadYjsUpdatesSince(item.ID, 0)
	if err != nil {
		t.Fatalf("pre-delete load: %v", err)
	}
	if len(pre) != 1 {
		t.Fatalf("want 1 row before delete, got %d", len(pre))
	}

	// Delete the item. DeleteItem does a soft delete by default; we want
	// the FK cascade to fire, so we hard-delete via the underlying SQL.
	// (The op-log GC for soft-deleted items is a separate concern that
	// PLAN-1248's Phase 5 sweeper will handle.)
	if _, err := s.db.Exec(s.dialect.Rebind(`DELETE FROM items WHERE id = ?`), item.ID); err != nil {
		t.Fatalf("hard delete item: %v", err)
	}

	post, err := s.LoadYjsUpdatesSince(item.ID, 0)
	if err != nil {
		t.Fatalf("post-delete load: %v", err)
	}
	if len(post) != 0 {
		t.Fatalf("want 0 rows after cascade, got %d", len(post))
	}
}
