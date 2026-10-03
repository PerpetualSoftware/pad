package storetest

import (
	"context"
	"database/sql/driver"
	"fmt"
	"sort"
	"sync"
	"testing"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// Write is one write the database recorded while a capture was running.
type Write struct {
	Table string
	Op    string // INSERT, UPDATE or DELETE
	// Depth is SQLite's sqlite3_preupdate_depth: 0 for a statement the
	// caller ran, above 0 for a write a trigger made. Postgres reports 0.
	Depth int
}

// CaptureWrites runs fn and returns every table write it caused, as the
// DATABASE saw it, triggers included (SPEC-6 §4, TASK-3388). It is the
// app store's write census instrument: a GUARD, not a proof.
//
// SQLite: the store is pinned to ONE connection (MaxOpenConns(1), no
// lifetime or idle expiry), and modernc's pre-update hook is registered on
// it, so every statement fn runs is observed. The hook does NOT fire for
// virtual tables or SQLite's own system tables. That blind inventory is
// asserted exactly by TestBlindInventory; FTS5's shadow tables are ordinary
// tables and ARE observed. A sentinel write into a temp table before and
// after fn proves the hook was still on the connection fn used. The store is
// left on one connection afterwards: a test that captures must not then
// expect concurrent store work.
//
// Postgres: a statement-level AFTER trigger on every table (installed by
// this harness, never by a shipped migration) appends to pad_write_audit,
// and the rows fn added are returned. A statement that changes zero rows
// still fires, so Postgres over-reports rather than under-reports.
func CaptureWrites(t *testing.T, s *store.Store, fn func()) []Write {
	t.Helper()
	if s.D().Driver() == store.DriverPostgres {
		return capturePostgres(t, s, fn)
	}
	return captureSQLite(t, s, fn)
}

// Tables returns the distinct tables in ws, sorted.
func Tables(ws []Write) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range ws {
		if !seen[w.Table] {
			seen[w.Table] = true
			out = append(out, w.Table)
		}
	}
	sort.Strings(out)
	return out
}

const sentinelTable = "pad_capture_sentinel"

func captureSQLite(t *testing.T, s *store.Store, fn func()) []Write {
	t.Helper()
	ctx := context.Background()
	db := s.DB()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	var mu sync.Mutex
	var writes []Write
	hook := func(d sqlite.SQLitePreUpdateData) {
		op := "?"
		switch d.Op {
		case sqlite3.SQLITE_INSERT:
			op = "INSERT"
		case sqlite3.SQLITE_UPDATE:
			op = "UPDATE"
		case sqlite3.SQLITE_DELETE:
			op = "DELETE"
		}
		mu.Lock()
		writes = append(writes, Write{Table: d.TableName, Op: op, Depth: d.Depth()})
		mu.Unlock()
	}
	setHook := func(h sqlite.PreUpdateHookFn) {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("capture: conn: %v", err)
		}
		defer conn.Close()
		if err := conn.Raw(func(dc any) error {
			// The store wraps the driver connection in its NUL guard.
			if u, ok := dc.(interface{ Unwrap() driver.Conn }); ok {
				dc = u.Unwrap()
			}
			r, ok := dc.(sqlite.HookRegisterer)
			if !ok {
				return fmt.Errorf("driver connection %T has no pre-update hook", dc)
			}
			r.RegisterPreUpdateHook(h)
			return nil
		}); err != nil {
			t.Fatalf("capture: %v", err)
		}
	}
	sentinel := func(when string) {
		mu.Lock()
		before := len(writes)
		mu.Unlock()
		if _, err := db.Exec(`INSERT INTO temp.`+sentinelTable+` (at) VALUES (?)`, when); err != nil {
			t.Fatalf("capture: sentinel %s: %v", when, err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(writes) != before+1 || writes[len(writes)-1].Table != sentinelTable {
			t.Fatalf("capture: the %s sentinel was not observed; the hook is not on the connection the store used", when)
		}
		writes = writes[:before]
	}

	if _, err := db.Exec(`CREATE TEMP TABLE IF NOT EXISTS ` + sentinelTable + ` (at TEXT)`); err != nil {
		t.Fatalf("capture: sentinel table: %v", err)
	}
	setHook(hook)
	defer setHook(nil)
	sentinel("start")
	fn()
	sentinel("end")

	mu.Lock()
	defer mu.Unlock()
	return append([]Write(nil), writes...)
}

var pgAuditOnce sync.Map // database identity -> struct{}

func capturePostgres(t *testing.T, s *store.Store, fn func()) []Write {
	t.Helper()
	db := s.DB()
	var dbName string
	if err := db.QueryRow(`SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if _, done := pgAuditOnce.Load(dbName); !done {
		installPGAudit(t, s)
		pgAuditOnce.Store(dbName, struct{}{})
	}
	var start int64
	if err := db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM pad_write_audit`).Scan(&start); err != nil {
		t.Fatalf("capture: audit start: %v", err)
	}
	fn()
	rows, err := db.Query(`SELECT tbl, op FROM pad_write_audit WHERE id > $1 ORDER BY id`, start)
	if err != nil {
		t.Fatalf("capture: audit read: %v", err)
	}
	defer rows.Close()
	var writes []Write
	for rows.Next() {
		var w Write
		if err := rows.Scan(&w.Table, &w.Op); err != nil {
			t.Fatalf("capture: audit scan: %v", err)
		}
		writes = append(writes, w)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("capture: audit rows: %v", err)
	}
	return writes
}

// installPGAudit puts a statement-level AFTER trigger on every base table in
// the store's schema. Tables created later are not covered, so it runs once
// per database, after migrations.
func installPGAudit(t *testing.T, s *store.Store) {
	t.Helper()
	db := s.DB()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS pad_write_audit (id BIGSERIAL PRIMARY KEY, txid BIGINT NOT NULL, tbl TEXT NOT NULL, op TEXT NOT NULL)`,
		`CREATE OR REPLACE FUNCTION pad_write_audit_fn() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   INSERT INTO pad_write_audit (txid, tbl, op) VALUES (txid_current(), TG_TABLE_NAME, TG_OP);
		   RETURN NULL;
		 END $$`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("capture: install audit: %v", err)
		}
	}
	rows, err := db.Query(`SELECT tablename FROM pg_tables WHERE schemaname = current_schema() AND tablename <> 'pad_write_audit'`)
	if err != nil {
		t.Fatalf("capture: list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("capture: list tables: %v", err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if len(tables) == 0 {
		t.Fatal("capture: no tables to audit")
	}
	for _, tbl := range tables {
		for _, q := range []string{
			fmt.Sprintf(`DROP TRIGGER IF EXISTS pad_write_audit_t ON %q`, tbl),
			fmt.Sprintf(`CREATE TRIGGER pad_write_audit_t AFTER INSERT OR UPDATE OR DELETE ON %q FOR EACH STATEMENT EXECUTE FUNCTION pad_write_audit_fn()`, tbl),
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("capture: trigger on %s: %v", tbl, err)
			}
		}
	}
}
