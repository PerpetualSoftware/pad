package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestBootstrapAgendaSkipsUnreadableSchedule(t *testing.T) {
	for _, corruption := range []string{"wall", "zone", "override"} {
		t.Run(corruption, func(t *testing.T) {
			w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
			good := w.sched(t, w.owner, "Good", "2031-03-10T09:00", "both")
			bad := w.sched(t, w.owner, "Bad", "2031-03-10T09:00", "both")
			switch corruption {
			case "wall":
				meAgendaExec(t, w.agendaWorld, `UPDATE item_schedules SET next_wall='bad' WHERE item_id=?`, bad.ID)
			case "zone":
				meAgendaExec(t, w.agendaWorld, `UPDATE item_schedules SET tz='Bad/Zone' WHERE item_id=?`, bad.ID)
			case "override":
				meAgendaExec(t, w.agendaWorld, `INSERT INTO schedule_occurrence_overrides(id,schedule_id,occurrence_key,role,value_at,created_at,updated_at) VALUES(?,?,?,'target','bad',?,?)`, bad.ID, w.scheduleID(t, bad.ID), "2031-03-10T09:00:00Z|2031-03-10T09:00:00", "2031-03-10T00:00:00Z", "2031-03-10T00:00:00Z")
			}
			a := w.bootstrap(t, w.tok, nil).Agenda
			if a == nil || len(a.DueToday) != 1 || a.DueToday[0].Ref != good.Ref {
				t.Fatalf("readable row lost: %+v", a)
			}
			if a.DueTodayComplete || a.CarriedComplete || a.StaleComplete {
				t.Fatalf("unreadable row reported complete: %+v", a)
			}
		})
	}
}

func TestStaleAgendaSkipsUnreadableSchedule(t *testing.T) {
	w := newBootAgendaWorld(t, "2031-03-30T12:00:00Z")
	good := w.sched(t, w.owner, "Good", "2031-03-01T09:00", "both")
	bad := w.sched(t, w.owner, "Bad", "2031-03-01T09:00", "both")
	meAgendaExec(t, w.agendaWorld, `UPDATE item_schedules SET next_wall='bad' WHERE item_id=?`, bad.ID)
	rr := doRequestWithHeaders(w.srv, "GET", "/api/v1/workspaces/"+w.ws.Slug+"/agenda?stale=true", nil, map[string]string{"Cookie": "pad_session=" + w.tok})
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var a staleAgendaResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if len(a.Entries) != 1 || a.Entries[0].Ref != good.Ref || !a.Truncated {
		t.Fatalf("stale partial read: %+v", a)
	}
}

func TestMeNeedsSkipsUnreadableSchedule(t *testing.T) {
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	f := newNfx(t, w.srv)
	viewer := f.user("recipient")
	f.member(w.ws, viewer, "editor")
	good := w.sched(t, viewer, "Good", "2031-03-10T09:00", "human")
	bad := w.sched(t, viewer, "Bad", "2031-03-10T09:00", "human")
	for _, id := range []string{good.ID, bad.ID} {
		meAgendaExec(t, w.agendaWorld, `UPDATE items SET created_at='2031-03-10T10:00:00Z' WHERE id=?`, id)
	}
	meAgendaExec(t, w.agendaWorld, `UPDATE item_schedules SET next_wall='bad' WHERE item_id=?`, bad.ID)
	got := f.need(viewer, "")
	if len(got.Rows) != 1 || got.Rows[0].Item.Ref != good.Ref {
		t.Fatalf("needs partial read: %+v", got)
	}
}
