package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// TASK-4103 C2b-ii r3 fixes. Each test is red at 3d50ef5e6: production passes
// time.Now() with nanoseconds, and the resolve tests' clock now does too.

// completionAtClock is completionAt with an exact clock.
func (f *schedFixture) completionAtClock(t *testing.T, in models.ScheduleCreate, x string, nowT time.Time) *models.ItemSchedule {
	t.Helper()
	item, sc := f.mk(t, in, "2026-10-01T00:00:00Z")
	xt := rt(x)
	f.exec(t, `UPDATE item_schedules SET next_at = ?, next_wall = ? WHERE id = ?`, x, xt.Format("2006-01-02T15:04:05"), sc.ID)
	res, err := f.s.resolveOccurrenceAt(f.ws.ID, item.ID, ResolveInput{OccurrenceAt: xt, State: "done", Actor: "user"}, nowT)
	if err != nil {
		t.Fatalf("resolve at %s: %v", nowT.Format(time.RFC3339Nano), err)
	}
	return res.Schedule
}

// A repeated-half completion, three-hour grid: the next is done+3h exactly.
func TestCompletionNanosecondClockRepeatedHalf(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	got := f.completionAtClock(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-20T00:30", Tz: "America/New_York", Rrule: "FREQ=HOURLY;INTERVAL=3", Anchor: "completion"},
		"2026-11-01T14:00:00Z", rt("2026-11-01T15:30:00Z").Add(500*time.Millisecond))
	if got.NextAt == nil || *got.NextAt != "2026-11-01T18:30:00Z" {
		t.Errorf("next_at = %v, want 2026-11-01T18:30:00Z", deref(got.NextAt))
	}
}

// A huge INTERVAL never steps the start out of the years a wall can name.
func TestCompletionHugeIntervalNanosecondClock(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	got := f.completionAtClock(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-20T00:30", Tz: "America/New_York", Rrule: "FREQ=HOURLY;INTERVAL=300000", Anchor: "completion"},
		"2026-11-01T14:00:00Z", rt("2026-11-01T15:30:00Z").Add(500*time.Millisecond))
	want := rt("2026-11-01T15:30:00Z").Add(300000 * time.Hour).Format(time.RFC3339)
	if got.NextAt == nil || *got.NextAt != want {
		t.Errorf("next_at = %v, want %s", deref(got.NextAt), want)
	}
}

// The one whole-second row: a clock with no fraction behaves the same.
func TestCompletionWholeSecondClock(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	got := f.completionAtClock(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-20T00:30", Tz: "America/New_York", Rrule: "FREQ=HOURLY;INTERVAL=3", Anchor: "completion"},
		"2026-11-01T14:00:00Z", rt("2026-11-01T15:30:00Z"))
	if got.NextAt == nil || *got.NextAt != "2026-11-01T18:30:00Z" {
		t.Errorf("next_at = %v, want 2026-11-01T18:30:00Z", deref(got.NextAt))
	}
}

// transitions returns the instants in [2026, 2028) where loc's offset changes.
func transitions(loc *time.Location) []time.Time {
	var out []time.Time
	_, prev := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).In(loc).Zone()
	for at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC); at.Before(time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)); at = at.Add(time.Hour) {
		if _, off := at.In(loc).Zone(); off != prev {
			lo, hi := at.Add(-time.Hour), at // the change is in (lo, hi]
			for hi.Sub(lo) > time.Second {
				mid := lo.Add(hi.Sub(lo) / 2)
				if _, o := mid.In(loc).Zone(); o == prev {
					lo = mid
				} else {
					hi = mid
				}
			}
			out = append(out, hi.Truncate(time.Second))
			prev = off
		}
	}
	return out
}

