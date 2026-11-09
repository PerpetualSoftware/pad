package store

import (
	"os"
	"strings"
	"testing"
)

// The gated side of a gate is the ITEM, not its schedule (TASK-4353, migration
// 174 on SQLite, 136 on Postgres): an item with no commitment can be gated, and
// deleting a blocker's schedule leaves its gates status-judged instead of
// cascading them away. These tests apply the migration to a database holding
// gates in the old shape and check that nothing is lost.

const gateMigration = "174_schedule_gates_item.sql"
const gatePGMigration = "136_schedule_gates_item.sql"

func gateMigrationSQL(t *testing.T, pg bool) string {
	t.Helper()
	fs, path := migrationsFS, "migrations/"+gateMigration
	if pg {
		fs, path = pgMigrationsFS, "pgmigrations/"+gatePGMigration
	}
	b, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// oldGateTable is the table as migration 172 and Postgres 134 made it.
const oldGateTable = `CREATE TABLE schedule_gates (
    id                     TEXT PRIMARY KEY,
    workspace_id           TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    blocked_schedule_id    TEXT NOT NULL REFERENCES item_schedules(id) ON DELETE CASCADE,
    blocker_item_id        TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    blocker_schedule_id    TEXT REFERENCES item_schedules(id) ON DELETE CASCADE,
    blocker_occurrence_key TEXT NOT NULL DEFAULT '',
    opened_at              TEXT,
    created_at             TEXT NOT NULL,
    CHECK (blocker_schedule_id IS NULL OR blocker_schedule_id <> blocked_schedule_id),
    UNIQUE (blocked_schedule_id, blocker_item_id)
)`

type gateMigrationFix struct {
	ws                        string
	a, b, c, d, bare          string // items
	sa, sb, sd                string // schedules of a, b, d
	g1, g2, g3                string // gates
	firstOpened, secondOpened string
}

// seedOldGates writes three gates in the old shape: a gated by b (a commitment),
// a gated by c (no commitment, already opened), d gated by b.
func seedOldGates(t *testing.T, s *Store) gateMigrationFix {
	t.Helper()
	ws := createTestWorkspace(t, s, "Gate migration")
	col := createTestCollection(t, s, ws.ID, "Gates")
	f := gateMigrationFix{ws: ws.ID}
	item := func(title string) string { return createTestItem(t, s, ws.ID, col.ID, title, "").ID }
	f.a, f.b, f.c, f.d, f.bare = item("a"), item("b"), item("c"), item("d"), item("bare")
	ts := "2026-10-29T12:00:00Z"
	sched := func(itemID string) string {
		id := newID()
		s.mustExec(t, `INSERT INTO item_schedules (id,workspace_id,item_id,kind,dtstart,tz,floating,next_at,next_wall,created_at,updated_at) VALUES (?,?,?,'check','2030-03-04T09:00:00','UTC',?,?,?,?,?)`,
			id, ws.ID, itemID, s.dialect.BoolToInt(false), "2030-03-04T09:00:00Z", "2030-03-04T09:00:00", ts, ts)
		return id
	}
	f.sa, f.sb, f.sd = sched(f.a), sched(f.b), sched(f.d)
	f.g1, f.g2, f.g3 = newID(), newID(), newID()
	f.firstOpened, f.secondOpened = "", "2026-11-01T00:00:00Z"
	ins := `INSERT INTO schedule_gates (id,workspace_id,blocked_schedule_id,blocker_item_id,blocker_schedule_id,blocker_occurrence_key,opened_at,created_at) VALUES (?,?,?,?,?,?,?,?)`
	s.mustExec(t, ins, f.g1, ws.ID, f.sa, f.b, f.sb, "2030-03-04T09:00:00Z|2030-03-04T09:00:00", nil, ts)
	s.mustExec(t, ins, f.g2, ws.ID, f.sa, f.c, nil, "", f.secondOpened, ts)
	s.mustExec(t, ins, f.g3, ws.ID, f.sd, f.b, f.sb, "2030-03-04T09:00:00Z|2030-03-04T09:00:00", nil, ts)
	return f
}

// checkGateMigration is the assertions of both dialects, after the migration.
func checkGateMigration(t *testing.T, s *Store, f gateMigrationFix) {
	t.Helper()
	type row struct{ blocked, blocker, blockerSched, key, opened string }
	read := func() map[string]row {
		rows, err := s.db.Query(s.q(`SELECT id, blocked_item_id, blocker_item_id, COALESCE(blocker_schedule_id,''), blocker_occurrence_key, COALESCE(opened_at,'') FROM schedule_gates`))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]row{}
		for rows.Next() {
			var id string
			var r row
			if err := rows.Scan(&id, &r.blocked, &r.blocker, &r.blockerSched, &r.key, &r.opened); err != nil {
				t.Fatal(err)
			}
			out[id] = r
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	key := "2030-03-04T09:00:00Z|2030-03-04T09:00:00"
	want := map[string]row{
		f.g1: {f.a, f.b, f.sb, key, ""},
		f.g2: {f.a, f.c, "", "", f.secondOpened},
		f.g3: {f.d, f.b, f.sb, key, ""},
	}
	got := read()
	if len(got) != len(want) {
		t.Fatalf("%d gates after the migration, want %d: %v", len(got), len(want), got)
	}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("gate %s = %+v, want %+v", id, got[id], w)
		}
	}
	// The gated side is the item: one gate per gated item and blocker, and no gate on itself.
	ins := `INSERT INTO schedule_gates (id,workspace_id,blocked_item_id,blocker_item_id,blocker_schedule_id,blocker_occurrence_key,created_at) VALUES (?,?,?,?,?,?,?)`
	if _, err := s.db.Exec(s.q(ins), newID(), f.ws, f.a, f.b, nil, "", "2026-11-02T00:00:00Z"); err == nil {
		t.Fatal("a second gate for the same gated item and blocker was accepted")
	}
	if _, err := s.db.Exec(s.q(ins), newID(), f.ws, f.a, f.a, nil, "", "2026-11-02T00:00:00Z"); err == nil {
		t.Fatal("a gate on itself was accepted")
	}
	// An item with no commitment can be gated.
	if _, err := s.db.Exec(s.q(ins), newID(), f.ws, f.bare, f.b, nil, "", "2026-11-02T00:00:00Z"); err != nil {
		t.Fatalf("a gate on an item with no commitment: %v", err)
	}
	// Deleting a blocker's schedule leaves its gates, status-judged.
	s.mustExec(t, `DELETE FROM item_schedules WHERE id=?`, f.sb)
	got = read()
	if len(got) != 4 || got[f.g1].blockerSched != "" || got[f.g3].blockerSched != "" || got[f.g1].blocker != f.b {
		t.Fatalf("after the blocker schedule was deleted: %v", got)
	}
	// Deleting the gated item's schedule leaves its gates, too.
	s.mustExec(t, `DELETE FROM item_schedules WHERE id=?`, f.sa)
	if n := s.countRows(t, `SELECT COUNT(*) FROM schedule_gates WHERE blocked_item_id=?`, f.a); n != 2 {
		t.Fatalf("%d gates on a after its schedule was deleted, want 2", n)
	}
	// The gated item or the blocker item going cascades its gates.
	s.mustExec(t, `DELETE FROM items WHERE id=?`, f.a)
	if n := s.countRows(t, `SELECT COUNT(*) FROM schedule_gates WHERE blocked_item_id=?`, f.a); n != 0 {
		t.Fatalf("%d gates left for a deleted gated item", n)
	}
	s.mustExec(t, `DELETE FROM items WHERE id=?`, f.b)
	if n := s.countRows(t, `SELECT COUNT(*) FROM schedule_gates WHERE blocker_item_id=?`, f.b); n != 0 {
		t.Fatalf("%d gates left for a deleted blocker item", n)
	}
	for _, name := range []string{"idx_schedule_gates_blocker", "idx_schedule_gates_workspace"} {
		var found int
		q := `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`
		if s.dialect.Driver() == DriverPostgres {
			q = `SELECT COUNT(*) FROM pg_indexes WHERE indexname=?`
		}
		found = s.countRows(t, q, name)
		if found != 1 {
			t.Fatalf("index %s: %d", name, found)
		}
	}
	if s.dialect.Driver() == DriverPostgres {
		return
	}
	rows, err := s.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key violation after the migration")
	}
}

