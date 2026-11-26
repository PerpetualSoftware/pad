package store

import (
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3744: a browser snapshot was written whatever its op-log cursor, so a
// delayed flush from a tab that had seen fewer ops overwrote a body built
// from more. The GC watermark stayed where the newer flush put it, above the
// content that now stood.

func bug3744Item(t *testing.T, rows int) (*Store, string, []int64) {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-3744")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Item", "v0")
	ids := make([]int64, rows)
	for i := range ids {
		id, err := s.AppendYjsUpdate(item.ID, []byte{0x00, byte(i + 1)}, "1")
		if err != nil {
			t.Fatalf("AppendYjsUpdate %d: %v", i, err)
		}
		ids[i] = id
	}
	return s, item.ID, ids
}

func snapshot(s *Store, itemID, body string, cursor int64) error {
	_, err := s.UpdateItem(itemID, models.ItemUpdate{
		Content:        &body,
		LastModifiedBy: "user",
		VersionSource:  "collab-snapshot",
		OpLogCursor:    &cursor,
	})
	return err
}

func contentOf(t *testing.T, s *Store, itemID string) string {
	t.Helper()
	it, err := s.GetItem(itemID)
	if err != nil {
		t.Fatal(err)
	}
	return it.Content
}

// The audit's scenario: cursors 10 and 11, the 11 flush lands, then the
// delayed 10 arrives.
func TestBUG3744_DelayedOlderSnapshotIsRefused(t *testing.T) {
	t.Parallel()
	s, itemID, ids := bug3744Item(t, 2)
	if err := snapshot(s, itemID, "newer", ids[1]); err != nil {
		t.Fatalf("newer snapshot: %v", err)
	}
	err := snapshot(s, itemID, "older", ids[0])
	var behind *SnapshotBehindContentError
	if !errors.As(err, &behind) {
		t.Fatalf("older snapshot: err = %v, want SnapshotBehindContentError", err)
	}
	if got := contentOf(t, s, itemID); got != "newer" {
		t.Fatalf("content = %q, want the newer body kept", got)
	}
	wm, _, err := s.GetItemContentFlushedOpLogID(itemID)
	if err != nil {
		t.Fatal(err)
	}
	if wm != ids[1] {
		t.Fatalf("watermark = %d, want %d (the newer body's)", wm, ids[1])
	}
}

// The case the GC watermark cannot see: the newer flush is itself below MAX,
// so it never moves the watermark, and comparing against the watermark alone
// would still let the older body through.
func TestBUG3744_OlderSnapshotRefusedWhenNewerWasBelowMax(t *testing.T) {
	t.Parallel()
	s, itemID, ids := bug3744Item(t, 3)
	if err := snapshot(s, itemID, "middle", ids[1]); err != nil {
		t.Fatalf("middle snapshot: %v", err)
	}
	if wm, _, _ := s.GetItemContentFlushedOpLogID(itemID); wm != 0 {
		t.Fatalf("precondition: watermark = %d, want 0 (a below-MAX flush does not move it)", wm)
	}
	err := snapshot(s, itemID, "older", ids[0])
	var behind *SnapshotBehindContentError
	if !errors.As(err, &behind) {
		t.Fatalf("older snapshot: err = %v, want SnapshotBehindContentError", err)
	}
	if got := contentOf(t, s, itemID); got != "middle" {
		t.Fatalf("content = %q, want the middle body kept", got)
	}
}

// A server-driven full write covers every row there is, so a snapshot from
// before it is refused too.
func TestBUG3744_SnapshotOlderThanAServerWriteIsRefused(t *testing.T) {
	t.Parallel()
	s, itemID, ids := bug3744Item(t, 2)
	body := "server"
	if _, err := s.UpdateItem(itemID, models.ItemUpdate{Content: &body, LastModifiedBy: "user"}); err != nil {
		t.Fatalf("server write: %v", err)
	}
	err := snapshot(s, itemID, "older", ids[0])
	var behind *SnapshotBehindContentError
	if !errors.As(err, &behind) {
		t.Fatalf("snapshot behind a server write: err = %v, want SnapshotBehindContentError", err)
	}
	if got := contentOf(t, s, itemID); got != "server" {
		t.Fatalf("content = %q, want the server body kept", got)
	}
}

// Controls: the same cursor and a newer cursor are written.
func TestBUG3744_SameOrNewerCursorIsWritten(t *testing.T) {
	t.Parallel()
	s, itemID, ids := bug3744Item(t, 2)
	if err := snapshot(s, itemID, "a", ids[0]); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := snapshot(s, itemID, "b", ids[0]); err != nil {
		t.Fatalf("same cursor: %v", err)
	}
	if err := snapshot(s, itemID, "c", ids[1]); err != nil {
		t.Fatalf("newer cursor: %v", err)
	}
	if got := contentOf(t, s, itemID); got != "c" {
		t.Fatalf("content = %q, want c", got)
	}
}

