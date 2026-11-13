package store

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestNeedsDueOccurrences(t *testing.T) {
	f := newBootFixture(t)
	now := rt("2031-03-10T12:00:00Z")
	due := rt("2031-03-10T09:00:00Z")
	want := map[string]time.Time{}
	for _, tc := range []struct {
		name, audience, action            string
		away, assigned, deleted, terminal bool
		want                              bool
	}{
		{name: "human", audience: "human", want: true},
		{name: "both", audience: "both", want: true},
		{name: "agent", audience: "agent"},
		{name: "ack", audience: "human", action: "ack"},
		{name: "done", audience: "human", action: "done"},
		{name: "skip", audience: "human", action: "skip"},
		{name: "move", audience: "human", action: "move"},
		{name: "snooze", audience: "human", action: "snooze", want: true},
		{name: "future", audience: "human", action: "future"},
		{name: "assigned", audience: "human", assigned: true, want: true},
		{name: "assigned away", audience: "human", away: true},
		{name: "deleted", audience: "human", deleted: true},
		{name: "terminal", audience: "human", terminal: true},
	} {
		in := models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC", Audience: tc.audience}
		if tc.action == "future" {
			in.Dtstart = "2031-03-11T09:00"
		}
		creator := f.me.ID
		if tc.assigned {
			creator = f.other.ID
		}
		it, _ := f.mkFor(t, creator, in, "2031-03-09T00:00:00Z")
		if tc.assigned {
			f.exec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, f.me.ID, it.ID)
		}
		if tc.away {
			f.exec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, f.other.ID, it.ID)
		}
		if tc.deleted {
			f.exec(t, `UPDATE items SET deleted_at = ? WHERE id = ?`, now.Format(time.RFC3339), it.ID)
		}
		if tc.terminal {
			f.exec(t, `UPDATE items SET fields = ? WHERE id = ?`, `{"status":"done"}`, it.ID)
		}
		if tc.action != "" && tc.action != "future" {
			until := now.Add(time.Hour)
			input := ResolveInput{OccurrenceAt: due, State: tc.action, Actor: "user"}
			if tc.action == "move" || tc.action == "snooze" {
				input.Until = &until
			}
			if tc.action == "ack" {
				f.exec(t, `UPDATE item_reminders SET fired_at=? WHERE item_id=?`, due.Format(time.RFC3339), it.ID)
			}
			f.mustResolve(t, it, input, "2031-03-10T11:00:00Z")
		}
		if tc.want {
			want[it.ID] = due
		}
	}
	got, err := f.s.NeedsDueOccurrences(f.ws.ID, CatchUpScope{}, f.me.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("due=%v; want %v", got, want)
	}
	for id, at := range want {
		if !got[id].Equal(at) {
			t.Errorf("%s due=%s want %s", id, got[id], at)
		}
	}
	if got, err := f.s.NeedsDueOccurrences(f.ws.ID, CatchUpScope{CollectionIDs: []string{}}, f.me.ID, now); err != nil || len(got) != 0 {
		t.Fatalf("empty scope = %v %v", got, err)
	}
}

func TestNeedsDueOccurrencesMoved(t *testing.T) {
	f := newBootFixture(t)
	it, _ := f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC"}, "2031-03-09T00:00:00Z")
	moved := rt("2031-03-10T13:00:00Z")
	f.mustResolve(t, it, ResolveInput{OccurrenceAt: rt("2031-03-10T09:00:00Z"), State: "move", Until: &moved, Actor: "user"}, "2031-03-10T11:00:00Z")
	for _, at := range []time.Time{moved.Add(-time.Second), moved, moved.Add(time.Hour)} {
		got, err := f.s.NeedsDueOccurrences(f.ws.ID, CatchUpScope{}, f.me.ID, at)
		if err != nil {
			t.Fatal(err)
		}
		if at.Before(moved) {
			if len(got) != 0 {
				t.Fatalf("before move due = %v", got)
			}
		} else if len(got) != 1 || !got[it.ID].Equal(moved) {
			t.Fatalf("at %s due = %v", at, got)
		}
	}
}

