package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BUG-3663: 025_doc_type_plan rebuilds documents with DROP TABLE while
// foreign keys are on, and versions.document_id references documents with no
// cascade. A database upgraded from before 025 that holds a version row
// failed every startup with "FOREIGN KEY constraint failed".

// preMigrationDB opens a SQLite database with the store's DSN pragmas and
// applies every migration whose name sorts before stop, through the real
// runner.
func preMigrationDB(t *testing.T, stop string) (*sql.DB, []string) {
	t.Helper()
	if err := registerGuardedDrivers(); err != nil {
		t.Fatal(err)
	}
	dsn := filepath.Join(t.TempDir(), "pre.db") + "?_pragma=busy_timeout(30000)&_pragma=foreign_keys(on)&_txlock=immediate"
	db, err := sql.Open(guardedSQLiteDriver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	names, err := readMigrationNames(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	var rest []string
	for _, name := range names {
		if name >= stop {
			rest = append(rest, name)
			continue
		}
		data, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := applySQLiteMigration(db, name, string(data)); err != nil {
			t.Fatalf("pre-%s migration %s: %v", stop, name, err)
		}
	}
	return db, rest
}

func TestBUG3663_Migration025UpgradesADatabaseWithVersions(t *testing.T) {
	db, rest := preMigrationDB(t, "025")
	ts := "2026-01-01T00:00:00Z"
	for _, stmt := range []string{
		`INSERT INTO workspaces (id, name, slug, created_at, updated_at) VALUES ('w1', 'W', 'w', '` + ts + `', '` + ts + `')`,
		`INSERT INTO documents (id, workspace_id, title, slug, doc_type, created_at, updated_at) VALUES ('d1', 'w1', 'Doc', 'doc', 'phase-plan', '` + ts + `', '` + ts + `')`,
		`INSERT INTO versions (id, document_id, content, created_at) VALUES ('v1', 'd1', 'body', '` + ts + `')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	for _, name := range rest {
		data, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := applySQLiteMigration(db, name, string(data)); err != nil {
			t.Fatalf("upgrade %s: %v", name, err)
		}
		if name >= "025" && name < "026" {
			var docType string
			if err := db.QueryRow(`SELECT doc_type FROM documents WHERE id = 'd1'`).Scan(&docType); err != nil || docType != "plan" {
				t.Fatalf("after %s: doc_type = %q (%v), want plan", name, docType, err)
			}
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM versions WHERE id = 'v1' AND document_id = 'd1'`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("after %s: version rows = %d (%v), want 1", name, n, err)
			}
		}
	}
}

