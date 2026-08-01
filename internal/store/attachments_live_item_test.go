package store

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Store-level tests for CreateAttachmentForLiveItem — PLAN-2391 DR-14 /
// TASK-2402. The handler-level coverage lives in internal/server; these pin
// the invariant at the layer that actually enforces it.

// liveItemAttachmentFixture is a workspace + collection + live item, plus a
// ready-to-insert attachment row pointed at that item.
func liveItemAttachmentFixture(t *testing.T, s *Store) (wsID string, itemID string, row *models.Attachment) {
	t.Helper()
	ws := createTestWorkspace(t, s, "LiveItemAttachments")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Parent", "")
	return ws.ID, item.ID, &models.Attachment{
		WorkspaceID: ws.ID,
		ItemID:      &item.ID,
		UploadedBy:  "system",
		StorageKey:  "fs:" + newID(),
		ContentHash: newID(),
		MimeType:    "image/png",
		SizeBytes:   1,
		Filename:    "derived.png",
	}
}

func countLiveAttachments(t *testing.T, s *Store, wsID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(s.q(
		`SELECT COUNT(*) FROM attachments WHERE workspace_id = ? AND deleted_at IS NULL`,
	), wsID).Scan(&n); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	return n
}

func TestCreateAttachmentForLiveItem_LiveParentInserts(t *testing.T) {
	s := testStore(t)
	wsID, _, row := liveItemAttachmentFixture(t, s)

	if err := s.CreateAttachmentForLiveItem(row); err != nil {
		t.Fatalf("CreateAttachmentForLiveItem: %v", err)
	}
	if got, err := s.GetAttachment(row.ID); err != nil || got == nil {
		t.Fatalf("row not persisted (err = %v)", err)
	}
	if n := countLiveAttachments(t, s, wsID); n != 1 {
		t.Errorf("live attachments = %d, want 1", n)
	}
}

