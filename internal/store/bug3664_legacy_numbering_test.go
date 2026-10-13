package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// BUG-3664: the legacy per-collection item numbering was converted on startup
// only while idx_items_collection_number existed, and SQLite migration 056's
// items rebuild drops that index. A legacy database upgrading through 056 kept
// its per-collection numbers and failed every startup creating the workspace
// index.

func bug3664Path(t *testing.T) string {
	t.Helper()
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite migration 056")
	}
	return filepath.Join(t.TempDir(), "legacy.db")
}

// openRaw opens the file with the store's DSN pragmas, below the store.
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	if err := registerGuardedDrivers(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(guardedSQLiteDriver, path+"?_pragma=busy_timeout(30000)&_pragma=foreign_keys(on)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// seedTwoNumberOnes puts two items numbered 1 in one workspace, one in each of
// two collections: per-collection numbering.
func seedTwoNumberOnes(t *testing.T, db *sql.DB) {
	t.Helper()
	ts := "2026-01-01T00:00:00Z"
	for _, stmt := range []string{
		`INSERT INTO workspaces (id, name, slug, created_at, updated_at) VALUES ('w1', 'W', 'w', '` + ts + `', '` + ts + `')`,
		`INSERT INTO collections (id, workspace_id, name, slug, prefix, created_at, updated_at) VALUES ('c1', 'w1', 'Tasks', 'tasks', 'TASK', '` + ts + `', '` + ts + `')`,
		`INSERT INTO collections (id, workspace_id, name, slug, prefix, created_at, updated_at) VALUES ('c2', 'w1', 'Ideas', 'ideas', 'IDEA', '` + ts + `', '` + ts + `')`,
		`INSERT INTO items (id, workspace_id, collection_id, title, slug, item_number, created_at, updated_at) VALUES ('i1', 'w1', 'c1', 'A', 'a', 1, '2026-01-01T00:00:01Z', '` + ts + `')`,
		`INSERT INTO items (id, workspace_id, collection_id, title, slug, item_number, created_at, updated_at) VALUES ('i2', 'w1', 'c2', 'B', 'b', 1, '2026-01-01T00:00:02Z', '` + ts + `')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}
}

func assertWorkspaceNumbering(t *testing.T, s *Store) {
	t.Helper()
	if !s.indexExists("idx_items_workspace_number") {
		t.Error("idx_items_workspace_number missing")
	}
	var a, b int
	if err := s.db.QueryRow(`SELECT item_number FROM items WHERE id = 'i1'`).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT item_number FROM items WHERE id = 'i2'`).Scan(&b); err != nil {
		t.Fatal(err)
	}
	if a != 1 || b != 2 {
		t.Errorf("item numbers = %d, %d; want 1, 2 (workspace order by created_at)", a, b)
	}
}

// A database from before workspace numbering, stopped before 056, holding the
// legacy index and per-collection numbers, upgrades in one go.
func TestBUG3664_LegacyDatabaseUpgradesThrough056(t *testing.T) {
	path := bug3664Path(t)
	db := openRaw(t, path)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	names, err := readMigrationNames(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= "056" {
			break
		}
		data, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := applySQLiteMigration(db, name, string(data)); err != nil {
			t.Fatalf("pre-056 migration %s: %v", name, err)
		}
	}
	// The index the initial release created in code.
	if _, err := db.Exec(`CREATE UNIQUE INDEX idx_items_collection_number ON items(collection_id, item_number)`); err != nil {
		t.Fatal(err)
	}
	seedTwoNumberOnes(t, db)
	db.Close()

	s, err := New(path)
	if err != nil {
		t.Fatalf("upgrading a legacy database through 056: %v", err)
	}
	defer s.Close()
	assertWorkspaceNumbering(t, s)
}

// A database an earlier release left in the broken state: fully migrated, no
// legacy index, no workspace index, per-collection numbers. It recovers on
// the next startup.
func TestBUG3664_StuckDatabaseRecoversOnStartup(t *testing.T) {
	path := bug3664Path(t)
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	db := openRaw(t, path)
	if _, err := db.Exec(`DROP INDEX idx_items_workspace_number`); err != nil {
		t.Fatal(err)
	}
	seedTwoNumberOnes(t, db)
	db.Close()

	s, err = New(path)
	if err != nil {
		t.Fatalf("restarting a stuck database: %v", err)
	}
	defer s.Close()
	assertWorkspaceNumbering(t, s)
}

// A current database whose numbers are already unique per workspace is left
// exactly as it is: the fallback marker is repetition, not a missing index.
func TestBUG3664_CurrentNumberingIsNotRenumbered(t *testing.T) {
	path := bug3664Path(t)
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	db := openRaw(t, path)
	if _, err := db.Exec(`DROP INDEX idx_items_workspace_number`); err != nil {
		t.Fatal(err)
	}
	seedTwoNumberOnes(t, db)
	if _, err := db.Exec(`UPDATE items SET item_number = 7 WHERE id = 'i2'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var b int
	if err := s.db.QueryRow(`SELECT item_number FROM items WHERE id = 'i2'`).Scan(&b); err != nil || b != 7 {
		t.Fatalf("i2's number = %d (%v), want 7 kept", b, err)
	}
}
