package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// migratedTestPair returns two INDEPENDENT store handles on one file, plus an
// item id to append against.
//
// Two handles rather than two connections from one pool, because the thing
// under test is a second PROCESS: the appender's busy_timeout belongs to its
// own connection, which is the measurement the whole mechanism rests on.
func migratedTestPair(t *testing.T) (a, b *Store, itemID string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "migrated.db")
	a, err := New(path)
	if err != nil {
		t.Fatalf("open handle A: %v", err)
	}
	t.Cleanup(func() { a.Close() })

	ws := createTestWorkspace(t, a, "Migrated")
	col := createTestCollection(t, a, ws.ID, "Tasks")
	item := createTestItem(t, a, ws.ID, col.ID, "Task A", "")

	b, err = New(path)
	if err != nil {
		t.Fatalf("open handle B: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return a, b, item.ID
}

type appendOutcome struct {
	err     error
	elapsed time.Duration
}

func appendAsync(b *Store, itemID string) <-chan appendOutcome {
	ch := make(chan appendOutcome, 1)
	go func() {
		start := time.Now()
		_, err := b.AppendYjsUpdate(itemID, []byte{1, 2, 3}, "1")
		ch <- appendOutcome{err: err, elapsed: time.Since(start)}
	}()
	return ch
}

// lockHold is how long the test holds the write lock while an append waits on
// it. It only has to be long enough for the appender to reach its blocked
// state; the mechanism under test is a schema change at COMMIT, not a duration.
const lockHold = 300 * time.Millisecond

// TestDeferredAppendSeesTriggerInstalledByTheTransactionItWaitedOn is proving
// test (a) for BUG-3072, and its name is the claim: an append that is ALREADY
// BLOCKED on the migration's write lock must be refused by a trigger that did
// not exist when that append started.
//
// WHY THIS IS THE ONLY EVIDENCE FOR THE DESIGN. The append is a pooled
// autocommit Exec (AppendYjsUpdate), so by the time the migration commits it is
// a prepared statement suspended mid-step inside SQLite. Whether it then sees
// the new trigger is a fact about SQLite's SQLITE_SCHEMA auto-reprepare, not
// about any code in this repo — and the entire BUG-3072 design is built on it
// being true. Nothing else in the suite would notice if a future SQLite, or a
// different driver build, stopped re-preparing: the migration would still
// "work", and the append would still silently land in an abandoned file, which
// is the exact defect this closes.
//
// THE TWO CONTROL LEGS ARE NOT DECORATION. Without CONTROL-UNBLOCKED the test
// cannot tell a refusal from a harness that never manages to append at all.
// Without CONTROL-NO-TRIGGER it cannot tell "the trigger refused it" from "the
// wait itself failed it" — that leg runs the identical block-and-commit
// sequence with no trigger and requires err == nil, which is the 3.04s-then-nil
// result BUG-3072 was filed on.
func TestDeferredAppendSeesTriggerInstalledByTheTransactionItWaitedOn(t *testing.T) {
	t.Run("CONTROL-UNBLOCKED: an append with no lock held succeeds", func(t *testing.T) {
		_, b, itemID := migratedTestPair(t)
		got := <-appendAsync(b, itemID)
		if got.err != nil {
			t.Fatalf("control leg must succeed, else a refusal below proves nothing: %v", got.err)
		}
	})

	t.Run("CONTROL-NO-TRIGGER: a deferred append succeeds when the tx commits nothing", func(t *testing.T) {
		a, b, itemID := migratedTestPair(t)
		tx, err := a.BeginSnapshot()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		ch := appendAsync(b, itemID)
		time.Sleep(lockHold)
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		got := <-ch
		if got.err != nil {
			t.Fatalf("a deferred append must SUCCEED when nothing refuses it — this is the "+
				"behaviour BUG-3072 was filed on, and if it errors here the test below is "+
				"measuring the wait, not the trigger: %v", got.err)
		}
		if got.elapsed < lockHold {
			t.Fatalf("append returned in %v, before the %v lock was released — it never "+
				"waited, so this leg is not exercising the deferral at all", got.elapsed, lockHold)
		}
	})

	t.Run("the deferred append is REFUSED once the marker commits", func(t *testing.T) {
		a, b, itemID := migratedTestPair(t)
		tx, err := a.BeginSnapshot()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		ch := appendAsync(b, itemID)
		time.Sleep(lockHold)
		if err := a.MarkMigratedTx(tx, "postgres://pg.example/pad"); err != nil {
			t.Fatalf("MarkMigratedTx: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		got := <-ch
		if got.err == nil {
			t.Fatal("the deferred append SUCCEEDED — it landed in a database that has been " +
				"abandoned, which is the whole of BUG-3072")
		}
		if !strings.Contains(got.err.Error(), MigratedMarker) {
			t.Fatalf("append was refused but not by the marker, so the refusal is not traceable "+
				"to the migration: %v", got.err)
		}
		if got.elapsed < lockHold {
			t.Fatalf("append returned in %v, before the %v lock was released — it was refused "+
				"without ever being deferred, so this does not exercise auto-reprepare", got.elapsed, lockHold)
		}
		if !strings.Contains(got.err.Error(), "postgres://pg.example/pad") {
			t.Fatalf("the refusal does not name where the data went: %v", got.err)
		}
	})
}

// TestMigratedDatabaseRefusesEveryWriteAndEveryOpen is GO condition (b), plus
// the breadth of the trigger population.
//
// TWO LEGS, AND THE SPLIT IS FORCED BY SQLITE, not by taste. A
// `BEFORE UPDATE/DELETE ... FOR EACH ROW` trigger fires per ROW, so on an
// EMPTY table an UPDATE or DELETE touches nothing, fires nothing, and returns
// success. The first version of this test asserted refusal for all three verbs
// on all 29 tables and reported 37 "ACCEPTED" failures — every one of them an
// empty table in the fixture, not a hole in the guard. Asserting a refusal that
// cannot occur measures the fixture, so:
//
//   - INSERT is checked on EVERY table. BEFORE INSERT fires before the row is
//     built, so a bare `INSERT INTO t (rowid) VALUES (...)` reaches the trigger
//     on a table this test cannot otherwise populate.
//   - UPDATE and DELETE are checked on every table that actually HAS a row,
//     with a floor on how many that must be and a requirement that the op-log
//     is among them — otherwise a fixture that stopped populating anything
//     would turn this leg green by doing nothing (the vacuous-assertion trap).
//   - Every table's three triggers are then checked STRUCTURALLY, so a table
//     the fixture cannot populate is still covered for all three verbs.
//
// The structural leg alone would not be evidence — a trigger can exist with a
// body that does nothing — but all of them are rendered by one function, and
// the behavioural leg proves what that function renders actually aborts.
func TestMigratedDatabaseRefusesEveryWriteAndEveryOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marked.db")
	s, err := New(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ws := createTestWorkspace(t, s, "Marked")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Task A", "")
	// Populate as much of the population as cheap helpers allow, so the
	// UPDATE/DELETE arms are exercised behaviourally on more than the three
	// tables a bare workspace creates. The named victim is first and is
	// separately required below: it is the table this whole mechanism exists
	// for, and covering it for INSERT only would be the sharpest possible
	// version of this test measuring the fixture.
	if _, err := s.AppendYjsUpdate(item.ID, []byte{1, 2, 3}, "1"); err != nil {
		t.Fatalf("seed op-log: %v", err)
	}
	other := createTestItem(t, s, ws.ID, col.ID, "Task B", "")
	if _, err := s.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Body: "seed"}); err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	if _, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: other.ID, LinkType: "blocks"}, item.ID); err != nil {
		t.Fatalf("seed item link: %v", err)
	}
	if _, err := s.CreateReminder(ws.ID, item.ID, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("seed reminder: %v", err)
	}
	createTestDoc(t, s, ws.ID, "Doc", "body")

	rowCount := func(table string) int {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	populated := map[string]bool{}
	for _, table := range MigratedRefusalTables() {
		populated[table] = rowCount(table) > 0
	}

	tx, err := s.BeginSnapshot()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := s.MarkMigratedTx(tx, "postgres://pg.example/pad"); err != nil {
		t.Fatalf("MarkMigratedTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	refused := func(table, stmt string) {
		t.Helper()
		_, err := s.db.Exec(stmt)
		if err == nil {
			t.Errorf("%s: %q was ACCEPTED on a migrated database", table, stmt)
			return
		}
		// A statement can also fail for an ordinary reason (a NOT NULL column
		// the bare INSERT does not supply). Only a refusal carrying the marker
		// proves the TRIGGER is what stopped it, which is why this asserts on
		// the marker rather than on "an error".
		if !strings.Contains(err.Error(), MigratedMarker) {
			t.Errorf("%s: %q failed, but not via the migration guard: %v", table, stmt, err)
		}
	}

	mutatedChecked := 0
	for _, table := range MigratedRefusalTables() {
		refused(table, `INSERT INTO `+table+` (rowid) VALUES (999999)`)
		if !populated[table] {
			continue
		}
		refused(table, `UPDATE `+table+` SET rowid = rowid`)
		refused(table, `DELETE FROM `+table)
		mutatedChecked++
	}
	// PRECONDITION, not decoration: without it this leg passes when the
	// fixture populates nothing at all.
	if mutatedChecked < 8 {
		t.Fatalf("only %d table(s) had rows, so the UPDATE/DELETE arms barely ran — "+
			"the fixture, not the guard, is what this measured", mutatedChecked)
	}
	if !populated["item_yjs_updates"] {
		t.Fatal("the op-log had no row, so the one table this whole mechanism exists for " +
			"was checked for INSERT only")
	}

	// Structural leg: every table, every verb, including the ones the fixture
	// cannot populate.
	for _, table := range MigratedRefusalTables() {
		for _, suffix := range []string{"ins", "upd", "del"} {
			name := migratedTriggerPrefix + table + "_" + suffix
			var body string
			if err := s.db.QueryRow(
				`SELECT COALESCE(sql, '') FROM sqlite_master WHERE type='trigger' AND name=?`,
				name).Scan(&body); err != nil {
				t.Errorf("trigger %s is missing: %v", name, err)
				continue
			}
			if !strings.Contains(body, "RAISE(ABORT") || !strings.Contains(body, MigratedMarker) {
				t.Errorf("trigger %s exists but does not abort with the marker: %s", name, body)
			}
		}
	}

	// The still-open handle keeps working for READS — the file is abandoned,
	// not corrupt, and an operator must be able to look at it.
	if _, err := s.LoadYjsUpdatesSince(item.ID, 0); err != nil {
		t.Errorf("reads must still work on a migrated database: %v", err)
	}
	s.Close()

	// (b): a FRESH open refuses, and says where the data went.
	reopened, err := New(path)
	if err == nil {
		reopened.Close()
		t.Fatal("New() opened a migrated database — every later write to it is lost")
	}
	for _, want := range []string{MigratedMarker, "postgres://pg.example/pad", path} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}

// TestRollbackLeavesTheSourceUntouched is GO conditions (c) and (d).
//
// WHAT IT ANSWERS, stated because it is narrower than the GO condition's
// wording (CONVE-30). It proves the MECHANISM: a MarkMigratedTx whose
// transaction is rolled back leaves no trigger, no marker row, and a fully
// writable database. It does NOT drive the cobra command, which would need a
// live PostgreSQL; the command's half of the guarantee is that every failure
// arm returns before the mark, with `committed` set on exactly one line
// immediately after Commit and a deferred Rollback otherwise.
func TestRollbackLeavesTheSourceUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rolledback.db")
	s, err := New(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ws := createTestWorkspace(t, s, "Rolled")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Task A", "")

	tx, err := s.BeginSnapshot()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := s.MarkMigratedTx(tx, "postgres://pg.example/pad"); err != nil {
		t.Fatalf("MarkMigratedTx: %v", err)
	}
	// The import failed after the mark — the one ordering in which a rollback
	// has something to undo.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// No marker.
	remedy, err := migratedRemedyIfMarked(s.db)
	if err != nil {
		t.Fatalf("marker check: %v", err)
	}
	if remedy != "" {
		t.Fatalf("a rolled-back migration left the source marked: %q", remedy)
	}

	// No triggers.
	var triggers int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name GLOB ?`,
		migratedTriggerPrefix+"*").Scan(&triggers); err != nil {
		t.Fatalf("count triggers: %v", err)
	}
	if triggers != 0 {
		t.Fatalf("a rolled-back migration left %d refusal trigger(s) behind", triggers)
	}

	// And the database still works — this is the promise the whole
	// rollback-rather-than-continue restructure exists to keep: a failed
	// migration leaves the operator a database to re-run from.
	if _, err := s.AppendYjsUpdate(item.ID, []byte{1, 2, 3}, "1"); err != nil {
		t.Fatalf("source must stay writable after a rolled-back migration: %v", err)
	}
	s.Close()

	reopened, err := New(path)
	if err != nil {
		t.Fatalf("source must still be openable after a rolled-back migration: %v", err)
	}
	reopened.Close()
}

// TestMigratedRefusalTablesAllExist keeps the population honest against the
// live schema.
//
// A name in that list that is not a table produces NO trigger and NO error —
// `CREATE TRIGGER ... ON nosuchtable` fails, so the migration would break; but
// a name DROPPED from the schema by a later migration would break it at the
// worst possible moment, mid-migration, on an operator's one-shot command. The
// instrument is sqlite_master rather than the migration text, because the
// latter carries `*_new` rebuild temporaries that are not tables in the end
// state.
func TestMigratedRefusalTablesAllExist(t *testing.T) {
	s := testStoreSQLite(t)
	for _, table := range MigratedRefusalTables() {
		var n int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
			t.Fatalf("look up %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("migratedRefusalTables names %q, which is not a table in the live schema", table)
		}
	}
}

// TestHeldSnapshotBlocksOtherWritersRatherThanLettingThemThrough pins the
// SQLite property the migration's consistency actually rests on.
//
// IT EXISTS BECAUSE I GOT THE REASON WRONG. The design was argued on "pooled
// reads take their own WAL snapshots, so a bundle's sections can disagree even
// under a held transaction" — which is true with NO transaction held and false
// for the migration, which holds BEGIN IMMEDIATE across its whole run. SQLite
// has one write lock, so while that is held nothing else can commit and a
// pooled read sees exactly what the transaction sees. Measuring that is what
// corrected the claim, so it is measured here rather than asserted in a
// comment nobody can falsify.
//
// What it therefore protects: if this ever goes green-with-a-writer-completing
// — the transaction stopped being IMMEDIATE, or stopped spanning the run — the
// bundle's consistency argument has changed underneath the code that relies on
// it, and the *Q threading becomes load-bearing rather than belt-and-braces.
func TestHeldSnapshotBlocksOtherWritersRatherThanLettingThemThrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.db")
	a, err := New(path)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	defer a.Close()
	ws := createTestWorkspace(t, a, "Snap")
	col := createTestCollection(t, a, ws.ID, "Tasks")
	createTestItem(t, a, ws.ID, col.ID, "first", "")

	b, err := New(path)
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	defer b.Close()

	count := func(q Queryer) int {
		t.Helper()
		var n int
		if err := q.QueryRow(`SELECT COUNT(*) FROM items WHERE workspace_id = ?`, ws.ID).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	tx, err := a.BeginSnapshot()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	before := count(tx)

	done := make(chan error, 1)
	go func() {
		_, err := b.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "second", Fields: `{"status":"open"}`})
		done <- err
	}()

	// Long relative to any single export query, so a writer that COULD get
	// through would have.
	time.Sleep(750 * time.Millisecond)

	select {
	case err := <-done:
		t.Fatalf("a second handle COMMITTED while the migration snapshot was held (err=%v) — "+
			"the bundle's consistency no longer follows from holding the write lock", err)
	default:
	}

	if got := count(tx); got != before {
		t.Errorf("the transaction's own view moved from %d to %d", before, got)
	}
	// The pool, deliberately: this is the leg that corrected the design's
	// stated reason. It must agree with the transaction.
	if got := count(a.db); got != before {
		t.Errorf("a POOLED read saw %d where the transaction sees %d — pooled reads CAN "+
			"diverge under a held snapshot after all, which makes the *Q threading "+
			"load-bearing rather than belt-and-braces", got, before)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the blocked writer must SUCCEED once the lock is released, or this test "+
			"is measuring a failed write rather than a deferred one: %v", err)
	}
	if got := count(a.db); got != before+1 {
		t.Errorf("after the commit the write should be visible: got %d, want %d", got, before+1)
	}
}
