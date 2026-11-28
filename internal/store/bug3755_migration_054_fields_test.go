package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedBUG3755Database(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "invalid_fields.db")
	db := openRaw(t, path)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	names, err := readMigrationNames(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= "054" {
			break
		}
		data, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := applySQLiteMigration(db, name, string(data)); err != nil {
			t.Fatalf("pre-054 migration %s: %v", name, err)
		}
	}
	const ts = "2026-01-01T00:00:00Z"
	if _, err := db.Exec(
		`INSERT INTO workspaces (id, name, slug, created_at, updated_at) VALUES ('w1', 'W', 'w', ?, ?)`, ts, ts,
	); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO collections (id, workspace_id, name, slug, created_at, updated_at) VALUES ('c1', 'w1', 'Tasks', 'tasks', ?, ?)`, ts, ts,
	); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	for _, seed := range []struct {
		id     string
		fields any
	}{
		{"i-garbage", "not json"},
		{"i-empty", ""},
		{"i-null", nil},
		{"i-wrongshape", "[]"},
		{"i-valid", `{"k":"v"}`},
		{"i-slug", `{"invocation_slug":"x"}`},
	} {
		if _, err := db.Exec(
			`INSERT INTO items (id, workspace_id, collection_id, title, slug, fields, created_at, updated_at)
			 VALUES (?, 'w1', 'c1', ?, ?, ?, ?, ?)`,
			seed.id, seed.id, seed.id, seed.fields, ts, ts,
		); err != nil {
			t.Fatalf("seed item %s: %v", seed.id, err)
		}
	}
	return path, db
}

func TestBUG3755_UpgradeWithInvalidItemFieldsCompletes(t *testing.T) {
	t.Parallel()
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite migration 054")
	}
	path, db := seedBUG3755Database(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatalf("upgrading a database with invalid item fields through 054: %v", err)
	}
	defer s.Close()
	for id, want := range map[string]string{
		"i-garbage":    `{}`,
		"i-empty":      `{}`,
		"i-null":       `{}`,
		"i-wrongshape": `{}`,
		"i-valid":      `{"k":"v"}`,
		"i-slug":       `{"invocation_slug":"x"}`,
	} {
		var got string
		if err := s.db.QueryRow(`SELECT fields FROM items WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("query item %s: %v", id, err)
		}
		if got != want {
			t.Errorf("item %s fields = %q, want %q", id, got, want)
		}
	}
	if !s.indexExists("idx_items_invocation_slug_per_collection") {
		t.Error("idx_items_invocation_slug_per_collection missing")
	}
}

func TestBUG3755_Migration054RepairsOnlyInvalidJSON(t *testing.T) {
	t.Parallel()
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite migration 054")
	}
	_, db := seedBUG3755Database(t)
	const name = "054_playbooks_invocation_fields.sql"
	data, err := migrationsFS.ReadFile("migrations/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := applySQLiteMigration(db, name, string(data)); err != nil {
		t.Fatalf("apply %s: %v", name, err)
	}
	for id, want := range map[string]sql.NullString{
		"i-garbage":    {String: `{}`, Valid: true},
		"i-empty":      {String: `{}`, Valid: true},
		"i-null":       {},
		"i-wrongshape": {String: `[]`, Valid: true},
		"i-valid":      {String: `{"k":"v"}`, Valid: true},
		"i-slug":       {String: `{"invocation_slug":"x"}`, Valid: true},
	} {
		var got sql.NullString
		if err := db.QueryRow(`SELECT fields FROM items WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("query item %s: %v", id, err)
		}
		if got != want {
			t.Errorf("item %s fields = %+v, want %+v", id, got, want)
		}
	}
	var indexName string
	if err := db.QueryRow(`SELECT name FROM sqlite_master
		WHERE type='index' AND name='idx_items_invocation_slug_per_collection'`).Scan(&indexName); err != nil {
		t.Fatalf("invocation_slug index missing after 054: %v", err)
	}
	const ts = "2026-01-01T00:00:00Z"
	if _, err := db.Exec(
		`INSERT INTO items (id, workspace_id, collection_id, title, slug, fields, created_at, updated_at)
		 VALUES ('i-slug-dup', 'w1', 'c1', 'Duplicate', 'i-slug-dup', ?, ?, ?)`,
		`{"invocation_slug":"x"}`, ts, ts,
	); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Errorf("duplicate invocation_slug insert error = %v, want UNIQUE constraint violation", err)
	}
}
