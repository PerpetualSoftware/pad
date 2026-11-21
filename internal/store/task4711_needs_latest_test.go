package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4711: once a snooze ends, the re-fired reminder is the newest claim, so
// Latest reads the re-fire instant (later than the occurrence's due instant).
// TASK-4683: a repeating reminder's later occurrences that came due count for
// Latest while the oldest one stays unresolved.

func needsDueOf(t *testing.T, f *bootFixture, itemID string, now time.Time) (NeedsDue, bool) {
	t.Helper()
	got, err := f.s.NeedsDueOccurrences(f.ws.ID, CatchUpScope{}, f.me.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := got[itemID]
	return d, ok
}

func TestTASK4711NeedsLatestSnoozeRefire(t *testing.T) {
	f := newBootFixture(t)
	due := rt("2031-03-10T09:00:00Z")
	it, _ := f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC", LeadMinutes: 30}, "2031-03-09T00:00:00Z")
	refire := rt("2031-03-12T09:00:00Z")
	f.mustResolve(t, it, ResolveInput{OccurrenceAt: due, State: "snooze", Until: &refire, Actor: "user"}, "2031-03-10T11:00:00Z")
	if d, ok := needsDueOf(t, f, it.ID, refire.Add(-time.Second)); ok {
		t.Fatalf("snoozed: due = %+v", d)
	}
	for _, now := range []time.Time{refire, refire.Add(time.Hour)} {
		d, ok := needsDueOf(t, f, it.ID, now)
		if !ok || !d.Earliest.Equal(due) || !d.Latest.Equal(refire) {
			t.Fatalf("at %s: due = %+v %v, want earliest %s latest %s", now, d, ok, due, refire)
		}
	}
}

func TestTASK4711NeedsLatestMovedSnoozeRefire(t *testing.T) {
	f := newBootFixture(t)
	item, sc := f.mkFor(t, f.me.ID, t4590Create(false), "2026-10-29T00:00:00Z")
	t4590MoveSnooze(t, f.schedFixture, item, sc)
	d1, s := rt(t4590D1), rt(t4590S)
	if d, ok := needsDueOf(t, f, item.ID, s.Add(-time.Second)); ok {
		t.Fatalf("snoozed: due = %+v", d)
	}
	d, ok := needsDueOf(t, f, item.ID, s.Add(time.Minute))
	if !ok || !d.Earliest.Equal(d1) || !d.Latest.Equal(s) {
		t.Fatalf("due = %+v %v, want earliest %s latest %s", d, ok, d1, s)
	}
}

func TestTASK4683NeedsLatestRepeating(t *testing.T) {
	f := newBootFixture(t)
	it, _ := f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-07T09:00", Tz: "UTC", Audience: "human", Rrule: "FREQ=DAILY"}, "2031-03-06T00:00:00Z")
	oldest := rt("2031-03-07T09:00:00Z")
	for _, tc := range []struct{ now, latest string }{
		{"2031-03-07T12:00:00Z", "2031-03-07T09:00:00Z"},
		{"2031-03-09T12:00:00Z", "2031-03-09T09:00:00Z"},
		{"2031-03-09T09:00:00Z", "2031-03-09T09:00:00Z"},
		{"2031-03-09T08:59:59Z", "2031-03-08T09:00:00Z"},
	} {
		d, ok := needsDueOf(t, f, it.ID, rt(tc.now))
		if !ok || !d.Earliest.Equal(oldest) || !d.Latest.Equal(rt(tc.latest)) {
			t.Fatalf("at %s: due = %+v %v, want earliest %s latest %s", tc.now, d, ok, oldest, tc.latest)
		}
	}
}

// A walk capped at 500 instances (a 15-minute rule overdue ten days) or at its
// 92-day window (a weekly rule overdue 200 days) still finds the newest
// occurrence due by now.
func TestTASK4683NeedsLatestCappedWalk(t *testing.T) {
	for _, tc := range []struct {
		name, rule, now, latest string
	}{
		{"instances", "FREQ=MINUTELY;INTERVAL=15", "2031-03-17T10:20:00Z", "2031-03-17T10:15:00Z"},
		{"window", "FREQ=WEEKLY", "2031-09-23T12:00:00Z", "2031-09-19T09:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBootFixture(t)
			it, _ := f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-07T09:00", Tz: "UTC", Audience: "human", Rrule: tc.rule}, "2031-03-06T00:00:00Z")
			d, ok := needsDueOf(t, f, it.ID, rt(tc.now))
			if !ok || !d.Earliest.Equal(rt("2031-03-07T09:00:00Z")) || !d.Latest.Equal(rt(tc.latest)) {
				t.Fatalf("due = %+v %v, want latest %s", d, ok, tc.latest)
			}
		})
	}
}
