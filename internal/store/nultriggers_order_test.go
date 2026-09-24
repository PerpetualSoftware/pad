package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// columnIntroductions applies the SQLite migration chain one file at a time to
// a fresh raw database and records, for every column that exists at the end,
// the migration after which it FIRST appeared.
//
// It is the instrument BUG-3108's ordering rule is checked against, and it is
// derived from the chain rather than written down, so a column cannot be
// assigned to a trigger file by a claim about when it arrived: the chain says.
//
// Raw driver, not the store: nothing here writes caller data, and the store's
// own migrate() interleaves the trigger re-assertion this instrument exists to
// check the inputs of.
func columnIntroductions(t *testing.T) (intro map[string]string, migrations []string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	migrations, err = readMigrationNames(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	intro = map[string]string{}
	for _, name := range migrations {
		data, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := applySQLiteMigration(db, name, string(data)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		rows, err := db.Query(`
			SELECT m.name, ti.name
			FROM sqlite_master m, pragma_table_info(m.name) ti
			WHERE m.type = 'table'
		`)
		if err != nil {
			t.Fatalf("columns after %s: %v", name, err)
		}
		for rows.Next() {
			var table, col string
			if err := rows.Scan(&table, &col); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if _, seen := intro[table+"."+col]; !seen {
				intro[table+"."+col] = name
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("rows after %s: %v", name, err)
		}
	}
	return intro, migrations
}

// TestNULTriggerFilesFollowTheirColumns is BUG-3108's ordering rule: every
// protected column's triggers live in a trigger file that runs AFTER the
// migration that introduced the column.
//
// The next column added to the list lands in 084 by default, and 084 predates
// it, so this is what fails, naming the migration to follow. Without it the
// only way through was the unprotected baseline, where a column recorded
// because the generator could not reach it reads exactly like one recorded
// because it carries no caller text.
func TestNULTriggerFilesFollowTheirColumns(t *testing.T) {
	t.Parallel()
	intro, migrations := columnIntroductions(t)

	shipped := map[string]bool{}
	for _, m := range migrations {
		shipped[m] = true
	}
	for i, f := range nulTriggerMigrations {
		if !shipped[f] {
			t.Errorf("trigger file %s is not in the migrations directory; regenerate with "+
				"GEN_NUL_TRIGGERS=1 go test ./internal/store/ -run TestGenerateNULTriggerMigration", f)
		}
		if i > 0 && f <= nulTriggerMigrations[i-1] {
			t.Errorf("nulTriggerMigrations is out of migration order at %s", f)
		}
	}

	protected := map[string]bool{}
	for _, c := range NULProtectedColumns() {
		protected[c.Table+"."+c.Column] = true
	}
	var keys []string
	for k := range nulColumnTriggerFile {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !protected[k] {
			t.Errorf("nulColumnTriggerFile assigns %s, which is not a protected column", k)
		}
		if f := nulColumnTriggerFile[k]; !slices.Contains(nulTriggerMigrations, f) {
			t.Errorf("nulColumnTriggerFile assigns %s to %s, which is not in nulTriggerMigrations", k, f)
		}
	}

	checked := 0
	for _, c := range NULProtectedColumns() {
		key := c.Table + "." + c.Column
		introduced, ok := intro[key]
		if !ok {
			// Absent from the schema entirely; TestNULColumnCensus reports it.
			continue
		}
		checked++
		file := nulTriggerFileFor(c)
		if file <= introduced {
			t.Errorf("%s is introduced by %s, but its triggers are in %s, which runs first. Append a new "+
				"trigger file numbered after %s to nulTriggerMigrations, map the column to it in "+
				"nulColumnTriggerFile, and regenerate. Never add it to a file that has shipped (BUG-3108).",
				key, introduced, file, introduced)
		}
	}
	// The instrument must have seen the population, or a clean pass is a
	// statement about the walk.
	if checked != len(NULProtectedColumns()) {
		t.Fatalf("checked only %d of %d protected columns; the chain walk is broken", checked, len(NULProtectedColumns()))
	}
}

// TestNULTriggerRestorationCoversLaterFiles drops a post-084 trigger pair and
// runs the startup re-assertion. The restore has to know about every applied
// trigger file, not only 084, or a table rebuild takes a later file's triggers
// away for good (BUG-3108).
//
// Protection is shown gone before the restore and back after it, on the same
// raw writer, so the restore is measured against a real loss.
func TestNULTriggerRestorationCoversLaterFiles(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("Layer B is SQLite-only")
	}
	ws := createTestWorkspace(t, s, "LaterFileWS")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Later file subject", "")

	for _, tr := range []string{"pad_nul_items_lease_holder_ins", "pad_nul_items_lease_holder_upd"} {
		// No IF EXISTS: the fixture needs the trigger to exist.
		if _, err := s.db.Exec("DROP TRIGGER " + tr); err != nil {
			t.Fatalf("drop %s: %v", tr, err)
		}
	}

	raw, err := sql.Open("sqlite", s.dbPath+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE items SET lease_holder = ? WHERE id = ?`, "gone"+textguard.NUL+"now", item.ID); err != nil {
		t.Fatalf("with the triggers dropped the write should succeed, showing the loss is real: %v", err)
	}
	if _, err := raw.Exec(`UPDATE items SET lease_holder = NULL WHERE id = ?`, item.ID); err != nil {
		t.Fatalf("reset: %v", err)
	}

	restored, err := s.ensureNULTriggersReporting()
	if err != nil {
		t.Fatalf("ensureNULTriggersReporting: %v", err)
	}
	if !restored {
		t.Fatal("the re-assertion saw nothing missing after a post-084 trigger pair was dropped")
	}
	_, err = raw.Exec(`UPDATE items SET lease_holder = ? WHERE id = ?`, "back"+textguard.NUL+"again", item.ID)
	if err == nil || !strings.Contains(err.Error(), nulTriggerMarker) {
		t.Fatalf("after the restore a raw NUL in items.lease_holder must be refused by our trigger, got %v", err)
	}
}

// TestNULTriggerRestorationSkipsUnappliedFiles puts a database back in the
// state an upgrade passes through: 084 applied and a later trigger file not yet
// applied. A restore in that window must re-create 084's triggers and none of
// the later file's, because the later file's columns may not exist yet.
//
// The window is reached by unrecording 094 and dropping its triggers, with one
// 084 trigger dropped too so the restore actually runs. Without that, "nothing
// was re-created" would hold because nothing was attempted.
func TestNULTriggerRestorationSkipsUnappliedFiles(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("Layer B is SQLite-only")
	}
	later := nulTriggerMigrations[len(nulTriggerMigrations)-1]
	laterTriggers := renderedNULTriggers([]string{later})
	if len(laterTriggers) == 0 {
		t.Fatalf("%s renders no triggers; the fixture proves nothing", later)
	}

	if _, err := s.db.Exec(`DELETE FROM schema_migrations WHERE version = ?`, later); err != nil {
		t.Fatalf("unrecord %s: %v", later, err)
	}
	for name := range laterTriggers {
		if _, err := s.db.Exec(`DROP TRIGGER "` + name + `"`); err != nil {
			t.Fatalf("drop %s: %v", name, err)
		}
	}
	if _, err := s.db.Exec(`DROP TRIGGER pad_nul_items_content_upd`); err != nil {
		t.Fatalf("drop an 084 trigger: %v", err)
	}

	restored, err := s.ensureNULTriggersReporting()
	if err != nil {
		t.Fatalf("ensureNULTriggersReporting: %v", err)
	}
	if !restored {
		t.Fatal("the dropped 084 trigger was not restored, so the restore never ran and the check below is vacuous")
	}
	have, err := nulTriggersIn(s.db)
	if err != nil {
		t.Fatalf("list triggers: %v", err)
	}
	if _, ok := have["pad_nul_items_content_upd"]; !ok {
		t.Error("the restore did not re-create the dropped 084 trigger")
	}
	for name := range laterTriggers {
		if _, ok := have[name]; ok {
			t.Errorf("the restore created %s from %s, which is not applied", name, later)
		}
	}
}
