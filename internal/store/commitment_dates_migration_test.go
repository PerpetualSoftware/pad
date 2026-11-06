package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const commitmentMigration = "167_commitment_dates.sql"
const commitmentPGMigration = "131_commitment_dates.sql"

func commitmentSQL(t *testing.T, pg bool) string {
	t.Helper()
	fs, path := migrationsFS, "migrations/"+commitmentMigration
	if pg {
		fs, path = pgMigrationsFS, "pgmigrations/"+commitmentPGMigration
	}
	b, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func commitmentPreStore(t *testing.T) *Store {
	t.Helper()
	db, _ := preMigrationDB(t, "167")
	return &Store{db: db, dialect: &sqliteDialect{}}
}

func commitmentApply(t *testing.T, s *Store) {
	t.Helper()
	if s.dialect.Driver() == DriverPostgres {
		if err := applyPostgresMigration(s.db, commitmentPGMigration+".probe", commitmentSQL(t, true)); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := applySQLiteMigration(s.db, commitmentMigration, commitmentSQL(t, false)); err != nil {
		t.Fatal(err)
	}
	b, err := migrationsFS.ReadFile("migrations/168_nul_invariant_triggers_post167.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := applySQLiteMigration(s.db, "168_nul_invariant_triggers_post167.sql", string(b)); err != nil {
		t.Fatal(err)
	}
}

func commitmentSeed(t *testing.T, s *Store) []string {
	t.Helper()
	ws := createTestWorkspace(t, s, "Commitment migration")
	col := createTestCollection(t, s, ws.ID, "Dates")
	var ids []string
	for _, kind := range []string{"deadline_soft", "deadline_hard", "check", "window"} {
		for _, variant := range []string{"fired", "ended", "floating", "snoozed", "exdates", "allday", "midnight"} {
			item := createTestItem(t, s, ws.ID, col.ID, kind+variant, "")
			id, ts := newID(), "2026-10-29T12:00:00Z"
			start, end := "2026-11-02T09:00:00", "2026-11-02T10:30:00"
			if variant == "allday" {
				start, end = "2026-11-02", "2026-11-04"
			}
			if variant == "midnight" {
				start, end = "2026-11-02T23:30:00", "2026-11-03T01:00:00"
			}
			var dtend, tz, nextAt, nextWall, ended, override, fired any = nil, "UTC", "2026-11-02T09:00:00Z", start, nil, nil, nil
			if kind == "window" {
				dtend = end
			}
			if variant == "floating" {
				tz = nil
			}
			if variant == "ended" {
				nextAt, nextWall, ended = nil, nil, ts
			}
			if variant == "snoozed" {
				override = "2026-11-05T09:00:00Z"
			}
			if variant == "fired" {
				fired = ts
			}
			ex := "[]"
			if variant == "exdates" {
				ex = `["2026-11-03T09:00:00"]`
			}
			s.mustExec(t, `INSERT INTO item_schedules (id,workspace_id,item_id,kind,dtstart,dtend,tz,floating,next_at,next_wall,ended_at,next_override_at,exdates,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, ws.ID, item.ID, kind, start, dtend, tz, s.dialect.BoolToInt(variant == "floating"), nextAt, nextWall, ended, override, ex, ts, ts)
			s.mustExec(t, `INSERT INTO schedule_occurrences (id,schedule_id,occurrence_at,occurrence_wall,state,actor,resolved_at) VALUES (?,?,?,?,'done','user',?)`, newID(), id, "2026-10-28T09:00:00Z", "2026-10-28T09:00:00", ts)
			if variant != "ended" {
				s.mustExec(t, `INSERT INTO item_reminders (id,workspace_id,item_id,schedule_id,remind_at,fired_at,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`, newID(), ws.ID, item.ID, id, ts, fired, ts, ts)
			}
			ids = append(ids, id)
		}
	}
	return ids
}

func commitmentOldRows(t *testing.T, s *Store) []string {
	t.Helper()
	var out []string
	for _, q := range []string{
		`SELECT id,kind,dtstart,COALESCE(dtend,''),COALESCE(next_at,''),COALESCE(next_wall,''),COALESCE(next_override_at,''),COALESCE(ended_at,''),exdates,COALESCE(tz,''),floating,COALESCE(rrule,''),rdates,anchor,audience,lead_minutes,COALESCE(anchor_wall,''),COALESCE(error_code,''),created_at,updated_at FROM item_schedules ORDER BY id`,
		`SELECT id,schedule_id,occurrence_at,occurrence_wall,state,actor,resolved_at FROM schedule_occurrences ORDER BY id`,
		`SELECT id,schedule_id,remind_at,COALESCE(fired_at,'') FROM item_reminders WHERE schedule_id IS NOT NULL ORDER BY id`,
	} {
		rows, err := s.db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			vals := make([]string, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out = append(out, strings.Join(vals, "|"))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return out
}

func TestCommitmentMigration_BackfillsAllFourKinds(t *testing.T) {
	s := commitmentPreStore(t)
	commitmentSeed(t, s)
	commitmentApply(t, s)
	commitmentCheckRoles(t, s)
}

func commitmentCheckRoles(t *testing.T, s *Store) {
	t.Helper()
	rows, err := s.db.Query(`SELECT kind,dtstart,COALESCE(dtend,''),target_days,target_secs,earliest_days,earliest_secs,deadline_days,deadline_secs,deadline_behavior,occupied_days,occupied_secs,outcome,roles_origin FROM item_schedules`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var kind, start, end, outcome, origin string
		var td, ts int
		var ed, es, dd, ds, od, os sql.NullInt64
		var behavior sql.NullString
		if err := rows.Scan(&kind, &start, &end, &td, &ts, &ed, &es, &dd, &ds, &behavior, &od, &os, &outcome, &origin); err != nil {
			t.Fatal(err)
		}
		if td != 0 || ts != 0 || ed.Valid || es.Valid || origin != "legacy_backfill" || outcome != "" {
			t.Fatalf("invalid roles for %s", kind)
		}
		if kind == "deadline_hard" {
			if !dd.Valid || !ds.Valid || dd.Int64 != 0 || ds.Int64 != 0 || behavior.String != "stays_owed" {
				t.Fatal("hard deadline mapping")
			}
		} else if dd.Valid || ds.Valid || behavior.Valid {
			t.Fatal("unexpected deadline")
		}
		if kind == "window" {
			days, secs := int64(0), int64(5400)
			if len(start) == 10 {
				days, secs = 2, 0
			}
			if strings.Contains(start, "23:30") {
				days, secs = 1, -81000
			}
			if !od.Valid || !os.Valid || od.Int64 != days || os.Int64 != secs {
				t.Fatalf("window %s to %s: %v %v", start, end, od, os)
			}
		} else if od.Valid || os.Valid {
			t.Fatal("unexpected occupied interval")
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 28 {
		t.Fatalf("rows=%d", n)
	}
	if n := s.countRows(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE actual_source='legacy_unknown' AND actual_done_at IS NULL`); n != 28 {
		t.Fatalf("unknown actuals=%d", n)
	}
}

func TestCommitmentMigration_PreservesRowsAndFKs(t *testing.T) {
	s := commitmentPreStore(t)
	ids := commitmentSeed(t, s)
	before := commitmentOldRows(t, s)
	counts := map[string]int{}
	for _, table := range []string{"event_outbox", "user_notifications"} {
		counts[table] = s.countRows(t, `SELECT COUNT(*) FROM `+table)
	}
	commitmentApply(t, s)
	commitmentCheckRoles(t, s)
	if !reflect.DeepEqual(before, commitmentOldRows(t, s)) {
		t.Fatal("legacy rows changed")
	}
	for _, table := range []string{"event_outbox", "user_notifications"} {
		if n := s.countRows(t, `SELECT COUNT(*) FROM `+table); n != counts[table] {
			t.Fatalf("%s before=%d after=%d", table, counts[table], n)
		}
	}
	rows, err := s.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Fatal("foreign key violation")
	}
	rows.Close()
	s.mustExec(t, `DELETE FROM item_schedules WHERE id=?`, ids[0])
	for _, table := range []string{"schedule_occurrences", "item_reminders"} {
		if n := s.countRows(t, `SELECT COUNT(*) FROM `+table+` WHERE schedule_id=?`, ids[0]); n != 0 {
			t.Fatal("cascade failed")
		}
	}
}

func TestCommitmentMigration_ExistingActualsStayUnknown(t *testing.T) {
	s := commitmentPreStore(t)
	ids := commitmentSeed(t, s)
	for i, state := range []string{"skipped", "moved", "missed"} {
		s.mustExec(t, `UPDATE schedule_occurrences SET state=? WHERE schedule_id=?`, state, ids[i])
	}
	commitmentApply(t, s)
	if n := s.countRows(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE actual_source='legacy_unknown' AND actual_done_at IS NULL`); n != 28 {
		t.Fatalf("unknown actuals=%d", n)
	}
}

func TestCommitmentMigration_RollsBackOnInvalidFK(t *testing.T) {
	s := commitmentPreStore(t)
	commitmentSeed(t, s)
	body := commitmentSQL(t, false) + `INSERT INTO schedule_occurrence_overrides (id,schedule_id,occurrence_key,role,value_at,created_at,updated_at) VALUES ('bad','absent','key','target','2026-11-02T00:00:00Z','now','now');`
	if err := applySQLiteMigration(s.db, commitmentMigration, body); err == nil {
		t.Fatal("invalid FK accepted")
	}
	if n := s.countRows(t, `SELECT COUNT(*) FROM schema_migrations WHERE version=?`, commitmentMigration); n != 0 {
		t.Fatal("failed migration recorded")
	}
	if _, err := s.db.Exec(`SELECT target_days FROM item_schedules`); err == nil {
		t.Fatal("partial DDL survived")
	}
	if n := s.countRows(t, `SELECT COUNT(*) FROM item_schedules`); n != 28 {
		t.Fatal("rows lost")
	}
}

func TestCommitmentMigration_PGLockTimeoutDeclared(t *testing.T) {
	body := commitmentSQL(t, true)
	if !strings.HasPrefix(body, "SET LOCAL lock_timeout = '5s';\n") {
		t.Fatal("missing transaction-local lock timeout")
	}
	url := os.Getenv("PAD_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set; header checked")
	}
	s := testStorePostgres(t, url)
	for i := 0; i < 2; i++ {
		if err := applyPostgresMigration(s.db, fmt.Sprintf("999_commitment_repeat_%d.sql", i), body); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCommitmentMigration_PostgresBackfillAndRollback(t *testing.T) {
	url := os.Getenv("PAD_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	s := testStorePostgres(t, url)
	s.mustExec(t, `DROP TABLE schedule_changes, schedule_occurrence_overrides`)
	for table, cols := range map[string][]string{"item_schedules": {"target_days", "target_secs", "earliest_secs", "earliest_days", "deadline_behavior", "deadline_secs", "deadline_days", "occupied_secs", "occupied_days", "outcome", "roles_origin"}, "schedule_occurrences": {"actual_done_at", "actual_source"}} {
		for _, col := range cols {
			s.mustExec(t, `ALTER TABLE `+table+` DROP COLUMN `+col+` CASCADE`)
		}
	}
	ids := commitmentSeed(t, s)
	counts := map[string]int{}
	for _, table := range []string{"event_outbox", "user_notifications"} {
		counts[table] = s.countRows(t, `SELECT COUNT(*) FROM `+table)
	}
	before := commitmentOldRows(t, s)
	body := commitmentSQL(t, true)
	if err := applyPostgresMigration(s.db, "999_commitment_bad.sql", body+`INSERT INTO schedule_occurrence_overrides (id,schedule_id,occurrence_key,role,value_at,created_at,updated_at) VALUES ('bad','absent','key','target','2026-11-02T00:00:00Z','now','now');`); err == nil {
		t.Fatal("invalid FK accepted")
	}
	if n := s.countRows(t, `SELECT COUNT(*) FROM schema_migrations WHERE version='999_commitment_bad.sql'`); n != 0 {
		t.Fatal("failed migration recorded")
	}
	if _, err := s.db.Exec(`SELECT target_days FROM item_schedules`); err == nil {
		t.Fatal("partial DDL survived")
	}
	commitmentApply(t, s)
	commitmentCheckRoles(t, s)
	if !reflect.DeepEqual(before, commitmentOldRows(t, s)) {
		t.Fatal("legacy rows changed")
	}
	if n := s.countRows(t, `SELECT COUNT(*) FROM item_schedules WHERE roles_origin='legacy_backfill' AND (kind<>'deadline_hard' OR (deadline_days=0 AND deadline_secs=0 AND deadline_behavior='stays_owed')) AND (kind<>'window' OR occupied_days IS NOT NULL)`); n != 28 {
		t.Fatal("PG backfill")
	}
	for table, want := range counts {
		if got := s.countRows(t, `SELECT COUNT(*) FROM `+table); got != want {
			t.Fatalf("%s before=%d after=%d", table, want, got)
		}
	}
	s.mustExec(t, `DELETE FROM item_schedules WHERE id=?`, ids[0])
	for _, table := range []string{"schedule_occurrences", "item_reminders"} {
		if n := s.countRows(t, `SELECT COUNT(*) FROM `+table+` WHERE schedule_id=?`, ids[0]); n != 0 {
			t.Fatal("cascade failed")
		}
	}

}

var commitmentChecks = []string{
	"CHECK ((earliest_days IS NULL) = (earliest_secs IS NULL))",
	"CHECK ((deadline_days IS NULL) = (deadline_secs IS NULL))",
	"CHECK ((occupied_days IS NULL) = (occupied_secs IS NULL))",
	"CHECK (deadline_behavior IN ('lapses','stays_owed'))",
	"CHECK ((deadline_days IS NULL) = (deadline_behavior IS NULL))",
	"CHECK (roles_origin IN ('legacy_backfill','native'))",
	"CHECK (actual_source IN ('reported','recorded_now','legacy_unknown'))",
	"CHECK (actual_done_at IS NULL OR (actual_source IS NOT NULL AND actual_source IN ('reported','recorded_now')))",
}

func TestCommitmentMigration_NoRebuild(t *testing.T) {
	s := commitmentPreStore(t)
	commitmentSeed(t, s)
	var before string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='item_schedules'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	body := commitmentSQL(t, false)
	if strings.Contains(strings.ToUpper(body), "DROP TABLE") {
		t.Fatal("table rebuild")
	}
	commitmentApply(t, s)
	var after string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='item_schedules'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(before, "\n") {
		if strings.Contains(line, "CHECK") {
			if !strings.Contains(after, strings.TrimSpace(line)) {
				t.Fatalf("lost check %s", line)
			}
		}
	}
	for _, check := range commitmentChecks {
		if !strings.Contains(body, check) {
			t.Fatalf("missing exact check %s", check)
		}
	}
	for _, index := range []string{"idx_item_schedules_next", "idx_item_schedules_ws_next", "uq_schedule_occurrences_allday_wall", "uq_item_reminders_schedule"} {
		var ddl string
		if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name=?`, index).Scan(&ddl); err != nil || !strings.Contains(ddl, " WHERE ") {
			t.Fatalf("lost partial index %s: %v", index, err)
		}
	}
	for _, set := range []string{"earliest_days=1", "deadline_days=1", "occupied_days=1", "deadline_behavior='lapses'", "roles_origin='bad'"} {
		if _, err := s.db.Exec(`UPDATE item_schedules SET ` + set); err == nil {
			t.Fatalf("invalid roles accepted: %s", set)
		}
	}
}

func TestMigrationCountsOnCopy(t *testing.T) {
	path := os.Getenv("PAD_MIGRATION_COPY")
	if path == "" {
		t.Skip("PAD_MIGRATION_COPY not set")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatal(err)
	}
	if abs == "/home/dave/.pad/pad.db" {
		t.Fatal("production path refused")
	}
	if err := registerGuardedDrivers(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(guardedSQLiteDriver, "file:"+abs+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	counts := func(db *sql.DB) []int {
		t.Helper()
		var out []int
		for _, q := range []string{`SELECT COUNT(*) FROM item_schedules`, `SELECT COUNT(*) FROM schedule_occurrences`, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id IS NOT NULL`, `SELECT COUNT(*) FROM event_outbox`, `SELECT COUNT(*) FROM user_notifications`} {
			var n int
			if err := db.QueryRow(q).Scan(&n); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		return out
	}
	before := counts(db)
	fkCount := func(q interface {
		Query(string, ...any) (*sql.Rows, error)
	}) int {
		t.Helper()
		rows, err := q.Query(`PRAGMA foreign_key_check`)
		if err != nil {
			t.Fatal("foreign key check failed")
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		return n
	}
	fkBefore := fkCount(db)
	db.Close()
	s, err := New(abs)
	if err != nil {
		t.Fatal("migration failed")
	}
	defer s.Close()
	after := counts(s.db)
	t.Logf("counts in=%v out=%v", before, after)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("counts differ")
	}
	fkAfter := fkCount(s.db)
	t.Logf("foreign key violations in=%d out=%d", fkBefore, fkAfter)
	if fkAfter != fkBefore {
		t.Fatal("migration changed the foreign key violation count")
	}
	var integrity string
	if err := s.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal("integrity check failed")
	}
}