func TestGateMigrationSQLite(t *testing.T) {
	t.Parallel()
	db, _ := preMigrationDB(t, "174")
	s := &Store{db: db, dialect: &sqliteDialect{}}
	f := seedOldGates(t, s)
	if err := applySQLiteMigration(db, gateMigration, gateMigrationSQL(t, false)); err != nil {
		t.Fatal(err)
	}
	checkGateMigration(t, s, f)
	if body := gateMigrationSQL(t, false); strings.Contains(body, "'") {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "--") && strings.Contains(line, "'") {
				t.Fatalf("an apostrophe in a SQL comment: %s", line)
			}
		}
	}
}

func TestGateMigrationPostgres(t *testing.T) {
	t.Parallel()
	url := os.Getenv("PAD_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	s := testStorePostgres(t, url)
	// Put the table back in the shape migration 134 gave it, then migrate it.
	s.mustExec(t, `DROP TABLE schedule_gates`)
	s.mustExec(t, oldGateTable)
	s.mustExec(t, `CREATE INDEX idx_schedule_gates_blocker ON schedule_gates(blocker_item_id)`)
	s.mustExec(t, `CREATE INDEX idx_schedule_gates_workspace ON schedule_gates(workspace_id)`)
	f := seedOldGates(t, s)
	if err := applyPostgresMigration(s.db, gatePGMigration+".probe", gateMigrationSQL(t, true)); err != nil {
		t.Fatal(err)
	}
	checkGateMigration(t, s, f)
}
