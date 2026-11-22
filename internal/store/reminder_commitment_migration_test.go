package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func reminderMigrationStore(t *testing.T, pg bool) (*Store, string, string) {
	t.Helper()
	var s *Store
	name := "183_reminders_require_commitment.sql"
	fs, dir := migrationsFS, "migrations/"
	if pg {
		url := os.Getenv("PAD_TEST_POSTGRES_URL")
		if url == "" {
			t.Skip("PAD_TEST_POSTGRES_URL not set")
		}
		s = testStorePostgres(t, url)
		s.mustExec(t, `ALTER TABLE item_reminders ALTER COLUMN schedule_id DROP NOT NULL`)
		name = "143_reminders_require_commitment.sql"
		fs, dir = pgMigrationsFS, "pgmigrations/"
		s.mustExec(t, `DELETE FROM schema_migrations WHERE version=?`, name)
	} else {
		db, _ := preMigrationDB(t, "183")
		// Bundle cases use current workspace APIs while testing the old reminder schema.
		// 189 because current resolve, agenda and bundle code reads and writes the
		// occurrence role snapshot columns (TASK-4714). 190 because the user
		// reads select users.day_start_minute (TASK-4699).
		for _, later := range []string{"187_workspace_slug_claims.sql", "189_occurrence_role_offsets.sql", "190_user_day_start.sql"} {
			body, err := migrationsFS.ReadFile("migrations/" + later)
			if err != nil {
				t.Fatal(err)
			}
			if err := applySQLiteMigration(db, later, string(body)); err != nil {
				t.Fatal(err)
			}
		}
		s = &Store{db: db, dialect: &sqliteDialect{}}
	}
	body, err := fs.ReadFile(dir + name)
	if err != nil {
		t.Fatalf("missing paired migration: %v", err)
	}
	return s, name, string(body)
}

func runReminderMigration(s *Store, name, body string) error {
	if s.dialect.Driver() == DriverPostgres {
		return applyPostgresMigration(s.db, name, body)
	}
	return applySQLiteMigration(s.db, name, body)
}

