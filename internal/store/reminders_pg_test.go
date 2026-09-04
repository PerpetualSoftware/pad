package store

import (
	"os"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
)

// Postgres-only pins for the fire path's row pin (IDEA-2641, codex round 12).
//
// reminderFireable READS liveness; the pin HOLDS it. TestFirePathInvariant
// covers the interleaving where the archival commits before the fire — the
// predicate misses and nothing fires. These two cover the interleaving the
// predicate cannot: the archival is IN FLIGHT, uncommitted, when the fire
// begins. Without the pin the fire's predicate reads the pre-archival row
// (READ COMMITTED sees only committed state), the UPDATE and the outbox write
// land, and the archival commits a moment later — an event about a resource
// that no longer exists. With the pin the fire blocks on the archival's row
// lock, and once the archival commits, the re-evaluated re-read no longer
// matches and the fire returns without emitting.
//
// "Blocked" is verified in the database via pg_stat_activity (waitForLockWait),
// not by elapsed time — a bare sleep would pass just as green if the goroutine
// were merely unscheduled.
//
// SQLite is excluded rather than skipped for convenience: its DSN sets
// _txlock=immediate, so the fire cannot even open its transaction while the
// archival is live. The interleaving these tests construct is unrepresentable
// there, which is why the pin is dialect-gated.
//
// MUTANT: removing the `if s.dialect.Driver() == DriverPostgres` pin block
// makes both tests fail at waitForLockWait — the fire completes instead of
// blocking, and emits.

func firePathPGStore(t *testing.T) (*Store, string, string, string) {
	t.Helper()
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set — the row pin only exists on Postgres")
	}
	s := testStorePostgres(t, pgURL)
	ws := createTestWorkspace(t, s, "Pin")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Ship it", "")
	id := armReminder(t, s, ws.ID, item.ID, past)

	ids, err := s.dueReminderCandidates(nowTS(), 0)
	if err != nil {
		t.Fatalf("dueReminderCandidates: %v", err)
	}
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("setup: expected the armed reminder as the only candidate, got %v", ids)
	}
	return s, ws.ID, item.ID, id
}

func TestFireOneReminderBlocksOnAnArchivingWorkspace(t *testing.T) {
	s, wsID, _, id := firePathPGStore(t)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin archiving tx: %v", err)
	}
	defer tx.Rollback() // no-op after Commit
	ts := now()
	if _, err := tx.Exec(s.q(`UPDATE workspaces SET deleted_at = ?, updated_at = ? WHERE id = ?`), ts, ts, wsID); err != nil {
		t.Fatalf("archive workspace in tx: %v", err)
	}

	type fireResult struct {
		fired bool
		err   error
	}
	done := make(chan error, 1)
	results := make(chan fireResult, 1)
	go func() {
		r, err := s.fireOneReminder(id, nowTS())
		results <- fireResult{fired: r != nil, err: err}
		done <- err
	}()

	waitForLockWait(t, s, "FOR NO KEY UPDATE OF", done)

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit archival: %v", err)
	}
	res := <-results
	if res.err != nil {
		t.Fatalf("fireOneReminder: %v", res.err)
	}
	if res.fired {
		t.Error("fired a reminder whose workspace was archived by the writer it was blocked on")
	}
	assertNothingLeft(t, s, wsID, id)
}

func TestFireOneReminderBlocksOnAnArchivingItem(t *testing.T) {
	s, wsID, itemID, id := firePathPGStore(t)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin archiving tx: %v", err)
	}
	defer tx.Rollback() // no-op after Commit
	ts := now()
	if _, err := tx.Exec(s.q(`UPDATE items SET deleted_at = ?, updated_at = ? WHERE id = ?`), ts, ts, itemID); err != nil {
		t.Fatalf("archive item in tx: %v", err)
	}

	type fireResult struct {
		fired bool
		err   error
	}
	done := make(chan error, 1)
	results := make(chan fireResult, 1)
	go func() {
		r, err := s.fireOneReminder(id, nowTS())
		results <- fireResult{fired: r != nil, err: err}
		done <- err
	}()

	waitForLockWait(t, s, "FOR NO KEY UPDATE OF", done)

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit archival: %v", err)
	}
	res := <-results
	if res.err != nil {
		t.Fatalf("fireOneReminder: %v", res.err)
	}
	if res.fired {
		t.Error("fired a reminder whose item was archived by the writer it was blocked on")
	}
	assertNothingLeft(t, s, wsID, id)
}

// assertNothingLeft: no event left the process and the reminder is still
// armed — the same three assertions TestFirePathInvariant makes, so the pin
// and the predicate are held to one standard.
func assertNothingLeft(t *testing.T, s *Store, wsID, reminderID string) {
	t.Helper()
	var events int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM event_outbox WHERE workspace_id = ? AND event_type = ?`),
		wsID, kernelevents.ItemReminderDue).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Errorf("%d reminder event(s) left the process", events)
	}
	var firedAt *string
	if err := s.db.QueryRow(s.q(`SELECT fired_at FROM item_reminders WHERE id = ?`), reminderID).Scan(&firedAt); err != nil {
		t.Fatalf("read reminder: %v", err)
	}
	if firedAt != nil {
		t.Errorf("reminder carries fired_at = %q after a fire that must not have happened", *firedAt)
	}
}
