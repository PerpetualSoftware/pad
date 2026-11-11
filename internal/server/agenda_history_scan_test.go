package server

import (
	"fmt"
	"testing"
)

func TestAgendaHistoryOutsideWindowDoesNotConsumeScan(t *testing.T) {
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := w.srv.store.DB().Exec(w.srv.store.D().Rebind(query), args...); err != nil {
			t.Fatal(err)
		}
	}
	for n := 0; n < 1002; n++ {
		it := w.item(t, "tasks", fmt.Sprintf("History %04d", n), `{"status":"done"}`, nil)
		id := fmt.Sprintf("history-%04d", n)
		at, wall, offset := "2020-01-01T09:00:00Z", "2020-01-01T09:00:00", 1
		if n == 1001 {
			at, wall, offset = "2031-03-10T09:00:00Z", "2031-03-10T09:00:00", 0
		}
		exec(`INSERT INTO item_schedules (id,workspace_id,item_id,kind,dtstart,tz,ended_at,target_days,created_at,updated_at) VALUES (?,?,?,'check',?,'UTC',?,?,?,?)`, id, w.ws.ID, it.ID, wall, at, offset, at, at)
		exec(`INSERT INTO schedule_occurrences (id,schedule_id,occurrence_at,occurrence_wall,state,resolved_at) VALUES (?,?,?,?,'done',?)`, id, id, at, wall, at)
	}
	m := w.ok(t, w.owner, agendaWindow)
	if got := seen(m); got != "schedule:History 1001" || m["window_complete"] != true {
		t.Fatalf("history scan: entries=%s complete=%v truncated=%v", got, m["window_complete"], m["truncated"])
	}
}
