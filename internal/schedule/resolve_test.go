package schedule

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func mustWall(t *testing.T, s string) Wall {
	t.Helper()
	w, err := ParseWall(s)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestResolve(t *testing.T) {
	rows := []struct {
		name, zone, wall, want string
	}{
		{"ny-normal-winter", "America/New_York", "2025-01-15T09:00", "2025-01-15T14:00:00Z"},
		{"ny-normal-summer", "America/New_York", "2025-07-15T09:00", "2025-07-15T13:00:00Z"},
		{"ny-gap-start", "America/New_York", "2025-03-09T02:00", "2025-03-09T07:00:00Z"},
		{"ny-gap-mid", "America/New_York", "2025-03-09T02:30", "2025-03-09T07:30:00Z"},
		{"ny-gap-last-second", "America/New_York", "2025-03-09T02:59:59", "2025-03-09T07:59:59Z"},
		{"ny-after-gap", "America/New_York", "2025-03-09T03:00", "2025-03-09T07:00:00Z"},
		{"ny-before-gap", "America/New_York", "2025-03-09T01:59", "2025-03-09T06:59:00Z"},
		{"ny-overlap-first-minute", "America/New_York", "2025-11-02T01:00", "2025-11-02T05:00:00Z"},
		{"ny-overlap-mid", "America/New_York", "2025-11-02T01:30", "2025-11-02T05:30:00Z"},
		{"ny-after-overlap", "America/New_York", "2025-11-02T02:00", "2025-11-02T07:00:00Z"},
		{"london-gap", "Europe/London", "2025-03-30T01:30", "2025-03-30T01:30:00Z"},
		{"london-overlap", "Europe/London", "2025-10-26T01:30", "2025-10-26T00:30:00Z"},
		{"lord-howe-gap", "Australia/Lord_Howe", "2025-10-05T02:15", "2025-10-04T15:45:00Z"},
		{"lord-howe-overlap", "Australia/Lord_Howe", "2025-04-06T01:45", "2025-04-05T14:45:00Z"},
		{"kolkata", "Asia/Kolkata", "2025-06-01T12:00", "2025-06-01T06:30:00Z"},
		{"utc", "UTC", "2025-06-01T12:00", "2025-06-01T12:00:00Z"},
		{"allday-ny", "America/New_York", "2025-03-09", "2025-03-09T05:00:00Z"},
		{"allday-ny-after-dst", "America/New_York", "2025-03-16", "2025-03-16T04:00:00Z"},
		{"allday-midnight-gap-havana", "America/Havana", "2024-03-10", "2024-03-10T05:00:00Z"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got := Resolve(mustLoc(t, r.zone), mustWall(t, r.wall))
			want, err := time.Parse(time.RFC3339, r.want)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(want) {
				t.Errorf("Resolve(%s, %s) = %s; want %s", r.zone, r.wall, got.UTC().Format(time.RFC3339), r.want)
			}
		})
	}
	if got := Resolve(nil, mustWall(t, "2025-06-01T12:00")); !got.Equal(time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("nil loc = %s; want UTC", got)
	}
}

// TestResolveMonotonic walks every minute of the 48 hours around each
// transition. Outside a gap a later wall reading never resolves to an earlier
// instant. Inside a gap the readings shift forward, so they ascend among
// themselves and land at or after the gap's end, which makes the sequence
// non-monotonic by exactly the gap length at the gap's last edge: the
// iterator in the engine must not assume otherwise (TASK-4100).
func TestResolveMonotonic(t *testing.T) {
	cases := []struct{ zone, day string }{
		{"America/New_York", "2025-03-09"}, {"America/New_York", "2025-11-02"},
		{"Europe/London", "2025-03-30"}, {"Europe/London", "2025-10-26"},
		{"Australia/Lord_Howe", "2025-10-05"}, {"Australia/Lord_Howe", "2025-04-06"},
		{"America/Havana", "2024-03-10"}, {"Asia/Kolkata", "2025-06-01"},
	}
	for _, c := range cases {
		loc := mustLoc(t, c.zone)
		day := mustWall(t, c.day).naive()
		var prevReal, prevGap time.Time
		gapSeen := 0
		for m := -24 * 60; m < 24*60; m++ {
			n := day.Add(time.Duration(m) * time.Minute)
			w := Wall{Year: n.Year(), Month: n.Month(), Day: n.Day(), Hour: n.Hour(), Minute: n.Minute()}
			got := Resolve(loc, w)
			if got.In(loc).Format("2006-01-02T15:04") == n.Format("2006-01-02T15:04") {
				if !prevReal.IsZero() && got.Before(prevReal) {
					t.Fatalf("%s %s: wall %s resolved to %s, before %s", c.zone, c.day, n.Format("2006-01-02T15:04"), got.UTC(), prevReal.UTC())
				}
				prevReal = got
				continue
			}
			gapSeen++ // the wall reading differs from the resolved clock: a gap reading
			if !prevGap.IsZero() && !got.After(prevGap) {
				t.Fatalf("%s %s: gap wall %s resolved to %s, not after %s", c.zone, c.day, n.Format("2006-01-02T15:04"), got.UTC(), prevGap.UTC())
			}
			prevGap = got
		}
		wantGap := map[string]bool{"2025-03-09": true, "2025-03-30": true, "2025-10-05": true, "2024-03-10": true}[c.day]
		if wantGap != (gapSeen > 0) {
			t.Errorf("%s %s: saw %d gap readings; want a gap: %v", c.zone, c.day, gapSeen, wantGap)
		}
	}
}
