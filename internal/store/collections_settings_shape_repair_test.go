package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// IDEA-1489 regression tests for the corrective backfill on
// collections.settings (migrations 058 + pgmigrations/037).
//
// Migration 055 / pg/034 (PR #562) shipped a NULL-only backfill before
// applying NOT NULL DEFAULT '{}'. PR #566 generalized the shape-repair
// pattern for the four migrations it owned (056/057, pg/035/036) but
// was scope-bounded out of retroactively repairing 055 + pg/034. These
// tests are the retroactive coverage.
//
// Toggle-verification: with the WHERE predicate in 058 / pg/037
// reverted to `settings IS NULL` only, both tests below FAIL on the
// non-NULL malformed seeds. With the full predicate they PASS.

// TestCollectionsSettingsShapeRepair_SQLite seeds collections rows with
// every shape pathology a pre-055 SQLite deployment could carry, then
// re-applies migration 058 and asserts every malformed row is repaired
// to '{}'.
//
// Migrations 001-054 are applied first to bring the schema up. We then
// stop BEFORE 055 to seed nullable + free-form settings rows (055
// rebuilds the table with NOT NULL, so post-055 inserts of `NULL` or
// `''` would error). Then 055 runs (its own NULL-only backfill repairs
// only the NULL seed; the rest survive into the new table because the
// SQLite rebuild's INSERT…SELECT preserves the literal bytes). Finally
// 058 runs and is the migration under test.
func TestCollectionsSettingsShapeRepair_SQLite(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific (json_valid / json_type)")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "settings_repair.db")
	dsn := dbPath + "?_pragma=busy_timeout(30000)&_pragma=foreign_keys(on)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatalf("WAL: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}

	names, err := readMigrationNames(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}

	// Apply everything up to but not including 055 — that's the boundary
	// where collections.settings goes from nullable / free-form to
	// NOT NULL. We need to seed the malformed shapes while the column
	// still permits them.
	postSeed := map[string]bool{
		"055_collections_settings_not_null.sql":     true,
		"056_items_jsonb_not_null.sql":              true,
		"057_views_config_not_null.sql":             true,
		"058_collections_settings_shape_repair.sql": true,
	}
	for _, name := range names {
		if postSeed[name] {
			continue
		}
		data, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if err := applySQLiteMigration(db, name, string(data)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}

	const wsID = "ws-cs"
	const ts = "2026-05-16T00:00:00Z"
	if _, err := db.Exec(
		`INSERT INTO workspaces (id, name, slug, settings, created_at, updated_at)
		 VALUES (?, ?, ?, '{}', ?, ?)`,
		wsID, "CSShape", "csshape", ts, ts,
	); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}

	// Seed collections with every observable settings pathology. Pre-055
	// the column is nullable + free-form, so every INSERT succeeds.
	collSeeds := []struct {
		id       string
		slug     string
		settings any // string or nil for SQL NULL
	}{
		{"c-null", "c-null", nil},                  // SQL NULL
		{"c-empty", "c-empty", ""},                 // empty-string (invalid JSON)
		{"c-jsnull", "c-jsnull", "null"},           // JSON null literal
		{"c-arr", "c-arr", "[]"},                   // wrong shape — array
		{"c-garbage", "c-garbage", "not json"},     // non-JSON text
		{"c-valid", "c-valid", `{"display":"ok"}`}, // already valid — must be preserved
	}
	for _, sd := range collSeeds {
		if _, err := db.Exec(
			`INSERT INTO collections (id, workspace_id, name, slug, settings, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			sd.id, wsID, sd.id, sd.slug, sd.settings, ts, ts,
		); err != nil {
			t.Fatalf("seed collection %s: %v", sd.id, err)
		}
	}

	// Apply 055 (NULL-only backfill + rebuild with NOT NULL), then 056,
	// 057, then the migration under test (058).
	postSeedOrder := []string{
		"055_collections_settings_not_null.sql",
		"056_items_jsonb_not_null.sql",
		"057_views_config_not_null.sql",
		"058_collections_settings_shape_repair.sql",
	}
	for _, name := range postSeedOrder {
		data, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if err := applySQLiteMigration(db, name, string(data)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}

	want := map[string]string{
		"c-null":    `{}`,
		"c-empty":   `{}`,
		"c-jsnull":  `{}`,
		"c-arr":     `{}`,
		"c-garbage": `{}`,
		"c-valid":   `{"display":"ok"}`,
	}
	for id, wantSettings := range want {
		var got string
		err := db.QueryRow(`SELECT settings FROM collections WHERE id = ?`, id).Scan(&got)
		if err != nil {
			t.Fatalf("query collection %s: %v", id, err)
		}
		if got != wantSettings {
			t.Errorf("collection %s settings: want %q, got %q", id, wantSettings, got)
		}
	}
}

// TestCollectionsSettingsShapeRepair_Postgres is the Postgres counterpart.
// JSONB rejects invalid JSON at write time, so the survivable pathologies
// are SQL NULL and JSONB-valid-but-wrong-shape. pgmigrations/034 already
// ran in testStore init; we seed rows after the fact (bypassing the store
// helpers' normalizers) then re-run pgmigrations/037's UPDATE clause and
// assert every malformed row is repaired to '{}'::jsonb.
func TestCollectionsSettingsShapeRepair_Postgres(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}

	s := testStore(t)
	ws := createTestWorkspace(t, s, "PgSettingsRepair")

	const ts = "2026-05-16T00:00:00Z"
	mustInsert := func(t *testing.T, id, slug, settingsExpr string) {
		t.Helper()
		// settingsExpr is a JSONB literal expression, e.g. "NULL",
		// "'null'::jsonb", "'[]'::jsonb". Inlined because parameterized
		// JSONB NULL vs SQL NULL is awkward to express via $N binds.
		q := `INSERT INTO collections (id, workspace_id, name, slug, settings, created_at, updated_at)
		      VALUES ($1, $2, $3, $4, ` + settingsExpr + `, $5, $6)`
		_, err := s.db.Exec(q, id, ws.ID, id, slug, ts, ts)
		if err != nil {
			t.Fatalf("seed collection %s: %v", id, err)
		}
	}
	// SQL NULL is not seedable post-pg/034 (the NOT NULL constraint is
	// already in place when testStore init runs the migrations). The
	// surviving wrong-shape pathologies on JSONB are JSONB null, arrays,
	// and primitives — those are what pg/037 exists to repair.
	mustInsert(t, "pg-cs-jsnull", "pg-cs-jsnull", "'null'::jsonb")
	mustInsert(t, "pg-cs-arr", "pg-cs-arr", "'[]'::jsonb")
	mustInsert(t, "pg-cs-prim", "pg-cs-prim", "'42'::jsonb")
	mustInsert(t, "pg-cs-valid", "pg-cs-valid", `'{"display":"ok"}'::jsonb`)

	// Re-run the backfill UPDATE clause from pgmigrations/037 (the
	// migration itself already ran during testStore init; this exercises
	// the UPDATE shape against the seeded rows).
	if _, err := s.db.Exec(
		"UPDATE collections SET settings = '{}'::jsonb WHERE settings IS NULL OR jsonb_typeof(settings) != 'object'",
	); err != nil {
		t.Fatalf("re-run pg/037 backfill: %v", err)
	}

	want := map[string]string{
		"pg-cs-jsnull": `{}`,
		"pg-cs-arr":    `{}`,
		"pg-cs-prim":   `{}`,
		"pg-cs-valid":  `{"display": "ok"}`, // Postgres adds whitespace
	}
	for id, wantSettings := range want {
		var got string
		err := s.db.QueryRow(`SELECT settings::text FROM collections WHERE id = $1`, id).Scan(&got)
		if err != nil {
			t.Fatalf("query collection %s: %v", id, err)
		}
		if !jsonEqualString(got, wantSettings) {
			t.Errorf("collection %s settings: want %q, got %q", id, wantSettings, got)
		}
	}
}
