package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// IDEA-2883. item_reminders.workspace_id is denormalized from the item, and
// until migration 086 nothing but a repeated read-side predicate stopped the
// two from disagreeing. IDEA-2641 closed that class one read at a time across
// two codex rounds and five call sites; these tests lock the constraint that
// makes the disagreeing row unrepresentable instead.

// reminderConstraintFixture returns a workspace with one item, plus a SECOND
// workspace whose id is the wrong one to file that item's reminder under.
func reminderConstraintFixture(t *testing.T, s *Store) (item *models.Item, ownWorkspace, otherWorkspace string) {
	t.Helper()
	own := createTestWorkspace(t, s, "Reminder Constraint Own")
	other := createTestWorkspace(t, s, "Reminder Constraint Other")
	coll, err := s.CreateCollection(own.ID, models.CollectionCreate{Name: "Tasks", Prefix: "TSK"})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	it, err := s.CreateItem(own.ID, coll.ID, models.ItemCreate{Title: "Remind me"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return it, own.ID, other.ID
}

// TestReminderWorkspaceMustMatchItem is the regression instrument: a direct
// INSERT filing a reminder under a workspace that is not its item's must be
// refused by the DATABASE.
//
// It goes through raw SQL on purpose. CreateReminder derives workspace_id
// from the item itself, so it cannot produce the row this constraint exists to
// forbid — testing through it would assert that a correct writer writes
// correctly, which is the thing already true before the migration.
func TestReminderWorkspaceMustMatchItem(t *testing.T) {
	s := testStore(t)
	item, own, other := reminderConstraintFixture(t, s)
	ts := now()

	// Control: the AGREEING row is accepted by the same statement shape. Without
	// this leg a broken INSERT would look like a working constraint.
	if _, err := s.db.Exec(s.q(`
		INSERT INTO item_reminders (id, workspace_id, item_id, remind_at, fired_at, acked_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, NULL, NULL, ?, ?)`),
		newID(), own, item.ID, time.Now().UTC().Add(time.Hour).Format(time.RFC3339), ts, ts); err != nil {
		t.Fatalf("control leg failed: the agreeing row was rejected: %v", err)
	}

	_, err := s.db.Exec(s.q(`
		INSERT INTO item_reminders (id, workspace_id, item_id, remind_at, fired_at, acked_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, NULL, NULL, ?, ?)`),
		newID(), other, item.ID, time.Now().UTC().Add(time.Hour).Format(time.RFC3339), ts, ts)
	if err == nil {
		t.Fatal("a reminder filed under a workspace that is not its item's was ACCEPTED; the composite foreign key is not in force")
	}
	t.Logf("disagreeing row refused: %v", err)
}

// TestReminderCascadeSurvivesTheCompositeKey is a PRESERVATION LOCK, not a
// regression instrument — it passes before and after migration 086. Replacing
// a single-column foreign key with a composite one is exactly the edit that
// can drop ON DELETE CASCADE without anything else noticing, and a reminder
// for a deleted item is a notification nobody can act on (085's rationale).
func TestReminderCascadeSurvivesTheCompositeKey(t *testing.T) {
	s := testStore(t)
	item, own, _ := reminderConstraintFixture(t, s)

	if _, err := s.CreateReminder(own, item.ID, time.Now().UTC().Add(time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("CreateReminder: %v", err)
	}
	var before int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM item_reminders WHERE item_id = ?`), item.ID).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if before != 1 {
		t.Fatalf("control leg failed: reminders before delete = %d, want 1", before)
	}

	// A HARD delete: the product's ordinary delete is a soft one, which by
	// design leaves reminders alone. The cascade is what protects the row when
	// the item is really gone (workspace purge).
	if _, err := s.db.Exec(s.q(`DELETE FROM items WHERE id = ?`), item.ID); err != nil {
		t.Fatalf("hard delete item: %v", err)
	}
	var after int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM item_reminders WHERE item_id = ?`), item.ID).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != 0 {
		t.Errorf("reminders after the item was deleted = %d, want 0 — the rebuild dropped ON DELETE CASCADE", after)
	}
}

// TestMigration086DropsPreExistingDisagreeingRows exercises the migration's
// repair pass against the shape it was written for: a database that already
// holds a disagreeing row when 086 runs.
//
// The pass matters because the alternative is a deployment incident. Postgres
// would fail ADD CONSTRAINT on such a row and SQLite would fail the rebuild's
// INSERT ... SELECT, and a migration that fails takes startup with it.
//
// Faithful in the parts that count: the migration text is read from the
// embedded FS (not retyped), applied through the real applySQLiteMigration,
// against the real items table. The one hand-written piece is the pre-086
// item_reminders shape, copied from 085.
func TestMigration086DropsPreExistingDisagreeingRows(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite rebuild path; the Postgres half of 086 is a DELETE + ADD CONSTRAINT")
	}
	const migrationName = "086_item_reminders_composite_fk.sql"

	dbPath := filepath.Join(t.TempDir(), "migrate086.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	item, own, other := reminderConstraintFixture(t, s)
	ts := now()
	remindAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	// Rewind to the pre-086 world: drop the constrained table, rebuild 085's
	// shape, and forget the bookkeeping row so the migration can run again.
	for _, stmt := range []string{
		`DROP TABLE item_reminders`,
		`CREATE TABLE item_reminders (
			id           TEXT PRIMARY KEY,
			workspace_id TEXT NOT NULL,
			item_id      TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
			remind_at    TEXT NOT NULL,
			fired_at     TEXT,
			acked_at     TEXT,
			created_at   TEXT NOT NULL,
			updated_at   TEXT NOT NULL
		)`,
		`DELETE FROM schema_migrations WHERE version = '` + migrationName + `'`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("rewind %q: %v", stmt, err)
		}
	}

	goodID, badID := newID(), newID()
	for _, r := range []struct{ id, ws string }{{goodID, own}, {badID, other}} {
		if _, err := s.db.Exec(`
			INSERT INTO item_reminders (id, workspace_id, item_id, remind_at, fired_at, acked_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, NULL, NULL, ?, ?)`,
			r.id, r.ws, item.ID, remindAt, ts, ts); err != nil {
			t.Fatalf("seed pre-migration row (ws=%s): %v", r.ws, err)
		}
	}
	// Control: the pre-086 schema really does admit the disagreeing row. If it
	// did not, the migration would have nothing to repair and a green below
	// would mean nothing.
	var seeded int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM item_reminders`).Scan(&seeded); err != nil {
		t.Fatalf("count seeded: %v", err)
	}
	if seeded != 2 {
		t.Fatalf("control leg failed: seeded %d rows into the pre-086 table, want 2", seeded)
	}

	body, err := migrationsFS.ReadFile("migrations/" + migrationName)
	if err != nil {
		t.Fatalf("read embedded migration: %v", err)
	}
	if !strings.Contains(string(body), "item_reminders_new") {
		t.Fatalf("control leg failed: the embedded migration is not the rebuild this test targets")
	}
	if err := applySQLiteMigration(s.db, migrationName, string(body)); err != nil {
		t.Fatalf("the migration failed on a database holding a disagreeing row: %v", err)
	}

	var survivors []string
	rows, err := s.db.Query(`SELECT id FROM item_reminders ORDER BY id`)
	if err != nil {
		t.Fatalf("read survivors: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		survivors = append(survivors, id)
	}
	if len(survivors) != 1 || survivors[0] != goodID {
		t.Fatalf("survivors = %v, want just the agreeing row %s", survivors, goodID)
	}

	// The rebuilt table is constrained, and its indexes came back.
	if _, err := s.db.Exec(`
		INSERT INTO item_reminders (id, workspace_id, item_id, remind_at, fired_at, acked_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, NULL, NULL, ?, ?)`,
		newID(), other, item.ID, remindAt, ts, ts); err == nil {
		t.Error("after the rebuild the table still accepts a disagreeing row")
	}
	for _, idx := range []string{"idx_item_reminders_armed", "idx_item_reminders_unacked", "idx_item_reminders_item"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&n); err != nil {
			t.Fatalf("look up %s: %v", idx, err)
		}
		if n != 1 {
			t.Errorf("index %s did not survive the rebuild", idx)
		}
	}

	// And foreign_keys came back ON for the pooled connection, which the
	// runner's defer is responsible for. A rebuild that leaks FKs=OFF would
	// disable enforcement for whoever checks that connection out next.
	var fk int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("read pragma: %v", err)
	}
	if fk != 1 {
		t.Errorf("PRAGMA foreign_keys = %d after the migration, want 1", fk)
	}
	var _ = sql.ErrNoRows
}

// TestMigration063DropsPreExistingDisagreeingRows is the Postgres counterpart.
// The two halves repair by different means — SQLite filters the rebuild's
// copy, Postgres DELETEs before ADD CONSTRAINT — so one test cannot stand for
// both, and the untested half is the one that would fail a deployment's
// startup.
func TestMigration063DropsPreExistingDisagreeingRows(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("Postgres half of IDEA-2883; the SQLite rebuild has its own test")
	}
	const migrationName = "063_item_reminders_composite_fk.sql"

	s := testStore(t)
	item, own, other := reminderConstraintFixture(t, s)
	ts := now()
	remindAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	// Rewind: drop the constraint this migration added and forget its
	// bookkeeping row, so the table admits the row again and 063 can re-run.
	for _, stmt := range []string{
		`ALTER TABLE item_reminders DROP CONSTRAINT item_reminders_item_workspace_fkey`,
		`DELETE FROM schema_migrations WHERE version = '` + migrationName + `'`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("rewind %q: %v", stmt, err)
		}
	}

	goodID, badID := newID(), newID()
	for _, r := range []struct{ id, ws string }{{goodID, own}, {badID, other}} {
		if _, err := s.db.Exec(`
			INSERT INTO item_reminders (id, workspace_id, item_id, remind_at, fired_at, acked_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NULL, NULL, $5, $6)`,
			r.id, r.ws, item.ID, remindAt, ts, ts); err != nil {
			t.Fatalf("seed pre-migration row (ws=%s): %v", r.ws, err)
		}
	}
	// Control: without the constraint the disagreeing row really is admitted,
	// so the migration below has something to repair.
	var seeded int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM item_reminders WHERE item_id = $1`, item.ID).Scan(&seeded); err != nil {
		t.Fatalf("count seeded: %v", err)
	}
	if seeded != 2 {
		t.Fatalf("control leg failed: seeded %d rows, want 2", seeded)
	}

	body, err := pgMigrationsFS.ReadFile("pgmigrations/" + migrationName)
	if err != nil {
		t.Fatalf("read embedded migration: %v", err)
	}
	if !strings.Contains(string(body), "ADD CONSTRAINT item_reminders_item_workspace_fkey") {
		t.Fatal("control leg failed: the embedded migration is not the one this test targets")
	}
	if err := applyPostgresMigration(s.db, migrationName, string(body)); err != nil {
		t.Fatalf("the migration failed on a database holding a disagreeing row: %v", err)
	}

	var survivors []string
	rows, err := s.db.Query(`SELECT id FROM item_reminders WHERE item_id = $1 ORDER BY id`, item.ID)
	if err != nil {
		t.Fatalf("read survivors: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		survivors = append(survivors, id)
	}
	if len(survivors) != 1 || survivors[0] != goodID {
		t.Fatalf("survivors = %v, want just the agreeing row %s", survivors, goodID)
	}

	if _, err := s.db.Exec(`
		INSERT INTO item_reminders (id, workspace_id, item_id, remind_at, fired_at, acked_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NULL, NULL, $5, $6)`,
		newID(), other, item.ID, remindAt, ts, ts); err == nil {
		t.Error("after the migration the table still accepts a disagreeing row")
	}
}

