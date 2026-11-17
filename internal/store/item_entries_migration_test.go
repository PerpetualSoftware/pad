package store

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const itemEntriesMigration = "184_item_entries_search.sql"
const itemEntriesNULMigration = "185_nul_invariant_triggers_post184.sql"
const itemEntriesPGMigration = "144_item_entries_search.sql"

func itemEntriesSQL(t *testing.T, pg bool) string {
	t.Helper()
	fs, path := migrationsFS, "migrations/"+itemEntriesMigration
	if pg {
		fs, path = pgMigrationsFS, "pgmigrations/"+itemEntriesPGMigration
	}
	b, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// itemEntriesSeedFields are the blobs a backfill must survive: well-formed
// logs, malformed ones, and fields that are not an object.
var itemEntriesSeedFields = []struct {
	fields string
	want   []itemEntryRow // nil: the Go extraction is the expectation
}{
	{fields: entriesFixtureFields},
	{fields: `{"status":"open","implementation_notes":[{"id":"x","summary":"only"}]}`},
	{fields: `{"status":"open"}`},
	{fields: `{"implementation_notes":"text","decision_log":[1,{"id":"d","decision":"kept","rationale":7}]}`, want: []itemEntryRow{{"decision", 1, "d", "kept", ""}}},
	{fields: `[{"implementation_notes":[{"id":"x"}]}]`, want: []itemEntryRow{}},
}

func TestItemEntriesMigration_Backfill(t *testing.T) {
	t.Parallel()
	db, _ := preMigrationDB(t, "184")
	s := &Store{db: db, dialect: &sqliteDialect{}}
	ws := createTestWorkspace(t, s, "Entries migration")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	var ids []string
	for i, seed := range itemEntriesSeedFields {
		it := createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("seed %d", i), "")
		s.mustExec(t, `UPDATE items SET fields = ? WHERE id = ?`, seed.fields, it.ID)
		ids = append(ids, it.ID)
	}
	// Fields that are not JSON at all cannot be stored: the expression index
	// on json_extract(fields) from migration 054 refuses them, and 056
	// repaired any that predated it. The JSON-but-not-an-object case is the
	// last seed.

	if err := applySQLiteMigration(s.db, itemEntriesMigration, itemEntriesSQL(t, false)); err != nil {
		t.Fatal(err)
	}
	b, err := migrationsFS.ReadFile("migrations/" + itemEntriesNULMigration)
	if err != nil {
		t.Fatal(err)
	}
	if err := applySQLiteMigration(s.db, itemEntriesNULMigration, string(b)); err != nil {
		t.Fatal(err)
	}
	for i, seed := range itemEntriesSeedFields {
		want := seed.want
		if want == nil {
			want = goItemEntries(seed.fields)
		}
		if got := storedItemEntries(t, s, ids[i]); !reflect.DeepEqual(got, want) {
			t.Errorf("seed %d: rows %+v, want %+v", i, got, want)
		}
	}
	s.mustExec(t, `INSERT INTO item_entries_fts(item_entries_fts) VALUES('integrity-check')`)
	if n := s.countRows(t, `SELECT COUNT(*) FROM item_entries_fts WHERE item_entries_fts MATCH 'embedded'`); n != 1 {
		t.Errorf("backfilled rationale not in the FTS index: %d", n)
	}
	// The items triggers are live after the migration.
	appendTestNote(t, s, ids[2], "after migration", "")
	assertEntriesMatchGo(t, s, ids[2], 1)
	if _, ok := slowSQLiteMigrations[itemEntriesMigration]; !ok {
		t.Error("the backfill is not announced in slowSQLiteMigrations")
	}
}

func TestItemEntriesMigration_PGLockTimeoutDeclared(t *testing.T) {
	t.Parallel()
	body := itemEntriesSQL(t, true)
	if !strings.HasPrefix(body, "SET LOCAL lock_timeout = '5s';\n") {
		t.Fatal("missing transaction-local lock timeout")
	}
	url := os.Getenv("PAD_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set; header checked")
	}
	s := testStorePostgres(t, url)
	for i := 0; i < 2; i++ {
		if err := applyPostgresMigration(s.db, fmt.Sprintf("999_item_entries_repeat_%d.sql", i), body); err != nil {
			t.Fatal(err)
		}
	}
}

