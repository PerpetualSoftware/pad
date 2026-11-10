package server

import (
	"database/sql"
	"fmt"
	"github.com/PerpetualSoftware/pad/internal/models"
	"testing"
	"time"
)

func TestTASK4423_R09GateConsumers(t *testing.T) {
	t.Run("pending gates cannot starve", func(t *testing.T) { needsBackends(t, graphTickFairness) })
	t.Run("held deadline", func(t *testing.T) { needsBackends(t, graphHeldDeadline) })
	t.Run("tick opens without reader", func(t *testing.T) {
		needsBackends(t, func(t *testing.T, srv *Server) {
			f := newSchedHTTPOn(t, srv)
			blocker := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "Blocker")
			acc := models.CommitmentAccess{Readable: map[string]bool{f.item.ID: true, blocker.ID: true}, Editable: map[string]bool{f.item.ID: true, blocker.ID: true}}
			g, err := f.srv.store.CreateScheduleGate(f.ws.ID, f.item.ID, blocker.ID, acc)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.graphExec(`UPDATE items SET fields='{"status":"done"}' WHERE id=?`, blocker.ID); err != nil {
				t.Fatal(err)
			}
			f.srv.runReminderTick()
			var opened sql.NullString
			if err := f.graphRow(`SELECT opened_at FROM schedule_gates WHERE id=?`, g.ID).Scan(&opened); err != nil {
				t.Fatal(err)
			}
			if !opened.Valid {
				t.Fatal("tick did not open gate")
			}
		})
	})
	t.Run("hold preserves reminder and does not starve", func(t *testing.T) {
		needsBackends(t, func(t *testing.T, srv *Server) {
			f := newSchedHTTPOn(t, srv)
			blocker := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "Blocker")
			other := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "Other")
			for _, it := range []*models.Item{f.item, other} {
				if _, err := f.srv.store.CreateCommitmentSchedule(f.ws.ID, it.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2030-01-01T09:00", Tz: "UTC"}, models.CommitmentRolesInput{}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.graphExec(`UPDATE item_schedules SET dtstart='2026-01-01T09:00',next_at='2026-01-01T09:00:00Z',next_wall='2026-01-01T09:00:00' WHERE workspace_id=?`, f.ws.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.graphExec(`UPDATE item_reminders SET remind_at='2026-01-01T09:00:00Z' WHERE workspace_id=?`, f.ws.ID); err != nil {
				t.Fatal(err)
			}
			acc := models.CommitmentAccess{Readable: map[string]bool{f.item.ID: true, blocker.ID: true}, Editable: map[string]bool{f.item.ID: true, blocker.ID: true}}
			if _, err := f.srv.store.CreateScheduleGate(f.ws.ID, f.item.ID, blocker.ID, acc); err != nil {
				t.Fatal(err)
			}
			// Order the held row first, independently of random ids.
			if _, err := f.graphExec(`UPDATE item_reminders SET remind_at='2025-12-31T09:00:00Z' WHERE item_id=?`, f.item.ID); err != nil {
				t.Fatal(err)
			}
			fired, err := f.srv.store.FireDueReminders(time.Now().UTC().Format(time.RFC3339), 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(fired) != 1 || fired[0].ItemID != other.ID {
				t.Fatalf("fired=%+v", fired)
			}
			var at sql.NullString
			if err := f.graphRow(`SELECT fired_at FROM item_reminders WHERE item_id=?`, f.item.ID).Scan(&at); err != nil {
				t.Fatal(err)
			}
			if at.Valid {
				t.Fatal("held reminder consumed")
			}
			fields := `{"status":"done"}`
			if _, err := f.srv.store.UpdateItem(blocker.ID, models.ItemUpdate{Fields: &fields}); err != nil {
				t.Fatal(err)
			}
			fired, err = f.srv.store.FireDueReminders(time.Now().UTC().Format(time.RFC3339), 1)
			if err != nil || len(fired) != 1 || fired[0].ItemID != f.item.ID {
				t.Fatalf("released=%+v %v", fired, err)
			}
		})
	})
}

