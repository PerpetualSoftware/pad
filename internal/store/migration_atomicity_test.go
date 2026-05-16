package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestMigrationAtomicity_FailedSQLite_RollsBackPartialDDLAndBookkeeping
// guards the IDEA-1485 atomicity guarantee for SQLite migrations: when a
// multi-statement migration fails partway through, NEITHER the partial
// schema change NOR the schema_migrations bookkeeping row survives.
//
// Before IDEA-1485 the runner exec'd statements one-by-one on the raw
// *sql.DB, so a crash (or SQL error) between statement N and statement
// N+1 left the schema in an intermediate state but did NOT record the
// migration, and the next startup re-ran the migration against the
// partially-mutated schema — exactly the data-loss window described in
// the IDEA's "Problem" section.
func TestMigrationAtomicity_FailedSQLite_RollsBackPartialDDLAndBookkeeping(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific atomicity test; Postgres has its own counterpart below")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "atomic.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	// Construct a deliberately-failing multi-statement migration:
	// statement 1 succeeds (creates a table), statement 2 fails
	// (references a non-existent table). Without tx-wrapping, the
	// table from statement 1 would survive; with tx-wrapping, it
	// rolls back.
	migration := `
CREATE TABLE atomic_test_t1 (id TEXT PRIMARY KEY);
INSERT INTO no_such_table_xyz (id) VALUES ('boom');
`

	err = applySQLiteMigration(s.db, "999_atomicity_probe.sql", migration)
	if err == nil {
		t.Fatal("expected migration to fail, got nil")
	}

	// schema_migrations must NOT carry a row for the failed migration.
	var count int
	if qerr := s.db.QueryRow(
		"SELECT COUNT(*) FROM schema_migrations WHERE version = ?",
		"999_atomicity_probe.sql",
	).Scan(&count); qerr != nil {
		t.Fatalf("query schema_migrations: %v", qerr)
	}
	if count != 0 {
		t.Errorf("schema_migrations row WAS recorded for failed migration; want 0 rows, got %d", count)
	}

	// The CREATE TABLE from statement 1 must have been rolled back.
	var tblName string
	qerr := s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='atomic_test_t1'`,
	).Scan(&tblName)
	if qerr == nil {
		t.Errorf("atomic_test_t1 still exists after failed migration; rollback did not happen")
	} else if qerr != sql.ErrNoRows {
		t.Fatalf("query sqlite_master: %v", qerr)
	}
}

// TestMigrationAtomicity_SuccessfulSQLite_RecordsBookkeeping is the
// positive control: a normal multi-statement migration commits both the
// DDL and the schema_migrations row together.
func TestMigrationAtomicity_SuccessfulSQLite_RecordsBookkeeping(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific")
	}

	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "atomic.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	migration := `
CREATE TABLE atomic_ok_t1 (id TEXT PRIMARY KEY);
CREATE TABLE atomic_ok_t2 (id TEXT PRIMARY KEY);
`
	if err := applySQLiteMigration(s.db, "999_atomic_ok.sql", migration); err != nil {
		t.Fatalf("applySQLiteMigration: %v", err)
	}

	var count int
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM schema_migrations WHERE version = ?",
		"999_atomic_ok.sql",
	).Scan(&count); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 schema_migrations row, got %d", count)
	}

	for _, tbl := range []string{"atomic_ok_t1", "atomic_ok_t2"} {
		var name string
		if err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&name); err != nil {
			t.Errorf("table %s missing after successful migration: %v", tbl, err)
		}
	}
}

// TestMigrationAtomicity_PragmaForeignKeysLifted exercises the
// PRAGMA-detection branch: a migration with `PRAGMA foreign_keys = OFF/ON`
// must succeed AND the PRAGMA must take effect (i.e. it was emitted
// outside the wrapping transaction, where SQLite would otherwise
// silently no-op it).
//
// The test rebuilds a tiny FK-having pair of tables: parent_t and
// child_t (child_t.parent_id REFERENCES parent_t(id)). The migration
// drops parent_t and recreates it; without FK-off this would fail
// because child_t still references it.
func TestMigrationAtomicity_PragmaForeignKeysLifted(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific (PRAGMA semantics)")
	}

	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "atomic.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	// Seed FK-having tables outside the migration runner.
	seed := []string{
		`CREATE TABLE atomic_parent_t (id TEXT PRIMARY KEY)`,
		`CREATE TABLE atomic_child_t (id TEXT PRIMARY KEY, parent_id TEXT REFERENCES atomic_parent_t(id))`,
		`INSERT INTO atomic_parent_t (id) VALUES ('p1')`,
		`INSERT INTO atomic_child_t (id, parent_id) VALUES ('c1', 'p1')`,
	}
	for _, stmt := range seed {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	// Rebuild parent_t — drop and recreate. With FKs enforced this would
	// fail (the child row references the old parent). The migration
	// disables FKs via PRAGMA so the rebuild succeeds.
	migration := `
PRAGMA foreign_keys = OFF;
DROP TABLE IF EXISTS atomic_parent_t_new;
CREATE TABLE atomic_parent_t_new (id TEXT PRIMARY KEY, extra TEXT NOT NULL DEFAULT '');
INSERT INTO atomic_parent_t_new (id) SELECT id FROM atomic_parent_t;
DROP TABLE atomic_parent_t;
ALTER TABLE atomic_parent_t_new RENAME TO atomic_parent_t;
PRAGMA foreign_keys = ON;
`
	if err := applySQLiteMigration(s.db, "999_fk_rebuild.sql", migration); err != nil {
		t.Fatalf("applySQLiteMigration: %v", err)
	}

	// Verify the new column exists.
	var hasExtra bool
	rows, err := s.db.Query(`PRAGMA table_info(atomic_parent_t)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == "extra" {
			hasExtra = true
		}
	}
	rows.Close()
	if !hasExtra {
		t.Error("rebuilt atomic_parent_t is missing the 'extra' column")
	}

	// The bookkeeping row must be present.
	var n int
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM schema_migrations WHERE version = ?", "999_fk_rebuild.sql",
	).Scan(&n); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 schema_migrations row, got %d", n)
	}

	// Sanity: foreign_keys should be ON again after the migration.
	var fk int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys not restored after migration; got %d, want 1", fk)
	}
}