// dropItemEntriesPG removes everything migration 144 created, leaving the
// database as it was before it.
func dropItemEntriesPG(t *testing.T, s *Store) {
	t.Helper()
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS items_entries_insert ON items`,
		`DROP TRIGGER IF EXISTS items_entries_update ON items`,
		`DROP TABLE IF EXISTS item_entries CASCADE`,
		`DROP FUNCTION IF EXISTS items_entries_sync_trigger()`,
		`DROP FUNCTION IF EXISTS item_entries_sync(TEXT, JSONB)`,
		`DROP FUNCTION IF EXISTS item_entries_source(JSONB)`,
		`DROP FUNCTION IF EXISTS item_entries_search_vector_update()`,
	} {
		s.mustExec(t, stmt)
	}
}

func TestItemEntriesMigration_PostgresBackfillAndRepeat(t *testing.T) {
	t.Parallel()
	url := os.Getenv("PAD_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	s := testStorePostgres(t, url)
	dropItemEntriesPG(t, s)
	ws := createTestWorkspace(t, s, "Entries PG migration")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	var ids []string
	for i, seed := range itemEntriesSeedFields {
		it := createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("seed %d", i), "")
		s.mustExec(t, `UPDATE items SET fields = ? WHERE id = ?`, seed.fields, it.ID)
		ids = append(ids, it.ID)
	}
	body := itemEntriesSQL(t, true)
	for i := 0; i < 2; i++ {
		if err := applyPostgresMigration(s.db, fmt.Sprintf("999_item_entries_backfill_%d.sql", i), body); err != nil {
			t.Fatal(err)
		}
		for j, seed := range itemEntriesSeedFields {
			want := seed.want
			if want == nil {
				want = goItemEntries(seed.fields)
			}
			if got := storedItemEntries(t, s, ids[j]); !reflect.DeepEqual(got, want) {
				t.Errorf("run %d seed %d: rows %+v, want %+v", i, j, got, want)
			}
		}
	}
	appendTestNote(t, s, ids[2], "after migration", "")
	assertEntriesMatchGo(t, s, ids[2], 1)
}

// seedItemEntriesLoad inserts n items with m notes each in one statement
// (Postgres only), for timing the backfill (TASK-4491 condition 3). The SQL is
// the recipe the report documents; it bypasses the store on purpose, so a
// large n seeds in seconds.
func seedItemEntriesLoad(t *testing.T, s *Store, workspaceID, collectionID string, n, m int) {
	t.Helper()
	s.mustExec(t, `
		INSERT INTO items (id, workspace_id, collection_id, title, slug, fields, created_at, updated_at)
		SELECT 'seed-' || g, ?, ?, 'seed ' || g, 'seed-' || g,
		       jsonb_build_object('status', 'open', 'implementation_notes',
		           (SELECT jsonb_agg(jsonb_build_object('id', 'note-' || g || '-' || k,
		                'summary', 'summary ' || k || ' of item ' || g,
		                'details', 'details for note ' || k || ' of seeded item ' || g || ' with some ordinary words'))
		            FROM generate_series(1, ?) k)),
		       '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'
		FROM generate_series(1, ?) g`, workspaceID, collectionID, m, n)
}

// The backfill is one statement, so its cost is one measurement: seed, empty
// the index (the recovery recipe in the migration header), and time the
// backfill INSERT. Small by default; PAD_ITEM_ENTRIES_SEED_ITEMS and
// PAD_ITEM_ENTRIES_SEED_NOTES scale it for a real timing.
func TestItemEntriesMigration_PostgresBackfillTiming(t *testing.T) {
	t.Parallel()
	url := os.Getenv("PAD_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	envInt := func(key string, def int) int {
		if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
			return v
		}
		return def
	}
	n, m := envInt("PAD_ITEM_ENTRIES_SEED_ITEMS", 200), envInt("PAD_ITEM_ENTRIES_SEED_NOTES", 3)
	s := testStorePostgres(t, url)
	ws := createTestWorkspace(t, s, "Entries timing")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	seedItemEntriesLoad(t, s, ws.ID, col.ID, n, m)
	s.mustExec(t, `TRUNCATE item_entries`)
	start := time.Now()
	s.mustExec(t, itemEntriesPGBackfill)
	t.Logf("backfill of %d items x %d notes: %s", n, m, time.Since(start))
	if got := s.countRows(t, `SELECT COUNT(*) FROM item_entries e JOIN items i ON i.id = e.item_id WHERE i.workspace_id = ?`, ws.ID); got != n*m {
		t.Errorf("backfilled %d rows, want %d", got, n*m)
	}
	res, err := s.Search(SearchParams{Query: "seeded", Workspace: ws.Slug, Unrestricted: true, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != n {
		t.Errorf("seeded notes searchable on %d items, want %d", res.Total, n)
	}
}

// itemEntriesPGBackfill is the backfill statement of pgmigrations/144,
// verbatim. TestItemEntriesPGBackfillIsTheMigrationStatement keeps the two
// equal.
const itemEntriesPGBackfill = `INSERT INTO item_entries (item_id, kind, entry_index, entry_id, head, body)
SELECT i.id, s.kind, s.entry_index, s.entry_id, s.head, s.body
FROM items i, LATERAL item_entries_source(i.fields) s
ON CONFLICT (item_id, kind, entry_index) DO NOTHING;`

func TestItemEntriesPGBackfillIsTheMigrationStatement(t *testing.T) {
	t.Parallel()
	body := itemEntriesSQL(t, true)
	if strings.Count(strings.ReplaceAll(body, "\n--   ", "\n"), itemEntriesPGBackfill) != 2 {
		t.Errorf("pgmigrations/%s must carry the backfill statement twice (the recovery header and the body)", itemEntriesPGMigration)
	}
	for _, col := range []string{"head", "body"} {
		if cap := fmt.Sprintf("left(NEW.%s, %d)", col, itemEntriesTSVectorCap); !strings.Contains(body, cap) {
			t.Errorf("the search_vector trigger must cap its input: %s", cap)
		}
	}
	if !strings.Contains(body, "TRUNCATE item_entries;") {
		t.Error("the recovery header must name how to empty the index")
	}
	sqlite := itemEntriesSQL(t, false)
	if !strings.Contains(sqlite, "DELETE FROM item_entries;") || !strings.Contains(sqlite, "must recreate") {
		t.Error("the SQLite header must carry the recovery recipe and the rebuild warning")
	}
}

// Postgres leg of condition 1 against the pre-cap input size: the trigger's
// own to_tsvector must not raise on the largest note the API accepts. Runs
// through testStore in TestItemEntriesLargeNoteIsIndexed; this asserts the
// cap is what makes it pass, by showing the uncapped expression raises.
func TestItemEntriesPGUncappedTSVectorRaises(t *testing.T) {
	t.Parallel()
	url := os.Getenv("PAD_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	s := testStorePostgres(t, url)
	big := largeDistinctWords(2<<20 - 4096)
	var n int
	if err := s.db.QueryRow(s.q(`SELECT length(to_tsvector('english', ?::text))`), big).Scan(&n); err == nil {
		t.Skipf("uncapped to_tsvector accepted %d bytes (%d lexemes); the cap is belt and braces here", len(big), n)
	} else if !strings.Contains(err.Error(), "too long") {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := s.db.QueryRow(s.q(`SELECT length(to_tsvector('english', left(?::text, `+strconv.Itoa(itemEntriesTSVectorCap)+`)))`), big).Scan(&n); err != nil {
		t.Errorf("the capped expression raises too: %v", err)
	}
}

// itemEntriesTSVectorCap mirrors the cap in pgmigrations/144.
const itemEntriesTSVectorCap = 65536
