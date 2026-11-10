package server

import (
	"database/sql"
	"github.com/PerpetualSoftware/pad/internal/models"
	"testing"
	"time"
)

func TestTASK4423_R09GateConsumers(t *testing.T) {
	t.Run("tick opens without reader", func(t *testing.T) {
		f := newSchedHTTP(t)
		blocker := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "Blocker")
		acc := models.CommitmentAccess{Readable: map[string]bool{f.item.ID: true, blocker.ID: true}, Editable: map[string]bool{f.item.ID: true, blocker.ID: true}}
		g, err := f.srv.store.CreateScheduleGate(f.ws.ID, f.item.ID, blocker.ID, acc)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.srv.store.DB().Exec(`UPDATE items SET fields='{"status":"done"}' WHERE id=?`, blocker.ID); err != nil {
			t.Fatal(err)
		}
		f.srv.runReminderTick()
		var opened sql.NullString
		if err := f.srv.store.DB().QueryRow(`SELECT opened_at FROM schedule_gates WHERE id=?`, g.ID).Scan(&opened); err != nil {
			t.Fatal(err)
		}
		if !opened.Valid {
			t.Fatal("tick did not open gate")
		}
	})
	t.Run("hold preserves reminder and does not starve", func(t *testing.T) {
		f := newSchedHTTP(t)
		blocker := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "Blocker")
		other := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "Other")
		for _, it := range []*models.Item{f.item, other} {
			if _, err := f.srv.store.CreateCommitmentSchedule(f.ws.ID, it.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-01-01T09:00", Tz: "UTC"}, models.CommitmentRolesInput{}); err != nil {
				t.Fatal(err)
			}
		}
		acc := models.CommitmentAccess{Readable: map[string]bool{f.item.ID: true, blocker.ID: true}, Editable: map[string]bool{f.item.ID: true, blocker.ID: true}}
		if _, err := f.srv.store.CreateScheduleGate(f.ws.ID, f.item.ID, blocker.ID, acc); err != nil {
			t.Fatal(err)
		}
		// Order the held row first, independently of random ids.
		if _, err := f.srv.store.DB().Exec(`UPDATE item_reminders SET remind_at='2025-12-31T09:00:00Z' WHERE item_id=?`, f.item.ID); err != nil {
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
		if err := f.srv.store.DB().QueryRow(`SELECT fired_at FROM item_reminders WHERE item_id=?`, f.item.ID).Scan(&at); err != nil {
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
}
