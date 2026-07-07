package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGuardSchemaAhead_Detection covers the pure detection logic: a DB is
// "ahead" iff it has an applied migration that sorts after the highest
// embedded one.
func TestGuardSchemaAhead_Detection(t *testing.T) {
	embedded := []string{"001_initial.sql", "002_foo.sql", "003_bar.sql"}

	cases := []struct {
		name      string
		applied   []string
		wantError bool
	}{
		{"fresh db (nothing applied)", nil, false},
		{"in sync", []string{"001_initial.sql", "002_foo.sql", "003_bar.sql"}, false},
		{"behind (missing latest)", []string{"001_initial.sql", "002_foo.sql"}, false},
		{"ahead by one", []string{"001_initial.sql", "002_foo.sql", "003_bar.sql", "004_future.sql"}, true},
		{"ahead by several", []string{"003_bar.sql", "010_way_ahead.sql", "011_more.sql"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applied := make(map[string]bool, len(tc.applied))
			for _, v := range tc.applied {
				applied[v] = true
			}
			err := guardSchemaAhead(applied, embedded)
			if tc.wantError && err == nil {
				t.Fatalf("expected schema-ahead error, got nil")
			}
			if !tc.wantError && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tc.wantError && !strings.Contains(err.Error(), "newer than this pad binary") {
				t.Fatalf("error should explain the downgrade, got: %v", err)
			}
		})
	}
}

// TestGuardSchemaAhead_ForceOverride verifies the escape hatch: with
// AllowSchemaAhead set, an ahead DB is allowed through (warning only).
func TestGuardSchemaAhead_ForceOverride(t *testing.T) {
	prev := AllowSchemaAhead
	t.Cleanup(func() { AllowSchemaAhead = prev })

	embedded := []string{"001_initial.sql", "002_foo.sql"}
	applied := map[string]bool{"001_initial.sql": true, "002_foo.sql": true, "003_future.sql": true}

	if err := guardSchemaAhead(applied, embedded); err == nil {
		t.Fatalf("without override, expected error")
	}

	AllowSchemaAhead = true
	if err := guardSchemaAhead(applied, embedded); err != nil {
		t.Fatalf("with override, expected nil, got %v", err)
	}
}

// TestMigrate_RefusesSchemaAheadDB is the end-to-end guard: a DB carrying a
// migration this binary doesn't ship must fail New(), and must succeed once
// the override is set.
func TestMigrate_RefusesSchemaAheadDB(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific path")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ahead.db")

	// Fully migrate a fresh DB, then stamp a from-the-future migration row.
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.db.Exec(
		"INSERT INTO schema_migrations (version, applied_at) VALUES ('999_from_the_future.sql', '2099-01-01T00:00:00Z')",
	); err != nil {
		t.Fatalf("stamp future migration: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening must now be refused.
	prev := AllowSchemaAhead
	t.Cleanup(func() { AllowSchemaAhead = prev })
	AllowSchemaAhead = false

	if _, err := New(dbPath); err == nil {
		t.Fatalf("expected New() to refuse a schema-ahead DB")
	} else if !strings.Contains(err.Error(), "newer than this pad binary") {
		t.Fatalf("unexpected error: %v", err)
	}

	// With the override, it must open.
	AllowSchemaAhead = true
	s2, err := New(dbPath)
	if err != nil {
		t.Fatalf("New() with override should succeed, got %v", err)
	}
	s2.Close()
}

// TestSnapshotBeforeMigrate_WritesCopy verifies the SQLite snapshot writes a
// byte-faithful copy next to the DB, named for the binary version.
func TestSnapshotBeforeMigrate_WritesCopy(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific path")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "snap.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	prev := BinaryVersion
	t.Cleanup(func() { BinaryVersion = prev })
	BinaryVersion = "9.9.9 (deadbee)" // exercises sanitizeVersion

	s.snapshotBeforeMigrate()

	dst := dbPath + ".pre-9.9.9--deadbee-"
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("snapshot not written at %s: %v", dst, err)
	}
	if info.Size() == 0 {
		t.Fatalf("snapshot is empty")
	}

	// The snapshot must be a real SQLite DB — open it and read a known table.
	snap, err := New(dst) // migrates cleanly (already fully migrated copy)
	if err != nil {
		t.Fatalf("snapshot is not a usable DB: %v", err)
	}
	defer snap.Close()
	var n int
	if err := snap.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatalf("query snapshot: %v", err)
	}
	if n == 0 {
		t.Fatalf("snapshot has no applied migrations recorded")
	}
}

// TestSnapshotBeforeMigrate_PreservesExisting verifies a retry doesn't
// clobber the original pre-upgrade snapshot with a later (partially-migrated)
// DB state.
func TestSnapshotBeforeMigrate_PreservesExisting(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific path")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "snap.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	prev := BinaryVersion
	t.Cleanup(func() { BinaryVersion = prev })
	BinaryVersion = "1.0.0"
	dst := dbPath + ".pre-1.0.0"

	// Pre-seed a sentinel snapshot as if a prior boot already wrote it.
	if err := os.WriteFile(dst, []byte("ORIGINAL-PRE-UPGRADE-STATE"), 0o600); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	s.snapshotBeforeMigrate() // must NOT overwrite the existing snapshot

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if string(got) != "ORIGINAL-PRE-UPGRADE-STATE" {
		t.Fatalf("existing snapshot was overwritten; want sentinel, got %d bytes", len(got))
	}
}

// TestSnapshotBeforeMigrate_SkipsInMemory ensures the snapshot path is a
// no-op for a store with no on-disk file (defensive: guards the dbPath gate).
func TestSnapshotBeforeMigrate_SkipsNoFile(t *testing.T) {
	s := &Store{dialect: &sqliteDialect{}, dbPath: ""}
	s.snapshotBeforeMigrate() // must not panic; nothing to do

	// A path that doesn't exist on disk is also a silent no-op.
	s2 := &Store{dialect: &sqliteDialect{}, dbPath: filepath.Join(t.TempDir(), "does-not-exist.db")}
	s2.snapshotBeforeMigrate()
	if _, err := os.Stat(s2.dbPath + ".pre-" + sanitizeVersion(BinaryVersion)); !os.IsNotExist(err) {
		t.Fatalf("expected no snapshot for a missing DB file")
	}
}

func TestSanitizeVersion(t *testing.T) {
	cases := map[string]string{
		"dev":             "dev",
		"1.2.3":           "1.2.3",
		"v1.2.3":          "v1.2.3",
		"1.2.3 (abc1234)": "1.2.3--abc1234-",
		"  ":              "unknown",
		"weird/../path":   "weird-..-path",
	}
	for in, want := range cases {
		if got := sanitizeVersion(in); got != want {
			t.Errorf("sanitizeVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