func TestCreateAttachmentForLiveItem_ArchivedParentRefuses(t *testing.T) {
	s := testStore(t)
	wsID, itemID, row := liveItemAttachmentFixture(t, s)

	if err := s.DeleteItem(itemID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	err := s.CreateAttachmentForLiveItem(row)
	if !errors.Is(err, ErrAttachmentParentItemGone) {
		t.Fatalf("err = %v, want ErrAttachmentParentItemGone", err)
	}
	if n := countLiveAttachments(t, s, wsID); n != 0 {
		t.Errorf("live attachments = %d, want 0 — the refusal still wrote a row", n)
	}
}

// A non-null item_id that names nothing at all is refused, not silently
// written as if it were an orphan.
func TestCreateAttachmentForLiveItem_MalformedParentRefuses(t *testing.T) {
	s := testStore(t)
	wsID, _, row := liveItemAttachmentFixture(t, s)

	ghost := newID()
	row.ItemID = &ghost
	if err := s.CreateAttachmentForLiveItem(row); !errors.Is(err, ErrAttachmentParentItemGone) {
		t.Fatalf("err = %v, want ErrAttachmentParentItemGone", err)
	}
	if n := countLiveAttachments(t, s, wsID); n != 0 {
		t.Errorf("live attachments = %d, want 0", n)
	}
}

// item_id carries no FK or same-workspace constraint, so a live item in
// ANOTHER workspace would pass a liveness-only re-check.
func TestCreateAttachmentForLiveItem_ForeignWorkspaceParentRefuses(t *testing.T) {
	s := testStore(t)
	wsID, _, row := liveItemAttachmentFixture(t, s)

	otherWS := createTestWorkspace(t, s, "Other")
	otherCol := createTestCollection(t, s, otherWS.ID, "Tasks")
	foreign := createTestItem(t, s, otherWS.ID, otherCol.ID, "Foreign", "")
	row.ItemID = &foreign.ID

	if err := s.CreateAttachmentForLiveItem(row); !errors.Is(err, ErrAttachmentParentItemGone) {
		t.Fatalf("err = %v, want ErrAttachmentParentItemGone", err)
	}
	if n := countLiveAttachments(t, s, wsID); n != 0 {
		t.Errorf("live attachments = %d, want 0", n)
	}
}

// An orphan row has no parent to pin and takes the plain insert path.
func TestCreateAttachmentForLiveItem_OrphanInserts(t *testing.T) {
	s := testStore(t)
	wsID, _, row := liveItemAttachmentFixture(t, s)
	row.ItemID = nil

	if err := s.CreateAttachmentForLiveItem(row); err != nil {
		t.Fatalf("orphan insert: %v", err)
	}
	if n := countLiveAttachments(t, s, wsID); n != 1 {
		t.Errorf("live attachments = %d, want 1", n)
	}
}

// waitForLockWait blocks until Postgres reports some backend running a
// statement containing needle AND waiting on a lock. It is the barrier that
// makes "the insert is blocked" an observation rather than an inference: a
// sleep cannot distinguish blocked-on-the-row from not-yet-scheduled or
// waiting-for-a-pool-connection, and both of those would pass while the lock
// was absent.
//
// done is watched at the same time. If the call under test completes while we
// are waiting for it to block, it never blocked at all — the lock is missing
// and the row was written.
func waitForLockWait(t *testing.T, s *Store, needle string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("the insert completed (err = %v) instead of blocking on the "+
				"item row held by an uncommitted archival — it read around the lock", err)
		default:
		}

		var waiting bool
		err := s.db.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid()
				  -- pg_stat_activity is CLUSTER-wide, and go test runs packages
				  -- concurrently against one server, so scope to this test's own
				  -- database or another package's backend could satisfy the
				  -- barrier.
				  AND datname = current_database()
				  AND state = 'active'
				  AND wait_event_type = 'Lock'
				  -- The needle is BOUND, not interpolated, so this poller's own
				  -- query text (which pg_stat_activity reports with the $1
				  -- placeholder intact) can never match itself.
				  AND query LIKE '%' || $1 || '%'
			)`, needle).Scan(&waiting)
		if err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend reported waiting on a lock for a statement containing %q "+
				"within the deadline", needle)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCreateAttachmentForLiveItem_BlocksOnArchivingWriter is the Postgres-only
// half: it proves the FOR NO KEY UPDATE row lock is what closes the window,
// not the point-in-time re-read.
//
// The interleaving is constructed, not sampled. An uncommitted transaction
// holds the item row after stamping deleted_at; the insert must BLOCK on that
// row rather than reading around it. Without the lock, read-committed hands
// the insert the pre-update snapshot immediately, it sees a live item, and it
// writes a quota-counted row against an item that is about to be archived.
// Once the archiving transaction commits, the re-read is re-evaluated, no
// longer matches `deleted_at IS NULL`, and the call fails closed.
//
// "Blocked" is verified IN THE DATABASE, not by elapsed time. waitForLockWait
// polls pg_stat_activity until the goroutine's statement is registered as
// waiting on a lock. A bare sleep would pass just as green if the goroutine
// were merely unscheduled or parked waiting for a pool connection — which is
// exactly the kind of timing-dependent assertion that rots silently.
//
// SQLite is excluded rather than skipped for convenience: its DSN sets
// _txlock=immediate, so the second writer cannot even open its transaction
// while the first is live — the interleaving this test constructs is
// unrepresentable there, which is why the locking clause is dialect-gated.
func TestCreateAttachmentForLiveItem_BlocksOnArchivingWriter(t *testing.T) {
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set — the lock only exists on Postgres")
	}
	s := testStorePostgres(t, pgURL)
	wsID, itemID, row := liveItemAttachmentFixture(t, s)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin archiving tx: %v", err)
	}
	defer tx.Rollback() // no-op after Commit
	ts := now()
	if _, err := tx.Exec(s.q(
		`UPDATE items SET deleted_at = ?, updated_at = ? WHERE id = ?`,
	), ts, ts, itemID); err != nil {
		t.Fatalf("archive item in tx: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- s.CreateAttachmentForLiveItem(row) }()

	// Wait until Postgres itself reports the statement as lock-blocked. If
	// it instead finishes, it read around the lock and the insert landed.
	waitForLockWait(t, s, "FOR NO KEY UPDATE", done)

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit archival: %v", err)
	}
	if err := <-done; !errors.Is(err, ErrAttachmentParentItemGone) {
		t.Fatalf("err = %v, want ErrAttachmentParentItemGone", err)
	}
	if n := countLiveAttachments(t, s, wsID); n != 0 {
		t.Errorf("live attachments = %d, want 0", n)
	}
}
