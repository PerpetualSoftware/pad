package store

import (
	"github.com/PerpetualSoftware/pad/internal/models"
	"testing"
)

func TestTASK4423_R09GateLifecycle(t *testing.T) {
	t.Run("done then reopened without read", func(t *testing.T) {
		f := newLinkFix(t, 1)
		blocker := f.buildItem(t, "open")
		g := f.gate(t, f.items[0].ID, blocker.ID, f.gateAccess(blocker))
		fields := `{"status":"shipped"}`
		if _, err := f.s.UpdateItem(blocker.ID, models.ItemUpdate{Fields: &fields}); err != nil {
			t.Fatal(err)
		}
		if !f.openedAt(t, g.ID).Valid {
			t.Fatal("Done did not durably open gate")
		}
		fields = `{"status":"open"}`
		if _, err := f.s.UpdateItem(blocker.ID, models.ItemUpdate{Fields: &fields}); err != nil {
			t.Fatal(err)
		}
		if !f.openedAt(t, g.ID).Valid {
			t.Fatal("reopen closed gate")
		}
	})
	t.Run("captured occurrence", func(t *testing.T) {
		f := newLinkFix(t, 2)
		f.recurring(t, 0)
		g := f.gate(t, f.items[1].ID, f.items[0].ID, f.allEdit())
		f.resolve(t, 0, "done")
		if !f.openedAt(t, g.ID).Valid {
			t.Fatal("resolve did not durably open gate")
		}
	})
	t.Run("read is pure", func(t *testing.T) {
		f := newLinkFix(t, 2)
		g := f.gate(t, f.items[1].ID, f.items[0].ID, f.allEdit())
		sc := f.reload(t, f.items[0].ID)
		f.exec(t, `INSERT INTO schedule_occurrences (id,schedule_id,occurrence_at,occurrence_wall,state,actor,resolved_at) VALUES (?,?,?,?,?,?,?)`, newID(), sc.ID, *sc.NextAt, *sc.NextWall, "done", "user", nowNano())
		f.gates(t, 1, f.allEdit())
		if f.openedAt(t, g.ID).Valid {
			t.Fatal("ScheduleGates wrote opened_at")
		}
	})
}
