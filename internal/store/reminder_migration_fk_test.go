package store

import (
	"context"
	"database/sql"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func seedUnrelatedReminderMigrationViolations(t *testing.T, s *Store) {
	t.Helper()
	ws := createTestWorkspace(t, s, "Legacy FK violations")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	createTestItem(t, s, ws.ID, col.ID, "Missing collection", "open")
	item := createTestItem(t, s, ws.ID, col.ID, "Missing item", "open")
	// Item creation already wrote a version. Retain it, but remove the
	// transition so the fixture has only the two intended dangling references.
	s.mustExec(t, `DELETE FROM status_transitions WHERE item_id=?`, item.ID)
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	defer func() {
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
			t.Error(err)
		}
	}()
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`PRAGMA foreign_keys=OFF`, nil},
		{`DELETE FROM items WHERE id=?`, []any{item.ID}},
		{`DELETE FROM collections WHERE id=?`, []any{col.ID}},
	} {
		if _, err := conn.ExecContext(ctx, stmt.query, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
}

// Include rowids so the test detects replacement or repair of a legacy orphan,
// not merely a change in the number of violations.
func reminderMigrationFKRows(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT "table" || '|' || rowid || '|' || parent || '|' || fkid FROM pragma_foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestReminderMigrationPreservesUnrelatedFKViolations(t *testing.T) {
	s, name, body := reminderMigrationStore(t, false)
	armedWS, _, armedID := legacyReminderRow(t, s, "armed")
	firedWS, _, firedID := legacyReminderRow(t, s, "fired")
	collisionWS, collisionItem, ackedID := legacyReminderRow(t, s, "acked")
	winnerID := newID()
	s.mustExec(t, `INSERT INTO item_reminders(id,workspace_id,item_id,remind_at,fired_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, winnerID, collisionWS, collisionItem, past, past, past, past)
	seedUnrelatedReminderMigrationViolations(t, s)
	before := reminderMigrationFKRows(t, s)
	if len(before) != 2 || !strings.HasPrefix(before[0], "item_versions|") || !strings.HasPrefix(before[1], "items|") {
		t.Fatalf("expected only item and version legacy violations, got %v", before)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := runReminderMigration(s, name, body); err != nil {
			t.Fatalf("upgrade %d: %v", attempt, err)
		}
	}
	if !recorded(t, s, name) {
		t.Fatal("migration version not recorded")
	}
	if after := reminderMigrationFKRows(t, s); !reflect.DeepEqual(after, before) {
		t.Fatalf("legacy violations changed: before=%v after=%v", before, after)
	}
	for _, want := range []struct {
		ws, id string
		fired  bool
	}{{armedWS, armedID, false}, {firedWS, firedID, true}, {collisionWS, winnerID, true}} {
		r, err := s.GetReminder(want.ws, want.id)
		if err != nil || r == nil || r.ScheduleID == nil || (r.FiredAt != nil) != want.fired || r.AckedAt != nil || r.RemindAt != past || r.CreatedAt != past || r.UpdatedAt != past {
			t.Fatalf("converted reminder %s: %+v, %v", want.id, r, err)
		}
	}
	if s.countRows(t, `SELECT COUNT(*) FROM item_reminders`) != 3 || s.countRows(t, `SELECT COUNT(*) FROM item_schedules`) != 3 || s.countRows(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='import'`) != 4 {
		t.Fatal("conversion or history counts changed")
	}
	if s.countRows(t, `SELECT COUNT(*) FROM schedule_changes WHERE item_id=? AND reason=? AND json_extract(before_json,'$.id')=?`, collisionItem, legacyReminderHistoryReason, ackedID) != 1 {
		t.Fatal("acked collision row was not kept as history")
	}
	for tick := 0; tick < 2; tick++ {
		fired, err := s.FireDueReminders(nowTS(), 0)
		if err != nil || (tick == 0 && (len(fired) != 1 || fired[0].ID != armedID)) || (tick == 1 && len(fired) != 0) {
			t.Fatalf("tick %d: fired=%+v err=%v", tick, fired, err)
		}
	}
}

func TestReminderMigrationRejectsNewFKViolations(t *testing.T) {
	for _, tc := range []struct{ table, column string }{
		{"item_reminders", "schedule_id"},
		{"item_schedules", "item_id"},
		{"schedule_changes", "workspace_id"},
	} {
		t.Run(tc.table, func(t *testing.T) {
			s, name, body := reminderMigrationStore(t, false)
			ws, item, _ := legacyReminderRow(t, s, "armed")
			seedUnrelatedReminderMigrationViolations(t, s)
			beforeFK := reminderMigrationFKRows(t, s)
			before := renegotiateState(t, s)
			// The runner already accepts SQL as a test seam. Split the PRAGMA
			// across lines so extractPragmas leaves it inside the transaction:
			// defer enforcement to exercise the before/after comparison.
			body += "\nPRAGMA defer_foreign_keys =\nON;\nUPDATE " + tc.table + " SET " + tc.column + "='missing-parent';"
			err := runReminderMigration(s, name, body)
			if err == nil || !strings.Contains(err.Error(), "foreign key violation(s) on "+tc.table+"|") {
				t.Fatalf("expected runner to reject new %s violation, got %v", tc.table, err)
			}
			if recorded(t, s, name) || !reflect.DeepEqual(renegotiateState(t, s), before) || !reflect.DeepEqual(reminderMigrationFKRows(t, s), beforeFK) {
				t.Fatal("failed migration did not roll back data and version")
			}
			var scheduleID sql.NullString
			if err := s.db.QueryRow(`SELECT schedule_id FROM item_reminders WHERE item_id=?`, item).Scan(&scheduleID); err != nil || scheduleID.Valid {
				t.Fatalf("conversion did not roll back: %v, %v", scheduleID, err)
			}
			// The rebuild must roll back its NOT NULL constraint too.
			s.mustExec(t, `INSERT INTO item_reminders(id,workspace_id,item_id,remind_at,created_at,updated_at) VALUES(?,?,?,?,?,?)`, newID(), ws, item, past, past, past)
		})
	}
}
