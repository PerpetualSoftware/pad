package server

import (
	"testing"
	"time"
)

func TestBootstrapAgendaFutureSchedulesDoNotConsumeDueScan(t *testing.T) {
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	for i := 0; i < 1001; i++ {
		w.sched(t, w.owner, "Future", "2032-03-10T09:00", "both")
	}
	good := w.sched(t, w.owner, "Due", "2031-03-10T09:00", "both")
	a := w.bootstrap(t, w.tok, nil).Agenda
	if a == nil || len(a.DueToday) != 1 || a.DueToday[0].Ref != good.Ref || !a.DueTodayComplete || a.Truncated {
		t.Fatalf("due scan consumed by future schedules: %+v", a)
	}
}

func TestBootstrapAgendaWorkScanLimit(t *testing.T) {
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	for i := 0; i < 201; i++ {
		it := w.item(t, "tasks", "Unplaced", `{}`, nil)
		meAgendaExec(t, w.agendaWorld, `UPDATE items SET created_by_user_id=? WHERE id=?`, w.owner.ID, it.ID)
	}
	a := w.bootstrap(t, w.tok, nil).Agenda
	if a == nil || len(a.NoDate) != 10 || a.NoDateMoreCount != 190 || a.NoDateComplete || a.PriorityListComplete || a.WaitingOnComplete {
		t.Fatalf("bootstrap must scan only 200 candidates: %+v", a)
	}
}

func TestBootstrapAgendaWorkUsesInitialDeadline(t *testing.T) {
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	it := w.item(t, "tasks", "Unplaced", `{}`, nil)
	meAgendaExec(t, w.agendaWorld, `UPDATE items SET created_by_user_id=? WHERE id=?`, w.owner.ID, it.ID)
	now := meAgendaNow
	calls := 0
	w.srv.agendaClock = func() time.Time {
		calls++
		if calls == 1 {
			return now
		}
		return now.Add(2 * time.Second)
	}
	a := w.bootstrap(t, w.tok, nil).Agenda
	if a == nil || len(a.NoDate) != 0 || a.NoDateComplete || a.PriorityListComplete || a.WaitingOnComplete {
		t.Fatalf("bootstrap work restarted deadline: %+v", a)
	}
}
