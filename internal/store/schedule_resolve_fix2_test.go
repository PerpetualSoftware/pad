package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// TASK-4103 C2b-ii r2 fixes. Each test is red at f3aa962ef.

func (f *schedFixture) completionAt(t *testing.T, in models.ScheduleCreate, x, nowT string) *models.ItemSchedule {
	t.Helper()
	item, sc := f.mk(t, in, "2026-10-01T00:00:00Z")
	xt := rt(x)
	f.exec(t, `UPDATE item_schedules SET next_at = ?, next_wall = ? WHERE id = ?`, x, xt.Format("2006-01-02T15:04:05"), sc.ID)
	return f.mustResolve(t, item, ResolveInput{OccurrenceAt: xt, State: "done", Actor: "user"}, nowT).Schedule
}

// r2-1: a completion resolved early never re-arms the occurrence it closed.
func TestCompletionNextIsStrictlyAfterResolvedOccurrence(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	got := f.completionAt(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-20T00:30", Tz: "America/New_York", Rrule: "FREQ=HOURLY", Anchor: "completion"},
		"2026-10-20T06:30:00Z", "2026-10-20T05:30:00Z")
	if got.NextAt == nil || *got.NextAt != "2026-10-20T07:30:00Z" {
		t.Errorf("hourly: next_at = %v, want 2026-10-20T07:30:00Z", deref(got.NextAt))
	}
	got = f.completionAt(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-20T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", Anchor: "completion"},
		"2026-10-21T09:00:00Z", "2026-10-20T08:00:00Z")
	if got.NextAt == nil || *got.NextAt != "2026-10-22T09:00:00Z" {
		t.Errorf("daily: next_at = %v, want 2026-10-22T09:00:00Z", deref(got.NextAt))
	}
}

// r2-2: an elapsed completion grid is phased from an unambiguous start, so the
// next is exactly done+span and anchor_wall names its own instant.
func TestCompletionElapsedIntervalAcrossFallBack(t *testing.T) {
	t.Parallel()
	ny, _ := time.LoadLocation("America/New_York")
	for _, c := range []struct{ name, rule, done, next, start string }{
		{"hourly interval 2", "FREQ=HOURLY;INTERVAL=2", "2026-11-01T04:10:00Z", "2026-11-01T06:10:00Z", "2026-11-01T04:10:00Z"},
		{"minutely 7 in repeated half", "FREQ=MINUTELY;INTERVAL=7", "2026-11-01T06:20:00Z", "2026-11-01T06:27:00Z", "2026-11-01T05:17:00Z"},
	} {
		f := newSchedFixture(t)
		got := f.completionAt(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-20T00:30", Tz: "America/New_York", Rrule: c.rule, Anchor: "completion"}, c.done, c.done)
		if got.NextAt == nil || *got.NextAt != c.next {
			t.Errorf("%s: next_at = %v, want %s", c.name, deref(got.NextAt), c.next)
		}
		if got.AnchorWall == nil {
			t.Fatalf("%s: no anchor_wall", c.name)
		}
		w, err := schedule.ParseWall(*got.AnchorWall)
		if err != nil {
			t.Fatal(err)
		}
		if g := schedule.Resolve(ny, w); g != rt(c.start) {
			t.Errorf("%s: anchor_wall %s resolves to %s, want %s", c.name, *got.AnchorWall, g.Format("2006-01-02T15:04:05Z"), c.start)
		}
	}
}