// Every 2026 and 2027 transition, spans from a minute to three hours, done
// before, inside and after it, with sub-second digits: no error, next is
// exactly done+span, and the stored anchor reads back to an instant of the grid.
// Kathmandu has no transition in those years, so it is pinned at exactly 0 and
// runs at two fixed instants instead, for its +05:45 offset (local 00:00, 11:45).
func TestCompletionElapsedTransitionTable(t *testing.T) {
	t.Parallel()
	zones := []struct {
		name  string
		min   int
		synth []time.Time
	}{{"America/New_York", 4, nil}, {"Europe/London", 4, nil}, {"Australia/Lord_Howe", 4, nil},
		{"Asia/Kathmandu", 0, []time.Time{rt("2026-03-14T18:15:00Z"), rt("2026-11-01T06:00:00Z")}}}
	spans := []struct {
		rule string
		d    time.Duration
	}{{"FREQ=MINUTELY", time.Minute}, {"FREQ=MINUTELY;INTERVAL=7", 7 * time.Minute}, {"FREQ=HOURLY", time.Hour}, {"FREQ=HOURLY;INTERVAL=2", 2 * time.Hour}, {"FREQ=HOURLY;INTERVAL=3", 3 * time.Hour}}
	offsets := []time.Duration{-150 * time.Minute, -61 * time.Minute, -30 * time.Minute, -time.Second, 0, time.Second, 29*time.Minute + 59*time.Second, 59 * time.Minute, 90 * time.Minute, 150 * time.Minute}
	ran, want := 0, 0
	for _, z := range zones {
		loc, err := time.LoadLocation(z.name)
		if err != nil {
			t.Fatal(err)
		}
		trs := transitions(loc)
		if len(trs) < z.min {
			t.Fatalf("%s: found %d transitions, want at least %d", z.name, len(trs), z.min)
		}
		if z.synth != nil {
			if len(trs) != 0 {
				t.Fatalf("%s: found %d transitions, want 0 for a zone run at fixed instants", z.name, len(trs))
			}
			trs = z.synth
		}
		want += len(trs) * len(spans) * len(offsets)
		f := newSchedFixture(t)
		for _, tr := range trs {
			for _, sp := range spans {
				for _, off := range offsets {
					done := tr.Add(off).Add(123456789 * time.Nanosecond)
					name := fmt.Sprintf("%s %s %s %s %s", z.name, tr.Format(time.RFC3339), sp.rule, off, done.Format(time.RFC3339Nano))
					x := done.Truncate(time.Second).Add(-time.Hour).Format(time.RFC3339)
					got := f.completionAtClock(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-20T00:30", Tz: z.name, Rrule: sp.rule, Anchor: "completion"}, x, done)
					ran++
					d0 := done.Truncate(time.Second)
					if got.NextAt == nil || *got.NextAt != d0.Add(sp.d).Format(time.RFC3339) {
						t.Errorf("%s: next_at = %v, want %s", name, deref(got.NextAt), d0.Add(sp.d).Format(time.RFC3339))
					}
					if got.AnchorWall == nil {
						t.Errorf("%s: no anchor_wall", name)
						continue
					}
					w, err := schedule.ParseWall(*got.AnchorWall)
					if err != nil {
						t.Fatal(err)
					}
					a := schedule.Resolve(loc, w)
					k := (time.Hour + sp.d - 1) / sp.d
					if !a.Equal(d0) && !a.Equal(d0.Add(sp.d)) && !a.Equal(d0.Add(-k*sp.d)) {
						t.Errorf("%s: anchor_wall %s resolves to %s, not on the {done, done+span, done-k*span} choice", name, *got.AnchorWall, a.Format(time.RFC3339))
					}
					if g := wallOf(a.In(loc)); g != w {
						t.Errorf("%s: anchor_wall %s does not read back to its own instant (%s)", name, *got.AnchorWall, a.Format(time.RFC3339))
					}
				}
			}
		}
	}
	t.Logf("transition table ran %d resolves", ran)
	if ran != want || ran < 3*4*5*10+2*5*10 {
		t.Errorf("table ran %d rows, want %d and at least %d", ran, want, 3*4*5*10+2*5*10)
	}
}
