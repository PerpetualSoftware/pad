package server

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/store"
)

func TestAgendaExpansionUsesRequestClockAndDeadline(t *testing.T) {
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	it := w.sched(t, w.owner, "Dense", "2031-03-10T09:00", "both")
	meAgendaExec(t, w.agendaWorld, `UPDATE item_schedules SET rrule='FREQ=MINUTELY',deadline_days=1,deadline_secs=0,deadline_behavior='stays_owed' WHERE item_id=?`, it.ID)
	from, to := meAgendaNow.Add(-12*time.Hour), meAgendaNow.Add(12*time.Hour)
	rows, _, err := w.srv.store.ListSchedulesForAgenda(w.ws.ID, store.PendingReminderScope{}, from, to, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("schedules=%d err=%v", len(rows), err)
	}
	calls := 0
	clock := func() time.Time {
		calls++
		if calls == 1 {
			return meAgendaNow
		}
		return meAgendaNow.Add(2 * time.Second)
	}
	got, err := expandAgendaSchedules(w.srv.store, rows, time.UTC, from, to, meAgendaNow, meAgendaNow.Add(agendaBudget), clock)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 0 || !got.Truncated || !got.SetCut || len(got.Unexpanded) != 1 {
		t.Fatalf("expired expansion must stop and report: entries=%d truncated=%v setCut=%v unexpanded=%v", len(got.Entries), got.Truncated, got.SetCut, got.Unexpanded)
	}
}
