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
// the LAST migration after which it appeared having been absent just before.
//
// Last, not first (codex round 1): a column dropped and re-added, or a table
// renamed away and replaced under its old name, is introduced again, and a
// trigger file between the two appearances would name a column that did not
// exist when it ran.
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
	migrations, err := readMigrationNames(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	steps := make([]chainStep, 0, len(migrations))
	for _, name := range migrations {
		data, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		steps = append(steps, chainStep{name, string(data)})
	}
	return columnIntroductionsOf(t, steps), migrations
}

type chainStep struct{ name, sql string }

func columnIntroductionsOf(t *testing.T, steps []chainStep) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	intro := map[string]string{}
	prev := map[string]bool{}
	for _, step := range steps {
		name := step.name
		if err := applySQLiteMigration(db, name, step.sql); err != nil {
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
		now := map[string]bool{}
		for rows.Next() {
			var table, col string
			if err := rows.Scan(&table, &col); err != nil {
				t.Fatalf("scan: %v", err)
			}
			key := table + "." + col
			now[key] = true
			if !prev[key] {
				intro[key] = name
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("rows after %s: %v", name, err)
		}
		prev = now
	}
	// Report only what exists at the end of the chain.
	for key := range intro {
		if !prev[key] {
			delete(intro, key)
		}
	}
	return intro
}

// TestColumnIntroductionsTakesTheLatestAppearance checks the instrument itself
// on a synthetic chain, because the real chain has no column that disappears
// and comes back, so it could not show the rule going red. A column dropped
// and re-added, and a table renamed away and replaced under its old name, are
// both introduced again at the later step.
func TestColumnIntroductionsTakesTheLatestAppearance(t *testing.T) {
	t.Parallel()
	intro := columnIntroductionsOf(t, []chainStep{
		{"001_a.sql", "CREATE TABLE t (id TEXT, c TEXT); CREATE TABLE u (id TEXT, d TEXT);"},
		{"002_a.sql", "ALTER TABLE t DROP COLUMN c; ALTER TABLE u RENAME TO u_old;"},
		{"003_a.sql", "ALTER TABLE t ADD COLUMN c TEXT; CREATE TABLE u (id TEXT, d TEXT);"},
	})
	for key, want := range map[string]string{
		"t.id":    "001_a.sql",
		"t.c":     "003_a.sql",
		"u.d":     "003_a.sql",
		"u_old.d": "002_a.sql",
	} {
		if got := intro[key]; got != want {
			t.Errorf("%s: introduced at %q, want %q", key, got, want)
		}
	}
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

	for _, tr := range []string{"pad_nul094_items_lease_holder_ins", "pad_nul094_items_lease_holder_upd"} {
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
	have, err := nulTriggersIn(s.db, nulTriggerMigrations)
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

// TestNULTriggerRestorationByAnOlderBinaryKeepsLaterFiles runs the restore as a
// binary that knows only 084 would, against a database carrying 094's
// triggers. That binary refuses to start on a schema ahead of it unless forced,
// and a forced one must not destroy the newer triggers (codex round 1): they
// are the protection Layer B exists to keep in force while an older binary
// writes the file.
//
// One 084 trigger is dropped so the older restore actually runs, rather than
// passing because it had nothing to do.
func TestNULTriggerRestorationByAnOlderBinaryKeepsLaterFiles(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("Layer B is SQLite-only")
	}
	later := renderedNULTriggers(nulTriggerMigrations[1:])
	if len(later) == 0 {
		t.Fatal("no later-file triggers; the fixture proves nothing")
	}
	if _, err := s.db.Exec(`DROP TRIGGER pad_nul_items_content_upd`); err != nil {
		t.Fatalf("drop an 084 trigger: %v", err)
	}

	restored, err := s.ensureNULTriggersFor(nulTriggerMigrations[:1])
	if err != nil {
		t.Fatalf("restore as an older binary: %v", err)
	}
	if !restored {
		t.Fatal("the older binary's restore did not run, so the check below is vacuous")
	}
	have, err := nulTriggersIn(s.db, nulTriggerMigrations)
	if err != nil {
		t.Fatalf("list triggers: %v", err)
	}
	for name := range later {
		if _, ok := have[name]; !ok {
			t.Errorf("an older binary's restore dropped %s", name)
		}
	}
}
