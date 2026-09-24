package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const migration095 = "095_rebuild_items_fts.sql"

// BUG-2758. Migration 095 repairs the duplicate items_fts postings a workspace
// import used to leave behind. The fixture reproduces that state the way the
// removed pass produced it, a second INSERT of an already-indexed row, and the
// control shows the fixture is really malformed before the repair runs.
func TestMigration095RepairsDoubleIndexedItems(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	skipUnlessSQLite(t, s)

	ws, err := s.ImportWorkspace(ftsImportBundle("the numbatword lives here"), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`
		INSERT INTO items_fts(rowid, title, content, tags)
		SELECT rowid, title, content, tags FROM items WHERE workspace_id = ?`, ws.ID); err != nil {
		t.Fatal(err)
	}
	if err := ftsIntegrity(t, s); err == nil {
		t.Fatal("control: the double-indexed fixture passes integrity-check, so it cannot show the repair works")
	}

	reapply095(t, s)
	if err := ftsIntegrity(t, s); err != nil {
		t.Fatalf("items_fts integrity-check after %s: %v", migration095, err)
	}
	hits, err := s.SearchItems(ws.ID, "numbatword")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("search after the repair: %d hits, want 1", len(hits))
	}
}

// reapply095 runs migration 095 again through the real runner
// (applySQLiteMigration: its own transaction, and the schema_migrations row),
// after forgetting that the store's construction already applied it.
func reapply095(t *testing.T, s *Store) {
	t.Helper()
	sqlText, err := migrationsFS.ReadFile("migrations/" + migration095)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM schema_migrations WHERE version = ?`, migration095); err != nil {
		t.Fatal(err)
	}
	if err := applySQLiteMigration(s.db, migration095, string(sqlText)); err != nil {
		t.Fatalf("apply %s: %v", migration095, err)
	}
}

// A database that lost one of the three triggers (a state
// validateFTSInvariants only warns about) is maintained again after 095: a
// rebuild alone would repair the index once and then let it drift.
func TestMigration095RestoresAMissingTrigger(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	skipUnlessSQLite(t, s)

	ws, err := s.ImportWorkspace(ftsImportBundle("the bilbyword lives here"), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER items_fts_update`); err != nil {
		t.Fatal(err)
	}
	reapply095(t, s)

	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND name = 'items_fts_update'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("items_fts_update after %s: %d, want 1", migration095, n)
	}
	item, err := s.GetItemBySlug(ws.ID, "first-doc")
	if err != nil || item == nil {
		t.Fatalf("imported item: %v, %v", item, err)
	}
	replaced := "the body now says platypusword"
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &replaced}); err != nil {
		t.Fatal(err)
	}
	if err := ftsIntegrity(t, s); err != nil {
		t.Fatalf("items_fts integrity-check after an edit: %v", err)
	}
	for word, want := range map[string]int{"bilbyword": 0, "platypusword": 1} {
		hits, err := s.SearchItems(ws.ID, word)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != want {
			t.Fatalf("search %q after the edit: %d hits, want %d", word, len(hits), want)
		}
	}
}

// A damaged database with no items_fts at all used to be a startup failure
// for a bare 'rebuild' (the migration errors and is not recorded). 095
// creates the table and indexes what is there.
func TestMigration095RecreatesAMissingTable(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	skipUnlessSQLite(t, s)

	ws, err := s.ImportWorkspace(ftsImportBundle("the dingoword lives here"), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP TRIGGER items_fts_insert`,
		`DROP TRIGGER items_fts_update`,
		`DROP TRIGGER items_fts_delete`,
		`DROP TABLE items_fts`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	reapply095(t, s)

	if err := ftsIntegrity(t, s); err != nil {
		t.Fatalf("items_fts integrity-check after %s: %v", migration095, err)
	}
	hits, err := s.SearchItems(ws.ID, "dingoword")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("search after %s: %d hits, want 1", migration095, len(hits))
	}
}

// An items_fts with ANOTHER definition is replaced rather than kept. A
// contentless table is the sharp case: FTS5 refuses 'rebuild' on one, so an
// IF NOT EXISTS create followed by a rebuild failed the migration (codex
// round 2).
func TestMigration095ReplacesAForeignDefinition(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	skipUnlessSQLite(t, s)

	ws, err := s.ImportWorkspace(ftsImportBundle("the possumword lives here"), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP TRIGGER items_fts_insert`,
		`DROP TRIGGER items_fts_update`,
		`DROP TRIGGER items_fts_delete`,
		`DROP TABLE items_fts`,
		`CREATE VIRTUAL TABLE items_fts USING fts5(title, content, tags, content='')`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	reapply095(t, s)

	var sqlText string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'items_fts'`).Scan(&sqlText); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sqlText, "content='items'") {
		t.Fatalf("items_fts after %s is not the external-content table: %s", migration095, sqlText)
	}
	if err := ftsIntegrity(t, s); err != nil {
		t.Fatalf("items_fts integrity-check after %s: %v", migration095, err)
	}
	hits, err := s.SearchItems(ws.ID, "possumword")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("search after %s: %d hits, want 1", migration095, len(hits))
	}
}

// The repair is SQLite-only by construction: it lives in the SQLite chain, and
// Postgres migrates from pgmigrations/, which does not carry it. The second
// half is behavioural and runs only against Postgres (make test-pg): a fully
// migrated Postgres store has no items_fts relation for the statement to name.
// (A text scan of pgmigrations for "items_fts" is not the instrument: the GIN
// index on search_vector is named idx_items_fts.)
func TestMigration095IsNotInThePostgresChain(t *testing.T) {
	t.Parallel()
	sqliteNames, err := readMigrationNames(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range sqliteNames {
		if n == migration095 {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s is not in the SQLite migration chain", migration095)
	}
	pgNames, err := readMigrationNames(pgMigrationsFS, "pgmigrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range pgNames {
		if n == migration095 {
			t.Fatalf("pgmigrations carries %s", n)
		}
	}

	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		return
	}
	var rel *string
	if err := s.db.QueryRow(`SELECT to_regclass('items_fts')::text`).Scan(&rel); err != nil {
		t.Fatal(err)
	}
	if rel != nil {
		t.Fatalf("a migrated Postgres store has a relation named items_fts (%s)", *rel)
	}
}

// A notice keyed by a name no migration carries would never print, and the
// map would read as a claim that the slow migration announces itself.
func TestSlowSQLiteMigrationsNameRealMigrations(t *testing.T) {
	t.Parallel()
	names, err := readMigrationNames(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for n := range slowSQLiteMigrations {
		if !have[n] {
			t.Errorf("slowSQLiteMigrations names %q, which is not a migration", n)
		}
	}
}
