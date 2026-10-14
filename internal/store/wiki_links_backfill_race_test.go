package store

import (
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// These tests set package-level hooks, so they are not parallel. Parallel
// tests start only after every sequential one has finished, and the hooks
// ignore any store but their own besides.

// wikiRowCount counts the item_wiki_links rows whose source is itemID, deleted
// or not: the false row this file guards against is a raw row, whatever a
// visibility-filtered read would show.
func wikiRowCount(t *testing.T, s *Store, itemID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM item_wiki_links WHERE source_item_id = ?`), itemID).Scan(&n); err != nil {
		t.Fatalf("count wiki rows: %v", err)
	}
	return n
}

// setAfterScanHook installs fn for store s for the rest of the test.
func setAfterScanHook(t *testing.T, s *Store, fn func()) {
	t.Helper()
	wikiBackfillAfterScanHook = func(got *Store) {
		if got == s {
			fn()
		}
	}
	t.Cleanup(func() { wikiBackfillAfterScanHook = nil })
}

// backfillSourceUnindexed creates a target and a source linking to it, then
// clears the index so the backfill will pick the source up.
func backfillSourceUnindexed(t *testing.T, s *Store) (ws string, target, source *models.Item) {
	t.Helper()
	w := createTestWorkspace(t, s, "Backfill race")
	col := createTestCollection(t, s, w.ID, "Tasks")
	target = createTestItem(t, s, w.ID, col.ID, "Target", "")
	source = createTestItem(t, s, w.ID, col.ID, "Source", "Links to [["+refOf(target)+"]].")
	if _, err := s.db.Exec(s.q(`DELETE FROM item_wiki_links WHERE source_item_id = ?`), source.ID); err != nil {
		t.Fatalf("clear rows: %v", err)
	}
	return w.ID, target, source
}

// BUG-3666, the filed scenario: the link is removed after the scan read the
// item. The backfill used to rebuild the row from the body it had buffered,
// and since rows then existed, every later boot skipped the item.
func TestBackfillWikiLinks_LinkRemovedAfterScanIsNotResurrected(t *testing.T) {
	s := testStore(t)
	_, _, source := backfillSourceUnindexed(t, s)

	setAfterScanHook(t, s, func() {
		body := "No links any more."
		if _, err := s.UpdateItem(source.ID, models.ItemUpdate{Content: &body}); err != nil {
			t.Fatalf("UpdateItem: %v", err)
		}
	})

	res, err := s.BackfillWikiLinks()
	if err != nil {
		t.Fatalf("BackfillWikiLinks: %v", err)
	}
	if n := wikiRowCount(t, s, source.ID); n != 0 {
		t.Errorf("source has %d wiki rows after the link was removed; want 0 (rebuilt from content read before the update)", n)
	}
	if res.Errors != 0 {
		t.Errorf("Errors = %d, want 0", res.Errors)
	}
}

// Control for the test above: with no write in the window, the same setup
// does produce the row, so a zero there is not a backfill that does nothing.
func TestBackfillWikiLinks_UntouchedSourceIsIndexed(t *testing.T) {
	s := testStore(t)
	_, _, source := backfillSourceUnindexed(t, s)

	res, err := s.BackfillWikiLinks()
	if err != nil {
		t.Fatalf("BackfillWikiLinks: %v", err)
	}
	if n := wikiRowCount(t, s, source.ID); n != 1 {
		t.Errorf("source has %d wiki rows, want 1", n)
	}
	if res.ItemsIndexed < 1 || res.LinksInserted < 1 {
		t.Errorf("result = %+v, want the source counted as indexed", res)
	}
}

// The content emptied after the scan: the write cleared the rows, and the
// backfill must not put them back from the old body.
func TestBackfillWikiLinks_ContentEmptiedAfterScanStaysEmpty(t *testing.T) {
	s := testStore(t)
	_, _, source := backfillSourceUnindexed(t, s)

	setAfterScanHook(t, s, func() {
		empty := ""
		if _, err := s.UpdateItem(source.ID, models.ItemUpdate{Content: &empty}); err != nil {
			t.Fatalf("UpdateItem: %v", err)
		}
	})

	if _, err := s.BackfillWikiLinks(); err != nil {
		t.Fatalf("BackfillWikiLinks: %v", err)
	}
	if n := wikiRowCount(t, s, source.ID); n != 0 {
		t.Errorf("emptied source has %d wiki rows, want 0", n)
	}
}

// The source deleted after the scan gets no rows.
func TestBackfillWikiLinks_SourceDeletedAfterScanIsSkipped(t *testing.T) {
	s := testStore(t)
	_, _, source := backfillSourceUnindexed(t, s)

	setAfterScanHook(t, s, func() {
		if err := s.DeleteItem(source.ID); err != nil {
			t.Fatalf("DeleteItem: %v", err)
		}
	})

	res, err := s.BackfillWikiLinks()
	if err != nil {
		t.Fatalf("BackfillWikiLinks: %v", err)
	}
	if n := wikiRowCount(t, s, source.ID); n != 0 {
		t.Errorf("deleted source has %d wiki rows, want 0", n)
	}
	if res.Errors != 0 {
		t.Errorf("Errors = %d, want 0", res.Errors)
	}
}

// The re-read only helps if nothing can change the item between it and the
// replacement. Every content write takes the workspace seq lock, so the item's
// transaction must hold it from the re-read on. Probed from a second
// transaction at that point; the other workspace's key is the control that
// the probe can answer "free" at all.
func TestBackfillWikiLinks_HoldsWorkspaceLockFromTheReread(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("advisory locks are a Postgres construct; the helper is a no-op elsewhere")
	}
	ws, _, source := backfillSourceUnindexed(t, s)
	other := createTestWorkspace(t, s, "Other")

	probed := false
	wikiBackfillAfterReadHook = func(got *Store, _ *sql.Tx, _, itemID string) {
		if got != s || itemID != source.ID {
			return
		}
		probed = true
		ptx, err := s.db.Begin()
		if err != nil {
			t.Errorf("begin probe: %v", err)
			return
		}
		defer ptx.Rollback() //nolint:errcheck

		var free bool
		if err := ptx.QueryRow("SELECT pg_try_advisory_xact_lock(hashtext($1))", ws).Scan(&free); err != nil {
			t.Errorf("probe workspace key: %v", err)
			return
		}
		if free {
			t.Error("the workspace seq lock was free while the backfill held the re-read content: a content write can land before the replacement")
		}
		var otherFree bool
		if err := ptx.QueryRow("SELECT pg_try_advisory_xact_lock(hashtext($1))", other.ID).Scan(&otherFree); err != nil {
			t.Errorf("probe control key: %v", err)
			return
		}
		if !otherFree {
			t.Error("control: another workspace's key was taken, so the probe cannot tell held from free")
		}
	}
	t.Cleanup(func() { wikiBackfillAfterReadHook = nil })

	if _, err := s.BackfillWikiLinks(); err != nil {
		t.Fatalf("BackfillWikiLinks: %v", err)
	}
	if !probed {
		t.Fatal("the after-read hook never ran for the source, so nothing was probed")
	}
}

// End to end, for the interval the lock probe above covers: a content write
// started after the re-read must wait for the backfill to commit, and then
// its own reindex wins. If it could commit inside the window, the backfill
// would write rows from the content it re-read, which is stale by then.
// Postgres only: SQLite's single writer serialises this by construction.
func TestBackfillWikiLinks_WriteAfterTheRereadWaitsAndWins(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("needs a concurrent writer; SQLite serialises writers by construction")
	}
	_, _, source := backfillSourceUnindexed(t, s)

	const deadline = 10 * time.Second
	body := "No links any more."
	editDone := make(chan error, 1)
	var once sync.Once
	outcome := ""
	wikiBackfillAfterReadHook = func(got *Store, tx *sql.Tx, _, itemID string) {
		if got != s || itemID != source.ID {
			return
		}
		once.Do(func() {
			// "Blocked" means blocked BY THIS transaction, not any lock wait in
			// the database (codex r2): an unrelated waiter would otherwise read
			// as the edit waiting, and an edit that ran only later would pass.
			var backfillPID int
			if err := tx.QueryRow("SELECT pg_backend_pid()").Scan(&backfillPID); err != nil {
				t.Errorf("read the backfill's backend pid: %v", err)
				return
			}
			go func() {
				_, err := s.UpdateItem(source.ID, models.ItemUpdate{Content: &body})
				editDone <- err
			}()
			stop := time.After(deadline)
			for outcome == "" {
				select {
				case err := <-editDone:
					outcome = "committed"
					editDone <- err // hand it on to the check after the backfill
				case <-stop:
					t.Errorf("within %s the edit neither committed nor was seen blocked by the backfill: inconclusive", deadline)
					return
				case <-time.After(20 * time.Millisecond):
					var waiting int
					if err := s.db.QueryRow(`SELECT count(*) FROM pg_stat_activity
						WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid))`, backfillPID).Scan(&waiting); err != nil {
						t.Errorf("read pg_stat_activity: %v", err)
						return
					}
					if waiting > 0 {
						outcome = "blocked"
					}
				}
			}
		})
	}
	t.Cleanup(func() { wikiBackfillAfterReadHook = nil })

	if _, err := s.BackfillWikiLinks(); err != nil {
		t.Fatalf("BackfillWikiLinks: %v", err)
	}
	if outcome == "" {
		t.Fatal("the seam never reached an outcome (it did not fire, or the run was inconclusive), so this run exercised nothing")
	}
	select {
	case err := <-editDone:
		if err != nil {
			t.Fatalf("the concurrent edit failed, so this run never exercised the race: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the concurrent edit never finished after the backfill committed")
	}

	if outcome != "blocked" {
		t.Error("the concurrent edit COMMITTED between the backfill's re-read and its replacement: nothing serialised them")
	}
	if n := wikiRowCount(t, s, source.ID); n != 0 {
		t.Errorf("source has %d wiki rows after the edit removed its link; want 0", n)
	}
}