// TestExtractPragmas validates the lifter in isolation.
func TestExtractPragmas(t *testing.T) {
	in := `-- header comment
PRAGMA foreign_keys = OFF;
CREATE TABLE x (id TEXT);
INSERT INTO x VALUES ('a');
PRAGMA foreign_keys = ON;
`
	pragmas, rest := extractPragmas(in)
	if len(pragmas) != 2 {
		t.Fatalf("expected 2 pragmas, got %d: %v", len(pragmas), pragmas)
	}
	wantPragmas := []string{"PRAGMA foreign_keys = OFF;", "PRAGMA foreign_keys = ON;"}
	for i, p := range pragmas {
		if p != wantPragmas[i] {
			t.Errorf("pragmas[%d] = %q; want %q", i, p, wantPragmas[i])
		}
	}
	if strings.Contains(strings.ToUpper(rest), "PRAGMA") {
		t.Errorf("PRAGMA still present in rest: %q", rest)
	}
	// CREATE TABLE / INSERT must remain.
	for _, kw := range []string{"CREATE TABLE x", "INSERT INTO x"} {
		if !strings.Contains(rest, kw) {
			t.Errorf("rest missing %q; got %q", kw, rest)
		}
	}
}

// TestMigrationAtomicity_FailedPostgres_RollsBackPartialDDLAndBookkeeping
// is the Postgres counterpart to the SQLite atomicity test. It only
// runs when PAD_TEST_POSTGRES_URL is set.
func TestMigrationAtomicity_FailedPostgres_RollsBackPartialDDLAndBookkeeping(t *testing.T) {
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}

	s := testStorePostgres(t, pgURL)

	// Suffix table names with a random token so concurrent runs of this
	// test (against the shared base URL) cannot collide. Each test gets
	// its own isolated database via testStorePostgres anyway, but the
	// suffix is cheap insurance.
	tag := strings.ReplaceAll(uuid.New().String()[:8], "-", "")
	t1 := "atomic_test_t1_" + tag
	migration := fmt.Sprintf(`
CREATE TABLE %s (id TEXT PRIMARY KEY);
INSERT INTO no_such_table_xyz (id) VALUES ('boom');
`, t1)

	err := applyPostgresMigration(s.db, "999_atomicity_probe.sql", migration)
	if err == nil {
		t.Fatal("expected migration to fail, got nil")
	}

	var count int
	if qerr := s.db.QueryRow(
		"SELECT COUNT(*) FROM schema_migrations WHERE version = $1",
		"999_atomicity_probe.sql",
	).Scan(&count); qerr != nil {
		t.Fatalf("query schema_migrations: %v", qerr)
	}
	if count != 0 {
		t.Errorf("schema_migrations row recorded for failed migration; want 0, got %d", count)
	}

	var tblName string
	qerr := s.db.QueryRow(
		`SELECT tablename FROM pg_tables WHERE tablename = $1`, t1,
	).Scan(&tblName)
	if qerr == nil {
		t.Errorf("%s still exists after failed migration; rollback did not happen", t1)
	} else if qerr != sql.ErrNoRows {
		t.Fatalf("query pg_tables: %v", qerr)
	}
}