func TestNeedsDueOccurrencesScopeBeforeCap(t *testing.T) {
	f := newBootFixture(t)
	due := rt("2031-03-10T09:00:00Z")
	var last *models.Item
	for i := 0; i < bootstrapAgendaScan+1; i++ {
		last, _ = f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC"}, "2031-03-09T00:00:00Z")
	}
	var log bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	defer slog.SetDefault(old)
	got, err := f.s.NeedsDueOccurrences(f.ws.ID, CatchUpScope{CollectionIDs: []string{}, ItemIDs: []string{last.ID}}, f.me.ID, due)
	if err != nil || len(got) != 1 || !got[last.ID].Equal(due) || log.Len() != 0 {
		t.Fatalf("scoped due=%v err=%v log=%s", got, err, log.String())
	}
	got, err = f.s.NeedsDueOccurrences(f.ws.ID, CatchUpScope{}, f.me.ID, due)
	if err != nil || len(got) != bootstrapAgendaScan || !strings.Contains(log.String(), "truncated") {
		t.Fatalf("cap count=%d err=%v log=%s", len(got), err, log.String())
	}
	log.Reset()
	f.exec(t, `UPDATE item_reminders SET fired_at=?,acked_at=? WHERE workspace_id=? AND item_id<>?`, due.Format(time.RFC3339), due.Format(time.RFC3339), f.ws.ID, last.ID)
	got, err = f.s.NeedsDueOccurrences(f.ws.ID, CatchUpScope{}, f.me.ID, due)
	if err != nil || len(got) != 1 || !got[last.ID].Equal(due) || log.Len() != 0 {
		t.Fatalf("ack exclusion before cap: due=%v err=%v log=%s", got, err, log.String())
	}
	f.exec(t, `UPDATE item_reminders SET acked_at=NULL WHERE workspace_id=?`, f.ws.ID)
	log.Reset()
	f.exec(t, `UPDATE items SET fields = ? WHERE workspace_id = ? AND id <> ?`, `{"status":"done"}`, f.ws.ID, last.ID)
	got, err = f.s.NeedsDueOccurrences(f.ws.ID, CatchUpScope{}, f.me.ID, due)
	if err != nil || len(got) != 1 || !got[last.ID].Equal(due) || log.Len() != 0 {
		t.Fatalf("terminal filter before cap: due=%v err=%v log=%s", got, err, log.String())
	}

}

func TestNeedsCandidateDueCreator(t *testing.T) {
	f := newBootFixture(t)
	it, _ := f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC"}, "2031-03-09T00:00:00Z")
	got, err := f.s.NeedsCandidateItems(f.ws.ID, CatchUpScope{}, NeedsCandidateParams{ForUserID: f.me.ID, DueItemIDs: []string{it.ID}})
	if err != nil || len(got) != 1 || got[0].ID != it.ID {
		t.Fatalf("creator candidate = %v %v", got, err)
	}
	got, err = f.s.NeedsCandidateItems(f.ws.ID, CatchUpScope{CollectionIDs: []string{}}, NeedsCandidateParams{ForUserID: f.me.ID, DueItemIDs: []string{it.ID}})
	if err != nil || len(got) != 0 {
		t.Fatalf("hidden due = %v %v", got, err)
	}
}

func TestNeedsDueOccurrencesViewerZone(t *testing.T) {
	f := newBootFixture(t)
	if _, err := f.s.SetUserTimezoneIfUnset(f.me.ID, "America/Los_Angeles"); err != nil {
		t.Fatal(err)
	}
	it, _ := f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Floating: true}, "2031-03-09T00:00:00Z")
	due := rt("2031-03-10T16:00:00Z")
	for _, now := range []time.Time{due.Add(-time.Second), due} {
		got, err := f.s.NeedsDueOccurrences(f.ws.ID, CatchUpScope{}, f.me.ID, now)
		if err != nil {
			t.Fatal(err)
		}
		if now.Before(due) {
			if len(got) != 0 {
				t.Fatalf("not due in viewer zone: %v", got)
			}
		} else if !got[it.ID].Equal(due) {
			t.Fatalf("due in viewer zone = %v", got)
		}
	}
}