func legacyReminderRow(t *testing.T, s *Store, state string) (string, string, string) {
	t.Helper()
	ws := createTestWorkspace(t, s, "Legacy reminders")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Legacy", "open")
	id := newID()
	var fired, acked any
	if state == "fired" || state == "acked" {
		fired = past
	}
	if state == "acked" {
		acked = past
	}
	s.mustExec(t, `INSERT INTO item_reminders(id,workspace_id,item_id,remind_at,fired_at,acked_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, id, ws.ID, item.ID, past, fired, acked, past, past)
	return ws.ID, item.ID, id
}

func testReminderMigrationLifecycle(t *testing.T, state string) {
	for _, pg := range []bool{false, true} {
		t.Run(fmt.Sprint(pg), func(t *testing.T) {
			s, name, body := reminderMigrationStore(t, pg)
			ws, item, id := legacyReminderRow(t, s, state)
			before, err := s.GetReminder(ws, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := runReminderMigration(s, name, body); err != nil {
				t.Fatal(err)
			}
			if err := runReminderMigration(s, name, body); err != nil {
				t.Fatalf("second upgrade: %v", err)
			}
			if state == "acked" {
				sc, err := s.GetSchedule(ws, item)
				if err != nil || sc == nil {
					t.Fatalf("carrier: %+v %v", sc, err)
				}
				assertDismissedReminderCarrier(t, s, ws, item, sc)
				if s.countRows(t, `SELECT COUNT(*) FROM schedule_changes WHERE item_id=? AND change_type='import'`, item) != 2 || s.countRows(t, `SELECT COUNT(*) FROM schedule_occurrences`) != 0 {
					t.Fatal("carrier journal or invented resolution")
				}
			} else {
				r, err := s.GetReminder(ws, id)
				if err != nil || r == nil || r.ScheduleID == nil || derefStr(r.FiredAt) != derefStr(before.FiredAt) || derefStr(r.AckedAt) != derefStr(before.AckedAt) || r.CreatedAt != before.CreatedAt || r.UpdatedAt != before.UpdatedAt {
					t.Fatalf("lost identity or state: %+v %v", r, err)
				}
				if s.countRows(t, `SELECT COUNT(*) FROM schedule_changes WHERE item_id=? AND change_type='import'`, item) != 1 || s.countRows(t, `SELECT COUNT(*) FROM schedule_occurrences`) != 0 {
					t.Fatal("conversion journal or invented resolution")
				}
			}
			for tick := 0; tick < 2; tick++ {
				fired, err := s.FireDueReminders(nowTS(), 0)
				want := 0
				if state == "armed" && tick == 0 {
					want = 1
				}
				if err != nil || len(fired) != want {
					t.Fatalf("tick %d fired=%d want=%d err=%v", tick, len(fired), want, err)
				}
			}
		})
	}
}
func TestReminderMigrationFiredNeverReplays(t *testing.T) { testReminderMigrationLifecycle(t, "fired") }
func TestReminderMigrationAckedNeverReplays(t *testing.T) { testReminderMigrationLifecycle(t, "acked") }
func TestReminderMigrationArmedPastFiresOnce(t *testing.T) {
	testReminderMigrationLifecycle(t, "armed")
}

func TestReminderMigrationConflictRollback(t *testing.T) {
	for _, pg := range []bool{false, true} {
		for _, mode := range []string{"orphan", "mismatched_owner", "after_backfill", "constraint"} {
			t.Run(fmt.Sprint(pg)+"/"+mode, func(t *testing.T) {
				s, name, body := reminderMigrationStore(t, pg)
				ws, item, id := legacyReminderRow(t, s, "armed")
				switch mode {
				case "orphan", "mismatched_owner":
					query, value := `UPDATE item_reminders SET item_id=? WHERE id=?`, newID()
					if mode == "mismatched_owner" {
						query = `UPDATE item_reminders SET workspace_id=? WHERE id=?`
						value = createTestWorkspace(t, s, "Other owner").ID
					}
					if pg {
						s.mustExec(t, `ALTER TABLE item_reminders DROP CONSTRAINT item_reminders_item_workspace_fkey`)
						s.mustExec(t, query, value, id)
					} else {
						ctx := context.Background()
						conn, err := s.db.Conn(ctx)
						if err != nil {
							t.Fatal(err)
						}
						for _, stmt := range []string{`PRAGMA foreign_keys=OFF`, s.q(query), `PRAGMA foreign_keys=ON`} {
							var args []any
							if stmt == s.q(query) {
								args = []any{value, id}
							}
							if _, err := conn.ExecContext(ctx, stmt, args...); err != nil {
								conn.Close()
								t.Fatal(err)
							}
						}
						conn.Close()
					}
				case "after_backfill":
					body = "SELECT * FROM absent_conversion_table;"
				case "constraint":
					body += "\nINSERT INTO item_reminders(id,workspace_id,item_id,remind_at,created_at,updated_at) SELECT id||'bad',workspace_id,item_id,remind_at,created_at,updated_at FROM item_reminders;"
				}
				before := renegotiateRows(t, s, "item_reminders", "") + renegotiateRows(t, s, "item_schedules", "") + renegotiateRows(t, s, "schedule_changes", "")
				err := runReminderMigration(s, name, body)
				if err == nil {
					t.Fatal("migration accepted invalid population")
				}
				if (mode == "orphan" || mode == "mismatched_owner") && !strings.Contains(err.Error(), "missing or mismatched owner") {
					t.Fatalf("wrong ownership refusal: %v", err)
				}
				after := renegotiateRows(t, s, "item_reminders", "") + renegotiateRows(t, s, "item_schedules", "") + renegotiateRows(t, s, "schedule_changes", "")
				if before != after || s.countRows(t, `SELECT COUNT(*) FROM schema_migrations WHERE version=?`, name) != 0 {
					t.Fatal("refusal was not atomic")
				}
				if mode == "constraint" {
					// The failed rebuild must restore the old nullable schema too.
					s.mustExec(t, `INSERT INTO item_reminders(id,workspace_id,item_id,remind_at,created_at,updated_at,schedule_id) VALUES(?,?,?,?,?,?,NULL)`, newID(), ws, item, past, past, past)
				}
			})
		}
	}
}

func TestReminderMigrationLockTimeoutRollback(t *testing.T) {
	s, name, body := reminderMigrationStore(t, true)
	_, _, _ = legacyReminderRow(t, s, "armed")
	before := renegotiateRows(t, s, "item_reminders", "")
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`LOCK TABLE items IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = runReminderMigration(s, name, body)
	if err == nil || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("expected lock timeout: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 29*time.Second || elapsed > 45*time.Second {
		t.Fatalf("unexpected lock timeout: %v", elapsed)
	}
	tx.Rollback()
	if renegotiateRows(t, s, "item_reminders", "") != before || s.countRows(t, `SELECT COUNT(*) FROM item_schedules`) != 0 || s.countRows(t, `SELECT COUNT(*) FROM schema_migrations WHERE version=?`, name) != 0 {
		t.Fatal("lock timeout changed migration state")
	}
}

func TestReminderCommitmentMigration(t *testing.T) {
	for _, pg := range []bool{false, true} {
		for _, state := range []string{"closed", "archived"} {
			t.Run(fmt.Sprint(pg)+"/"+state, func(t *testing.T) {
				s, name, body := reminderMigrationStore(t, pg)
				ws, item, id := legacyReminderRow(t, s, "armed")
				if state == "closed" {
					s.mustExec(t, `UPDATE items SET fields='{"status":"done"}' WHERE id=?`, item)
				} else {
					s.mustExec(t, `UPDATE items SET deleted_at=? WHERE id=?`, past, item)
				}
				if err := runReminderMigration(s, name, body); err != nil {
					t.Fatal(err)
				}
				if fired, err := s.FireDueReminders(nowTS(), 0); err != nil || len(fired) != 0 {
					t.Fatalf("paused fire=%+v %v", fired, err)
				}
				s.mustExec(t, `UPDATE items SET fields='{"status":"open"}',deleted_at=NULL WHERE id=?`, item)
				fired, err := s.FireDueReminders(nowTS(), 0)
				if err != nil || len(fired) != 1 || fired[0].ID != id {
					t.Fatalf("restored fire=%+v %v", fired, err)
				}
				if r, err := s.GetReminder(ws, id); err != nil || r == nil || r.ScheduleID == nil {
					t.Fatalf("restored alarm=%+v %v", r, err)
				}
			})
		}
	}
}

func TestMigratedReminderAckCancelsLegacyNotification(t *testing.T) {
	for _, pg := range []bool{false, true} {
		t.Run(fmt.Sprint(pg), func(t *testing.T) {
			s, name, body := reminderMigrationStore(t, pg)
			ws, item, id := legacyReminderRow(t, s, "fired")
			u := createTestUser(t, s, "legacy-notification@example.com", "Recipient", "password123")
			for _, row := range []struct{ id, key, state string }{{"old", "reminder:" + id + ":2020-01-01T00:00:00.123456789Z", "pending"}, {"delivered", "reminder:" + id + ":2020-01-01T00:00:00Z", "sent"}, {"other", "reminder:other:2020-01-01T00:00:00Z", "pending"}} {
				s.mustExec(t, `INSERT INTO user_notifications(id,user_id,workspace_id,item_id,kind,dedupe_key,created_at,push_state) VALUES(?,?,?,?,'reminder',?,?,?)`, row.id, u.ID, ws, item, row.key, past, row.state)
			}
			if err := runReminderMigration(s, name, body); err != nil {
				t.Fatal(err)
			}
			r, err := s.AckReminder(ws, id)
			if err != nil || r == nil || r.AckedAt == nil {
				t.Fatalf("ack=%+v %v", r, err)
			}
			var state string
			for id, want := range map[string]string{"old": "skipped", "delivered": "sent", "other": "pending"} {
				if err := s.db.QueryRow(s.q(`SELECT push_state FROM user_notifications WHERE id=?`), id).Scan(&state); err != nil || state != want {
					t.Fatalf("%s state=%s want=%s err=%v", id, state, want, err)
				}
			}
		})
	}
}