// TestMigration063DropsEveryLegacyItemIdConstraint exercises the LOOP in 063's
// DO block, which the repair-pass test above cannot: that fixture carries one
// legacy foreign key, and one is what a `SELECT ... INTO` handles correctly
// too. Two is where they differ — plpgsql's INTO takes the first row and does
// not error, so the second would survive and sit alongside the composite key,
// leaving the two dialects disagreeing about the table's shape while the
// migration reported success.
//
// Nothing creates a duplicate today. It is tested because the loop exists to
// handle a case no other test reaches, and an untested branch that only runs
// on a database nobody has is worse than no branch at all.
func TestMigration063DropsEveryLegacyItemIdConstraint(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("Postgres-only: the DO block has no SQLite counterpart (the rebuild replaces the FK wholesale)")
	}
	const migrationName = "063_item_reminders_composite_fk.sql"

	s := testStore(t)

	legacyCount := func() int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(`
			SELECT COUNT(*) FROM pg_constraint
			WHERE conrelid = 'item_reminders'::regclass
			  AND contype = 'f'
			  AND confrelid = 'items'::regclass
			  AND conkey = ARRAY[(SELECT attnum FROM pg_attribute
			                      WHERE attrelid = 'item_reminders'::regclass AND attname = 'item_id')]::smallint[]
		`).Scan(&n); err != nil {
			t.Fatalf("count legacy constraints: %v", err)
		}
		return n
	}

	// Rewind past 063 and put TWO single-column FKs on item_id, the shape the
	// loop exists for.
	for _, stmt := range []string{
		`ALTER TABLE item_reminders DROP CONSTRAINT item_reminders_item_workspace_fkey`,
		`ALTER TABLE item_reminders ADD CONSTRAINT item_reminders_item_id_fkey FOREIGN KEY (item_id) REFERENCES items(id) ON DELETE CASCADE`,
		`ALTER TABLE item_reminders ADD CONSTRAINT item_reminders_item_id_fkey1 FOREIGN KEY (item_id) REFERENCES items(id) ON DELETE CASCADE`,
		`DELETE FROM schema_migrations WHERE version = '` + migrationName + `'`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("rewind %q: %v", stmt, err)
		}
	}
	if got := legacyCount(); got != 2 {
		t.Fatalf("control leg failed: seeded %d legacy constraints, want 2 — the loop's case is not set up", got)
	}

	body, err := pgMigrationsFS.ReadFile("pgmigrations/" + migrationName)
	if err != nil {
		t.Fatalf("read embedded migration: %v", err)
	}
	if err := applyPostgresMigration(s.db, migrationName, string(body)); err != nil {
		t.Fatalf("migration failed against two legacy constraints: %v", err)
	}

	if got := legacyCount(); got != 0 {
		t.Errorf("legacy single-column constraints remaining = %d, want 0 — the DO block dropped only the first", got)
	}

	// And the composite key is the one in force.
	var composite int
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM pg_constraint
		WHERE conrelid = 'item_reminders'::regclass AND conname = 'item_reminders_item_workspace_fkey'
	`).Scan(&composite); err != nil {
		t.Fatalf("count composite: %v", err)
	}
	if composite != 1 {
		t.Errorf("composite constraints = %d, want 1", composite)
	}
}