// fkCheckStore is a migrated store with a parent/child pair to break.
func fkCheckStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific (PRAGMA semantics)")
	}
	s, err := New(filepath.Join(t.TempDir(), "fk.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, stmt := range []string{
		`CREATE TABLE fk3663_parent (id TEXT PRIMARY KEY)`,
		`CREATE TABLE fk3663_child (id TEXT PRIMARY KEY, parent_id TEXT REFERENCES fk3663_parent(id))`,
		`INSERT INTO fk3663_parent (id) VALUES ('p1'), ('p2')`,
		`INSERT INTO fk3663_child (id, parent_id) VALUES ('c1', 'p1'), ('c2', 'p2')`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	return s
}

func recorded(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// A migration that turns foreign keys off and leaves a reference dangling is
// refused, and rolls back with its bookkeeping.
func TestBUG3663_FKOffMigrationThatLeavesAViolationIsRefused(t *testing.T) {
	s := fkCheckStore(t)
	const name = "999_bug3663_breaks_a_reference.sql"
	err := applySQLiteMigration(s.db, name, "PRAGMA foreign_keys = OFF;\nDELETE FROM fk3663_parent WHERE id = 'p1';\nPRAGMA foreign_keys = ON;\n")
	if err == nil || !strings.Contains(err.Error(), "foreign key violation") {
		t.Fatalf("err = %v, want a refusal naming the foreign key violation", err)
	}
	if recorded(t, s, name) {
		t.Error("the refused migration was recorded")
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM fk3663_parent WHERE id = 'p1'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("the refused migration's delete committed: parent rows = %d (%v)", n, err)
	}
}

// A violation the database already had (written before FKs were enforced on
// every connection) does not block a migration that adds none.
func TestBUG3663_FKOffMigrationIgnoresAViolationThatPredatesIt(t *testing.T) {
	s := fkCheckStore(t)
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{"PRAGMA foreign_keys = OFF", "DELETE FROM fk3663_parent WHERE id = 'p2'", "PRAGMA foreign_keys = ON"} {
		if _, err := conn.ExecContext(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	const name = "999_bug3663_rebuilds_cleanly.sql"
	body := "PRAGMA foreign_keys = OFF;\nCREATE TABLE fk3663_parent_new (id TEXT PRIMARY KEY);\nINSERT INTO fk3663_parent_new SELECT id FROM fk3663_parent;\nDROP TABLE fk3663_parent;\nALTER TABLE fk3663_parent_new RENAME TO fk3663_parent;\nPRAGMA foreign_keys = ON;\n"
	if err := applySQLiteMigration(s.db, name, body); err != nil {
		t.Fatalf("a clean rebuild over a legacy violation: %v", err)
	}
	if !recorded(t, s, name) {
		t.Error("the clean migration was not recorded")
	}
}

// The legacy violation sits in a table the migration rebuilds, and the copy
// renumbers its rowids (codex r1): it is still the same one violation, and
// does not block.
func TestBUG3663_FKOffRebuildThatRenumbersALegacyViolationIsNotRefused(t *testing.T) {
	s := fkCheckStore(t)
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{"PRAGMA foreign_keys = OFF", "DELETE FROM fk3663_parent WHERE id = 'p2'", "PRAGMA foreign_keys = ON"} {
		if _, err := conn.ExecContext(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	const name = "999_bug3663_rebuilds_the_child.sql"
	body := "PRAGMA foreign_keys = OFF;\nCREATE TABLE fk3663_child_new (id TEXT PRIMARY KEY, parent_id TEXT REFERENCES fk3663_parent(id));\nINSERT INTO fk3663_child_new (id, parent_id) SELECT id, parent_id FROM fk3663_child ORDER BY id DESC;\nDROP TABLE fk3663_child;\nALTER TABLE fk3663_child_new RENAME TO fk3663_child;\nPRAGMA foreign_keys = ON;\n"
	if err := applySQLiteMigration(s.db, name, body); err != nil {
		t.Fatalf("a rebuild that renumbers a legacy violation: %v", err)
	}
	var rowid int64
	if err := s.db.QueryRow(`SELECT rowid FROM fk3663_child WHERE id = 'c2'`).Scan(&rowid); err != nil || rowid != 1 {
		t.Fatalf("instrument: c2's rowid = %d (%v), want 1 (renumbered from 2)", rowid, err)
	}
}

// foreign_key_check can fail outright ("foreign key mismatch") when an FK's
// parent key has no unique index, as a rebuild can leave mid-chain (CI on
// #2120, re-applying 056). That is not a violation the migration made, and
// must not refuse it.
func TestBUG3663_FKOffMigrationIsNotRefusedWhenTheCheckCannotRun(t *testing.T) {
	s := fkCheckStore(t)
	for _, stmt := range []string{
		`CREATE TABLE fk3663_loose_parent (code TEXT)`,
		`CREATE TABLE fk3663_loose_child (id TEXT PRIMARY KEY, code TEXT REFERENCES fk3663_loose_parent(code))`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	const name = "999_bug3663_over_a_mismatched_fk.sql"
	if err := applySQLiteMigration(s.db, name, "PRAGMA foreign_keys = OFF;\nCREATE TABLE fk3663_noop (id TEXT);\nPRAGMA foreign_keys = ON;\n"); err != nil {
		t.Fatalf("an FK-off migration over a schema the check cannot read: %v", err)
	}
	if !recorded(t, s, name) {
		t.Error("the migration was not recorded")
	}
}

// The same, but the check runs cleanly BEFORE and only the migration's body
// leaves the schema it cannot read: the parent key starts with a unique
// index and the body drops it. This is the after-stage path; the leg above
// skips the check before the body runs and never reaches it.
func TestBUG3663_FKOffMigrationIsNotRefusedWhenItsBodyBreaksTheCheck(t *testing.T) {
	s := fkCheckStore(t)
	for _, stmt := range []string{
		`CREATE TABLE fk3663_keyed_parent (code TEXT)`,
		`CREATE UNIQUE INDEX fk3663_keyed_parent_code ON fk3663_keyed_parent (code)`,
		`CREATE TABLE fk3663_keyed_child (id TEXT PRIMARY KEY, code TEXT REFERENCES fk3663_keyed_parent(code))`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := fkCheckRuns(t, s); err != nil {
		t.Fatalf("instrument: the check must run before the body: %v", err)
	}
	const name = "999_bug3663_body_drops_the_parent_key.sql"
	if err := applySQLiteMigration(s.db, name, "PRAGMA foreign_keys = OFF;\nDROP INDEX fk3663_keyed_parent_code;\nPRAGMA foreign_keys = ON;\n"); err != nil {
		t.Fatalf("an FK-off migration whose body leaves a schema the check cannot read: %v", err)
	}
	if !recorded(t, s, name) {
		t.Error("the migration was not recorded")
	}
	if err := fkCheckRuns(t, s); err == nil {
		t.Fatal("instrument: after the body the check must fail, or this leg does not reach the after stage")
	}
}

func fkCheckRuns(t *testing.T, s *Store) error {
	t.Helper()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	res, err := foreignKeyViolations(context.Background(), tx)
	if err != nil {
		t.Fatal(err)
	}
	for _, cerr := range res.unavailable {
		return cerr
	}
	return nil
}

// A table whose check cannot run does not blind the check for the others
// (codex on #2120): with one unreadable FK in the schema, a dangling
// reference the body leaves in a different, readable table is refused.
func TestBUG3663_UnreadableTableDoesNotHideAViolationElsewhere(t *testing.T) {
	s := fkCheckStore(t)
	for _, stmt := range []string{
		`CREATE TABLE fk3663_loose_parent (code TEXT)`,
		`CREATE TABLE fk3663_loose_child (id TEXT PRIMARY KEY, code TEXT REFERENCES fk3663_loose_parent(code))`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := fkCheckRuns(t, s); err == nil {
		t.Fatal("instrument: the loose FK must make one table's check fail")
	}
	const name = "999_bug3663_dangles_beside_a_mismatch.sql"
	err := applySQLiteMigration(s.db, name, "PRAGMA foreign_keys = OFF;\nDELETE FROM fk3663_parent WHERE id = 'p1';\nPRAGMA foreign_keys = ON;\n")
	if err == nil || !strings.Contains(err.Error(), "fk3663_child|fk3663_parent") {
		t.Fatalf("a dangling reference beside an unreadable FK: err = %v, want a refusal naming fk3663_child|fk3663_parent", err)
	}
	if recorded(t, s, name) {
		t.Error("the refused migration was recorded")
	}
}

// Failing to list the tables at all is the check being unavailable, not a
// reason to refuse the upgrade (codex on #2120).
func TestBUG3663_FKOffMigrationIsNotRefusedWhenTheTablesCannotBeListed(t *testing.T) {
	s := fkCheckStore(t)
	saved := fkCheckTablesQuery
	fkCheckTablesQuery = `SELECT name FROM fk3663_no_such_table`
	t.Cleanup(func() { fkCheckTablesQuery = saved })
	const name = "999_bug3663_unlistable.sql"
	if err := applySQLiteMigration(s.db, name, "PRAGMA foreign_keys = OFF;\nCREATE TABLE fk3663_noop (id TEXT);\nPRAGMA foreign_keys = ON;\n"); err != nil {
		t.Fatalf("an FK-off migration whose check cannot list the tables: %v", err)
	}
	if !recorded(t, s, name) {
		t.Error("the migration was not recorded")
	}
}

// The same at the after stage only: the listing reads a sentinel table the
// body drops, so the before pass lists and the after pass cannot.
func TestBUG3663_FKOffMigrationIsNotRefusedWhenItsBodyBreaksTheListing(t *testing.T) {
	s := fkCheckStore(t)
	if _, err := s.db.Exec(`CREATE TABLE fk3663_sentinel (id TEXT)`); err != nil {
		t.Fatal(err)
	}
	saved := fkCheckTablesQuery
	fkCheckTablesQuery = saved[:strings.Index(saved, " ORDER BY")] + ` AND EXISTS (SELECT 1 FROM fk3663_sentinel) ORDER BY name`
	t.Cleanup(func() { fkCheckTablesQuery = saved })
	if err := fkCheckRuns(t, s); err != nil {
		t.Fatalf("instrument: the before pass must list: %v", err)
	}
	const name = "999_bug3663_drops_the_sentinel.sql"
	if err := applySQLiteMigration(s.db, name, "PRAGMA foreign_keys = OFF;\nDROP TABLE fk3663_sentinel;\nPRAGMA foreign_keys = ON;\n"); err != nil {
		t.Fatalf("an FK-off migration whose body breaks the table listing: %v", err)
	}
	if !recorded(t, s, name) {
		t.Error("the migration was not recorded")
	}
}

// A listing that returns rows and THEN fails is a failed listing, not a
// complete one (codex on #2120): the partial list must not be compared. Here
// the partial list includes fk3663_child, where the body leaves a dangling
// reference; treated as complete, that would refuse the migration.
func TestBUG3663_ListingThatFailsMidwayIsNotTreatedAsComplete(t *testing.T) {
	s := fkCheckStore(t)
	saved := fkCheckTablesQuery
	fkCheckTablesQuery = `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' UNION ALL SELECT CAST(abs(-9223372036854775807 - (rowid > 0)) AS TEXT) FROM sqlite_master WHERE name = 'fk3663_child'`
	t.Cleanup(func() { fkCheckTablesQuery = saved })
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(fkCheckTablesQuery)
	if err != nil {
		t.Fatalf("instrument: the listing must prepare and start: %v", err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	iterErr := rows.Err()
	rows.Close()
	tx.Rollback()
	if n == 0 || iterErr == nil {
		t.Fatalf("instrument: want rows then an iteration error, got %d rows, err %v", n, iterErr)
	}
	const name = "999_bug3663_partial_listing.sql"
	if err := applySQLiteMigration(s.db, name, "PRAGMA foreign_keys = OFF;\nDELETE FROM fk3663_parent WHERE id = 'p1';\nPRAGMA foreign_keys = ON;\n"); err != nil {
		t.Fatalf("an FK-off migration whose table listing fails midway: %v", err)
	}
}