// TestMigrationAtomicity_FailedSQLite_RestoresForeignKeysOnError guards the
// codex-R1 P2: once `PRAGMA foreign_keys = OFF` lands on the pinned migration
// connection, any subsequent failure (BeginTx / execMulti / INSERT / Commit
// / post-tx PRAGMA) returns early. The runner MUST register a deferred
// `PRAGMA foreign_keys = ON` so the connection never goes back to the pool
// with FK enforcement disabled — otherwise the next pool checkout silently
// bypasses FKs.
//
// The test forces a single-connection pool via SetMaxOpenConns(1) so the
// post-failure FK check is deterministically against the same physical
// connection that ran the failed migration.
func TestMigrationAtomicity_FailedSQLite_RestoresForeignKeysOnError(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific (PRAGMA foreign_keys is a SQLite construct)")
	}

	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "atomic.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	// Force a single pool connection so the assertion below targets the
	// SAME physical conn that ran the failing migration. Without this,
	// a fresh checkout might land on a different conn (where FKs were
	// never disabled), masking a regression of the bug.
	s.db.SetMaxOpenConns(1)

	// Migration: disable FKs, then fail. The failure point is AFTER the
	// PRAGMA so the FK-OFF state has reached the connection. The failure
	// itself is a violated constraint inside the body so the wrapping tx
	// rolls back.
	migration := `
PRAGMA foreign_keys = OFF;
CREATE TABLE fk_err_t (id TEXT PRIMARY KEY);
INSERT INTO fk_err_t (id) VALUES ('a');
INSERT INTO fk_err_t (id) VALUES ('a');
`
	if err := applySQLiteMigration(s.db, "999_fk_err.sql", migration); err == nil {
		t.Fatal("expected migration to fail (duplicate PK), got nil")
	}

	// On the SAME connection from the (1-conn) pool, foreign_keys must
	// be back to 1. Use a tightly-scoped block so the conn is released
	// back to the pool before later queries that also need it.
	ctx := context.Background()
	func() {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn: %v", err)
		}
		defer conn.Close()

		var fk int
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatalf("PRAGMA foreign_keys: %v", err)
		}
		if fk != 1 {
			t.Errorf("foreign_keys leaked OFF after failed migration; got %d, want 1", fk)
		}
	}()

	// Bookkeeping row must also be absent (atomicity invariant from
	// the prior test, re-asserted here for completeness).
	var count int
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM schema_migrations WHERE version = ?", "999_fk_err.sql",
	).Scan(&count); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if count != 0 {
		t.Errorf("schema_migrations row recorded for failed migration; want 0, got %d", count)
	}
}