func (f *schedHTTP) graphExec(query string, args ...any) (sql.Result, error) {
	return f.srv.store.DB().Exec(f.srv.store.D().Rebind(query), args...)
}
func (f *schedHTTP) graphRow(query string, args ...any) *sql.Row {
	return f.srv.store.DB().QueryRow(f.srv.store.D().Rebind(query), args...)
}

func graphTickFairness(t *testing.T, srv *Server) {
	f := newSchedHTTPOn(t, srv)
	pending := mustItem(t, srv, f.ws.ID, f.coll.ID, "Pending blocker")
	success := mustItem(t, srv, f.ws.ID, f.coll.ID, "Successful blocker")
	acc := models.CommitmentAccess{Readable: map[string]bool{pending.ID: true, success.ID: true, f.item.ID: true}, Editable: map[string]bool{pending.ID: true, success.ID: true, f.item.ID: true}}
	for i := 0; i < 5; i++ {
		child := mustItem(t, srv, f.ws.ID, f.coll.ID, fmt.Sprintf("Pending child %d", i))
		acc.Readable[child.ID], acc.Editable[child.ID] = true, true
		g, err := srv.store.CreateScheduleGate(f.ws.ID, child.ID, pending.ID, acc)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.graphExec(`UPDATE schedule_gates SET id=? WHERE id=?`, fmt.Sprintf("a-pending-%d", i), g.ID); err != nil {
			t.Fatal(err)
		}
	}
	g, err := srv.store.CreateScheduleGate(f.ws.ID, f.item.ID, success.ID, acc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.graphExec(`UPDATE schedule_gates SET id='z-success' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.graphExec(`UPDATE items SET fields='{"status":"done"}' WHERE id=?`, success.ID); err != nil {
		t.Fatal(err)
	}
	srv.reminderTick.mu.Lock()
	srv.reminderTick.limit = 1
	srv.reminderTick.mu.Unlock()
	for i := 0; i < 6; i++ {
		srv.runReminderTick()
	}
	var opened sql.NullString
	if err := f.graphRow(`SELECT opened_at FROM schedule_gates WHERE id='z-success'`).Scan(&opened); err != nil {
		t.Fatal(err)
	}
	if !opened.Valid {
		t.Fatal("pending gates starved later success")
	}
}

func graphHeldDeadline(t *testing.T, srv *Server) {
	f := newSchedHTTPOn(t, srv)
	blocker := mustItem(t, srv, f.ws.ID, f.coll.ID, "Pending blocker")
	zero := 0
	behavior := "stays_owed"
	if _, err := srv.store.CreateCommitmentSchedule(f.ws.ID, f.item.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2030-01-01T09:00", Tz: "UTC"}, models.CommitmentRolesInput{DeadlineDays: &zero, DeadlineSecs: &zero, DeadlineBehavior: &behavior}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.graphExec(`UPDATE item_schedules SET dtstart='2026-01-01T09:00',next_at='2026-01-01T09:00:00Z',next_wall='2026-01-01T09:00:00' WHERE item_id=?`, f.item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.graphExec(`UPDATE item_reminders SET remind_at='2026-01-01T09:00:00Z' WHERE item_id=?`, f.item.ID); err != nil {
		t.Fatal(err)
	}
	acc := models.CommitmentAccess{Readable: map[string]bool{f.item.ID: true, blocker.ID: true}, Editable: map[string]bool{f.item.ID: true, blocker.ID: true}}
	if _, err := srv.store.CreateScheduleGate(f.ws.ID, f.item.ID, blocker.ID, acc); err != nil {
		t.Fatal(err)
	}
	srv.runReminderTick()
	var misses int
	if err := f.graphRow(`SELECT COUNT(*) FROM schedule_changes WHERE item_id=? AND role='deadline' AND change_type='missed_recorded'`, f.item.ID).Scan(&misses); err != nil {
		t.Fatal(err)
	}
	if misses != 1 {
		t.Fatalf("held hard deadline misses=%d", misses)
	}
	var fired sql.NullString
	if err := f.graphRow(`SELECT fired_at FROM item_reminders WHERE item_id=?`, f.item.ID).Scan(&fired); err != nil {
		t.Fatal(err)
	}
	if fired.Valid {
		t.Fatal("held target fired")
	}
}