// A snapshot without a cursor cannot show what it covers: refused once the
// stored body has recorded one, written as before when none has.
func TestBUG3744_CursorlessSnapshotRefusedOnceACursorIsRecorded(t *testing.T) {
	t.Parallel()
	s, itemID, ids := bug3744Item(t, 1)
	bare := func(body string) error {
		_, err := s.UpdateItem(itemID, models.ItemUpdate{
			Content:        &body,
			LastModifiedBy: "user",
			VersionSource:  "collab-snapshot",
		})
		return err
	}
	if err := bare("legacy"); err != nil {
		t.Fatalf("cursorless snapshot before any cursor: %v", err)
	}
	if err := snapshot(s, itemID, "tracked", ids[0]); err != nil {
		t.Fatalf("tracked snapshot: %v", err)
	}
	var behind *SnapshotBehindContentError
	if err := bare("stale"); !errors.As(err, &behind) {
		t.Fatalf("cursorless snapshot after a cursor: err = %v, want SnapshotBehindContentError", err)
	}
	if got := contentOf(t, s, itemID); got != "tracked" {
		t.Fatalf("content = %q, want tracked", got)
	}
}

func contentOpCursorOf(t *testing.T, s *Store, itemID string) int64 {
	t.Helper()
	var cursor int64
	if err := s.DB().QueryRow(s.q(`SELECT content_op_cursor FROM items WHERE id = ?`), itemID).Scan(&cursor); err != nil {
		t.Fatalf("read content_op_cursor: %v", err)
	}
	return cursor
}

// BUG-3558: a snapshot whose cursor exceeds MAX(item_yjs_updates.id) (or 0 on
// an empty op-log) is refused with SnapshotAheadOfOpLogError and leaves content
// and content_op_cursor untouched.
func TestBUG3558_SnapshotAheadOfOpLogIsRefused(t *testing.T) {
	t.Parallel()
	t.Run("non-empty op-log", func(t *testing.T) {
		t.Parallel()
		s, itemID, ids := bug3744Item(t, 2)
		err := snapshot(s, itemID, "forged", ids[1]+1000)
		var ahead *SnapshotAheadOfOpLogError
		if !errors.As(err, &ahead) {
			t.Fatalf("ahead snapshot: err = %v, want SnapshotAheadOfOpLogError", err)
		}
		if ahead.Cursor != ids[1]+1000 || ahead.Max != ids[1] {
			t.Fatalf("ahead = %+v, want Cursor=%d Max=%d", ahead, ids[1]+1000, ids[1])
		}
		if got := contentOf(t, s, itemID); got != "v0" {
			t.Fatalf("content = %q, want v0 unchanged", got)
		}
		if got := contentOpCursorOf(t, s, itemID); got != 0 {
			t.Fatalf("content_op_cursor = %d, want 0 unchanged", got)
		}
	})

	t.Run("empty op-log", func(t *testing.T) {
		t.Parallel()
		s, itemID, _ := bug3744Item(t, 0)
		err := snapshot(s, itemID, "forged", 1)
		var ahead *SnapshotAheadOfOpLogError
		if !errors.As(err, &ahead) {
			t.Fatalf("ahead snapshot on empty op-log: err = %v, want SnapshotAheadOfOpLogError", err)
		}
		if ahead.Cursor != 1 || ahead.Max != 0 {
			t.Fatalf("ahead = %+v, want Cursor=1 Max=0", ahead)
		}
		if got := contentOf(t, s, itemID); got != "v0" {
			t.Fatalf("content = %q, want v0 unchanged", got)
		}
		if got := contentOpCursorOf(t, s, itemID); got != 0 {
			t.Fatalf("content_op_cursor = %d, want 0 unchanged", got)
		}
	})
}

// BUG-3558: a snapshot whose cursor equals MAX(item_yjs_updates.id) (including
// 0 on an empty op-log) is accepted.
func TestBUG3558_SnapshotAtMaxOpLogIsAccepted(t *testing.T) {
	t.Parallel()
	t.Run("non-empty op-log", func(t *testing.T) {
		t.Parallel()
		s, itemID, ids := bug3744Item(t, 2)
		if err := snapshot(s, itemID, "at-max", ids[1]); err != nil {
			t.Fatalf("snapshot at MAX: %v", err)
		}
		if got := contentOf(t, s, itemID); got != "at-max" {
			t.Fatalf("content = %q, want at-max", got)
		}
		if got := contentOpCursorOf(t, s, itemID); got != ids[1] {
			t.Fatalf("content_op_cursor = %d, want %d", got, ids[1])
		}
	})

	t.Run("empty op-log", func(t *testing.T) {
		t.Parallel()
		s, itemID, _ := bug3744Item(t, 0)
		if err := snapshot(s, itemID, "at-zero", 0); err != nil {
			t.Fatalf("snapshot at 0 on empty op-log: %v", err)
		}
		if got := contentOf(t, s, itemID); got != "at-zero" {
			t.Fatalf("content = %q, want at-zero", got)
		}
		if got := contentOpCursorOf(t, s, itemID); got != 0 {
			t.Fatalf("content_op_cursor = %d, want 0", got)
		}
	})
}
