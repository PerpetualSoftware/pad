package server

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestMeAgendaBudgetIsRequestWide(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	other := meAgendaWorkspace(t, w, "Other")
	for _, world := range []*agendaWorld{w, other} {
		world.item(t, "tasks", "One", `{}`, oneShot())
	}
	clock := time.Now()
	calls := 0
	w.srv.agendaClock = func() time.Time {
		calls++
		// Allow the first schedule's internal deadline checks, then expire
		// the shared request budget before the next workspace.
		if calls > 8 {
			return clock.Add(2 * time.Second)
		}
		return clock
	}
	m := meAgendaOK(t, w, w.owner, agendaWindow)
	if m["truncated"] != true || m["next_from"] != nil || len(entriesOf(m)) != 1 {
		t.Fatalf("request budget = %v", m)
	}
}

func TestMeAgendaCreatedCompletedSQLCaps(t *testing.T) {
	for _, kind := range []string{"created", "completed"} {
		t.Run(kind, func(t *testing.T) {
			w := newAgendaWorld(t, "tasks")
			for i := 0; i < 501; i++ {
				at := meAgendaNow.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
				it := w.item(t, "tasks", at, `{}`, nil)
				if kind == "created" {
					meAgendaExec(t, w, `UPDATE items SET created_at = ? WHERE id = ?`, at, it.ID)
				} else {
					meAgendaTransition(t, w, it, at, "done")
				}
			}
			m := meAgendaOK(t, w, w.owner, agendaWindow)
			if len(entriesOf(m)) != 500 || m["truncated"] != true || m["next_from"] != nil {
				t.Fatalf("%s SQL cap = entries:%d truncated:%v next:%v", kind, len(entriesOf(m)), m["truncated"], m["next_from"])
			}
		})
	}
}

func TestMeAgendaUnavailableVisibility(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks", "secret")
	visible := w.item(t, "tasks", "Unreadable", `{}`, oneShot())
	hidden := w.item(t, "secret", "Hidden unreadable", `{}`, oneShot())
	for _, it := range []*models.Item{visible, hidden} {
		meAgendaExec(t, w, `UPDATE item_schedules SET error_code = ? WHERE item_id = ?`, "invalid_rule", it.ID)
	}
	u := mustUser(t, w.srv, "unavailable-me-agenda@example.com", "unavailablemeagenda", "")
	if _, err := w.srv.store.CreateItemGrant(w.ws.ID, visible.ID, u.ID, "view", w.owner.ID); err != nil {
		t.Fatal(err)
	}
	m := meAgendaOK(t, w, u, agendaWindow)
	rows, ok := m["unavailable"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("unavailable = %v", m)
	}
	row := rows[0].(map[string]any)
	if row["workspace"] != w.ws.Slug || row["item_ref"] != visible.Ref {
		t.Fatalf("unavailable = %v", row)
	}
	for _, it := range []*models.Item{visible, hidden} {
		meAgendaExec(t, w, `UPDATE item_schedules SET error_code = NULL, dtstart = ?, rrule = ? WHERE item_id = ?`, "2001-03-01T00:00:00", "FREQ=MINUTELY;BYHOUR=3;BYMINUTE=0", it.ID)
	}
	m = meAgendaOK(t, w, u, agendaWindow)
	rows, ok = m["unexpanded"].([]any)
	if !ok || len(rows) != 1 || m["truncated"] != true || m["next_from"] != m["to"] {
		t.Fatalf("unexpanded = %v", m)
	}
	row = rows[0].(map[string]any)
	if row["workspace"] != w.ws.Slug || row["item_ref"] != visible.Ref {
		t.Fatalf("unexpanded = %v", row)
	}

}
