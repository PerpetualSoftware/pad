package server

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

func TestMeNeedsDueHumanOccurrence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, audience, action          string
		creator, away, terminal, hidden bool
		want                            bool
	}{
		{name: "human", audience: "human", want: true},
		{name: "both", audience: "both", want: true},
		{name: "agent", audience: "agent"},
		{name: "future", audience: "human", action: "future"},
		{name: "done", audience: "human", action: "done"},
		{name: "skip", audience: "human", action: "skip"},
		{name: "move", audience: "human", action: "move"},
		{name: "snooze", audience: "human", action: "snooze", want: true},
		{name: "creator", audience: "human", creator: true, want: true},
		{name: "assigned away", audience: "human", creator: true, away: true},
		{name: "terminal", audience: "human", terminal: true},
		{name: "hidden", audience: "human", hidden: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
			f := newNfx(t, w.srv)
			viewer := f.user("recipient")
			f.member(w.ws, viewer, "editor")
			it := w.sched(t, viewer, "Due item", "2031-03-10T09:00", tc.audience)
			meAgendaExec(t, w.agendaWorld, `UPDATE items SET created_at = ? WHERE id = ?`, "2031-03-10T10:00:00Z", it.ID)
			if tc.creator {
				meAgendaExec(t, w.agendaWorld, `UPDATE items SET assigned_user_id = NULL, created_by_user_id = ? WHERE id = ?`, viewer.ID, it.ID)
			}
			if tc.away {
				meAgendaExec(t, w.agendaWorld, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, w.owner.ID, it.ID)
			}
			if tc.terminal {
				meAgendaExec(t, w.agendaWorld, `UPDATE items SET fields = ? WHERE id = ?`, `{"status":"done"}`, it.ID)
			}
			if tc.hidden {
				if err := w.srv.store.SetMemberCollectionAccess(w.ws.ID, viewer.ID, "specific", []string{}); err != nil {
					t.Fatal(err)
				}
			}
			due := time.Date(2031, 3, 10, 9, 0, 0, 0, time.UTC)
			until := due.Add(48 * time.Hour)
			switch tc.action {
			case "future":
				meAgendaExec(t, w.agendaWorld, `UPDATE item_schedules SET next_at = ?, next_wall = ? WHERE item_id = ?`, until.Format(time.RFC3339), until.Format("2006-01-02T15:04:05"), it.ID)
			case "done", "skip":
				in := store.ResolveInput{OccurrenceAt: due, State: tc.action, Actor: "user"}
				if _, err := w.srv.store.ResolveOccurrence(w.ws.ID, it.ID, in); err != nil {
					t.Fatal(err)
				}
			case "move", "snooze":
				stamp := meAgendaNow.Format(time.RFC3339)
				meAgendaExec(t, w.agendaWorld, `UPDATE item_schedules SET next_override_at = ?, updated_at = ? WHERE item_id = ?`, until.Format(time.RFC3339), stamp, it.ID)
				if tc.action == "move" {
					meAgendaExec(t, w.agendaWorld, `INSERT INTO schedule_occurrences (id,schedule_id,occurrence_at,occurrence_wall,state,actor,resolved_at) VALUES (?,?,?,?,'moved','user',?)`, it.ID, w.scheduleID(t, it.ID), due.Format(time.RFC3339), "2031-03-10T09:00:00", stamp)
					// Current moves persist placement separately from reminder timing.
					key := store.OccurrenceKey(&models.ItemSchedule{}, due.Format(time.RFC3339), "2031-03-10T09:00:00")
					meAgendaExec(t, w.agendaWorld, `INSERT INTO schedule_occurrence_overrides(id,schedule_id,occurrence_key,role,value_at,created_at,updated_at) VALUES(?,?,?,'target',?,?,?)`, it.ID+"-target", w.scheduleID(t, it.ID), key, until.Format(time.RFC3339), stamp, stamp)
				}
			}
			got := f.need(viewer, "")
			want := 0
			if tc.want {
				want = 1
			}
			if got.Total != want || len(got.Rows) != want || len(got.Degraded) != 0 {
				t.Fatalf("needs = %+v; want %d", got, want)
			}
			if !tc.want {
				return
			}
			row := got.Rows[0]
			rule := "assigned"
			if tc.creator {
				rule = "due"
			}
			if row.Why.Rule != rule || !row.WaitingSince.Equal(due) {
				t.Fatalf("rule=%s waiting=%s", row.Why.Rule, row.WaitingSince)
			}
			if len(row.Because) != 1 || row.Because[0].Kind != "due" || !row.Because[0].At.Equal(due) {
				t.Fatalf("because = %+v", row.Because)
			}
		})
	}
}

func TestMeNeedsDueNotOnOtherPersonsRow(t *testing.T) {
	t.Parallel()
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	f := newNfx(t, w.srv)
	recipient := f.user("recipient")
	f.member(w.ws, recipient, "editor")
	yes := true
	if _, err := w.srv.store.UpdateCollection(w.colls["tasks"].ID, models.CollectionUpdate{AwaitsHuman: &yes}); err != nil {
		t.Fatal(err)
	}
	it := w.sched(t, recipient, "Ask fallback", "2031-03-10T09:00", "human")
	meAgendaExec(t, w.agendaWorld, `UPDATE items SET assigned_user_id = NULL, created_by_user_id = ? WHERE id = ?`, recipient.ID, it.ID)
	got := f.need(w.owner, "")
	if got.Total != 1 || got.Rows[0].Why.Rule != "admin_fallback" {
		t.Fatalf("owner = %+v", got)
	}
	for _, b := range got.Rows[0].Because {
		if b.Kind == "due" {
			t.Fatalf("recipient's due leaked: %+v", b)
		}
	}
	own := f.need(recipient, "")
	if own.Total != 1 || own.Rows[0].Why.Rule != "due" {
		t.Fatalf("recipient = %+v", own)
	}
}

func TestMeNeedsDueScopeBeforeCap(t *testing.T) {
	t.Parallel()
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	f := newNfx(t, w.srv)
	viewer := f.user("recipient")
	f.member(w.ws, viewer, "editor")
	hidden := f.collection(w.ws, "Hidden", false)
	if err := w.srv.store.SetMemberCollectionAccess(w.ws.ID, viewer.ID, "specific", []string{w.colls["tasks"].ID}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1001; i++ {
		it := w.sched(t, viewer, "Hidden due", "2031-03-10T09:00", "human")
		meAgendaExec(t, w.agendaWorld, `UPDATE items SET collection_id = ? WHERE id = ?`, hidden.ID, it.ID)
	}
	it := w.sched(t, viewer, "Visible due", "2031-03-10T11:00", "human")
	got := f.need(viewer, "")
	if got.Total != 1 || len(got.Rows) != 1 || got.Rows[0].Item.Ref != it.Ref || len(got.Degraded) != 0 {
		t.Fatalf("scoped due = %+v", got)
	}
}
