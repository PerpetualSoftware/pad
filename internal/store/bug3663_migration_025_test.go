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
