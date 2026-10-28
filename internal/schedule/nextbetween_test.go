package schedule

import (
	"errors"
	"sync"
	"testing"
	"time"
)

const nyZone = "America/New_York"

func compile(t *testing.T, s Spec) *Compiled {
	t.Helper()
	c, err := Compile(s)
	if err != nil {
		t.Fatalf("Compile(%+v): %v", s, err)
	}
	return c
}

// pinned is a timed schedule pinned to zone.
func pinned(t *testing.T, zone, start, rule string) Spec {
	t.Helper()
	return Spec{Start: mustWall(t, start), Zone: zone, Rule: rule}
}

func inst(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func ats(occ []Occurrence) []string {
	out := make([]string, len(occ))
	for i, o := range occ {
		out[i] = o.At.UTC().Format(time.RFC3339)
	}
	return out
}

func wantAts(t *testing.T, got []Occurrence, want ...string) {
	t.Helper()
	g := ats(got)
	if len(g) != len(want) {
		t.Fatalf("got %v; want %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("got %v; want %v", g, want)
		}
	}
}

func TestNextAfterStrictlyAfter(t *testing.T) {
	c := compile(t, pinned(t, nyZone, "2025-01-06T09:00", "FREQ=DAILY"))
	o, ok, err := c.NextAfter(inst(t, "2025-01-07T14:00:00Z"), nil, Limits{}) // exactly the 09:00 EST instant
	if err != nil || !ok || o.At.Format(time.RFC3339) != "2025-01-08T14:00:00Z" {
		t.Fatalf("NextAfter at an occurrence = %v %v %v; want the next day", o.At, ok, err)
	}
}

func TestNextAfterBeforeStartReturnsSyncedStart(t *testing.T) {
	c := compile(t, pinned(t, nyZone, "2025-01-06T09:00", "FREQ=DAILY"))
	if !c.StartSynced() {
		t.Fatal("DAILY from Start must be synced")
	}
	o, ok, _ := c.NextAfter(inst(t, "2020-01-01T00:00:00Z"), nil, Limits{})
	if !ok || o.At.Format(time.RFC3339) != "2025-01-06T14:00:00Z" {
		t.Fatalf("got %v %v; want Start itself", o.At, ok)
	}
}

// TestNextAfterUnsyncedStartSkipsStart pins ruling Q1 (dateutil semantics): a
// "every Friday" schedule made on a Tuesday does not fire on the Tuesday.
func TestNextAfterUnsyncedStartSkipsStart(t *testing.T) {
	tuesday := "2025-10-28T09:00" // a Tuesday
	c := compile(t, pinned(t, nyZone, tuesday, "FREQ=WEEKLY;BYDAY=FR"))
	if c.StartSynced() {
		t.Error("StartSynced = true; a Tuesday Start is not produced by BYDAY=FR")
	}
	o, ok, err := c.NextAfter(inst(t, "2025-10-28T00:00:00Z"), nil, Limits{})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if got := o.At.Format(time.RFC3339); got != "2025-10-31T13:00:00Z" { // the following Friday 09:00 EDT
		t.Errorf("first occurrence = %s; want Friday 2025-10-31 09:00 EDT, not the Tuesday", got)
	}
	occ, _ := c.Between(inst(t, "2025-10-28T00:00:00Z"), inst(t, "2025-11-10T00:00:00Z"), nil, Limits{})
	wantAts(t, occ, "2025-10-31T13:00:00Z", "2025-11-07T14:00:00Z")
}

func TestNextAfterExhaustedCount(t *testing.T) {
	c := compile(t, pinned(t, nyZone, "2025-01-06T09:00", "FREQ=DAILY;COUNT=2"))
	if _, ok, err := c.NextAfter(inst(t, "2025-01-07T14:00:00Z"), nil, Limits{}); ok || err != nil {
		t.Fatalf("ok=%v err=%v; want exhausted", ok, err)
	}
}

func TestNextAfterExhaustedUntil(t *testing.T) {
	c := compile(t, pinned(t, nyZone, "2025-01-06T09:00", "FREQ=DAILY;UNTIL=20250107"))
	if _, ok, err := c.NextAfter(inst(t, "2025-01-07T14:00:00Z"), nil, Limits{}); ok || err != nil {
		t.Fatalf("ok=%v err=%v; want exhausted", ok, err)
	}
}

func TestNextAfterAcrossDST(t *testing.T) {
	c := compile(t, pinned(t, nyZone, "2025-03-08T02:30", "FREQ=DAILY"))
	o, _, _ := c.NextAfter(inst(t, "2025-03-08T08:00:00Z"), nil, Limits{})
	if got := o.At.Format(time.RFC3339); got != "2025-03-09T07:30:00Z" {
		t.Errorf("gap day = %s; want 07:30Z (02:30 shifted to 03:30 EDT)", got)
	}
	if o.Wall.String() != "2025-03-09T02:30:00" {
		t.Errorf("Wall = %s; the identity stays the wall reading", o.Wall)
	}
}

func TestNextAfterHonoursExDates(t *testing.T) {
	s := pinned(t, nyZone, "2025-01-06T09:00", "FREQ=DAILY")
	s.ExDates = []Wall{mustWall(t, "2025-01-07T09:00")}
	o, _, _ := compile(t, s).NextAfter(inst(t, "2025-01-06T14:00:00Z"), nil, Limits{})
	if got := o.At.Format(time.RFC3339); got != "2025-01-08T14:00:00Z" {
		t.Errorf("got %s; want the 8th (the 7th is excluded)", got)
	}
}

func TestBetweenHalfOpen(t *testing.T) {
	c := compile(t, pinned(t, "UTC", "2025-01-01T09:00", "FREQ=DAILY"))
	occ, err := c.Between(inst(t, "2025-01-02T09:00:00Z"), inst(t, "2025-01-04T09:00:00Z"), nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	wantAts(t, occ, "2025-01-02T09:00:00Z", "2025-01-03T09:00:00Z") // from included, to excluded
}

func TestBetweenEmptyAndInverted(t *testing.T) {
	c := compile(t, pinned(t, "UTC", "2025-01-01T09:00", "FREQ=DAILY"))
	a, b := inst(t, "2025-01-02T00:00:00Z"), inst(t, "2025-01-03T00:00:00Z")
	for _, w := range [][2]time.Time{{a, a}, {b, a}} {
		if occ, err := c.Between(w[0], w[1], nil, Limits{}); occ != nil || err != nil {
			t.Errorf("Between(%v, %v) = %v, %v; want nil, nil", w[0], w[1], occ, err)
		}
	}
}

// chainBetween rebuilds Between's window one NextAfter at a time.
func chainBetween(t *testing.T, c *Compiled, from, to time.Time, view *time.Location, lim Limits) []Occurrence {
	t.Helper()
	var out []Occurrence
	for at := from.Add(-time.Nanosecond); ; {
		o, ok, err := c.NextAfter(at, view, lim)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || !o.At.Before(to) {
			return out
		}
		out, at = append(out, o), o.At
	}
}

func TestBetweenEqualsChainedNextAfter(t *testing.T) {
	rules := []string{
		"FREQ=DAILY", "FREQ=DAILY;BYHOUR=1,2", "FREQ=WEEKLY;BYDAY=MO,WE,FR", "FREQ=WEEKLY;INTERVAL=2;BYDAY=TU",
		"FREQ=MONTHLY;BYMONTHDAY=-1", "FREQ=MONTHLY;BYDAY=1SU,-1SU", "FREQ=YEARLY;BYMONTH=3,11;BYDAY=2SU", "FREQ=HOURLY;INTERVAL=5",
		"FREQ=MINUTELY;INTERVAL=45", "FREQ=HOURLY;BYHOUR=1,2,3", "FREQ=MINUTELY;INTERVAL=20;BYHOUR=1,2", "FREQ=DAILY;COUNT=40",
		"FREQ=DAILY;UNTIL=20250320T120000Z", "FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1",
	}
	windows := [][2]string{{"2025-03-07T00:00:00Z", "2025-03-12T00:00:00Z"}, {"2025-10-31T00:00:00Z", "2025-11-04T00:00:00Z"}, {"2025-01-01T00:00:00Z", "2025-01-08T00:00:00Z"}}
	for _, rule := range rules {
		c := compile(t, pinned(t, nyZone, "2025-01-01T01:30", rule))
		for _, w := range windows {
			from, to := inst(t, w[0]), inst(t, w[1])
			got, err := c.Between(from, to, nil, Limits{MaxDuration: 20 * time.Second, MaxInstances: 100000})
			if err != nil {
				t.Fatalf("%s: %v", rule, err)
			}
			chain := chainBetween(t, c, from, to, nil, Limits{MaxDuration: 20 * time.Second, MaxInstances: 100000})
			if len(got) != len(chain) {
				t.Fatalf("%s %v: Between %d vs chained %d", rule, w, len(got), len(chain))
			}
			for i := range got {
				if got[i] != chain[i] {
					t.Fatalf("%s %v #%d: Between %v vs chained %v", rule, w, i, got[i], chain[i])
				}
			}
		}
	}
}

func TestBetweenDedupesByInstant(t *testing.T) {
	c := compile(t, pinned(t, nyZone, "2025-03-09T00:00", "FREQ=DAILY;BYHOUR=2,3;COUNT=3"))
	occ, err := c.Between(inst(t, "2025-03-09T00:00:00Z"), inst(t, "2025-03-11T00:00:00Z"), nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	wantAts(t, occ, "2025-03-09T07:00:00Z", "2025-03-10T06:00:00Z") // 02:00 and 03:00 on the gap day are one instant; COUNT=3 walls give 2 instants
}

func TestStartSynced(t *testing.T) {
	for _, tc := range []struct {
		start, rule string
		want        bool
	}{
		{"2025-10-28T09:00", "", true},
		{"2025-10-28T09:00", "FREQ=DAILY", true},
		{"2025-10-28T09:00", "FREQ=WEEKLY;BYDAY=TU", true},
		{"2025-10-28T09:00", "FREQ=WEEKLY;BYDAY=FR", false},
		{"2025-10-28T09:00", "FREQ=HOURLY;INTERVAL=3", true},
		{"2025-10-28T09:00", "FREQ=MONTHLY;BYMONTHDAY=1", false},
		{"2025-10-28T09:00", "FREQ=DAILY;BYHOUR=10", false},
	} {
		if got := compile(t, pinned(t, nyZone, tc.start, tc.rule)).StartSynced(); got != tc.want {
			t.Errorf("%s %q: StartSynced = %v; want %v", tc.start, tc.rule, got, tc.want)
		}
	}
}

func TestBoundedFlag(t *testing.T) {
	for rule, want := range map[string]bool{"": true, "FREQ=DAILY": false, "FREQ=DAILY;COUNT=3": true, "FREQ=DAILY;UNTIL=20300101": true} {
		if got := compile(t, pinned(t, nyZone, "2025-10-28T09:00", rule)).Bounded(); got != want {
			t.Errorf("%q: Bounded = %v; want %v", rule, got, want)
		}
	}
}

func TestCompiledConcurrentUse(t *testing.T) {
	c := compile(t, pinned(t, nyZone, "2025-01-01T01:30", "FREQ=DAILY;BYHOUR=1,2"))
	from, to := inst(t, "2025-03-01T00:00:00Z"), inst(t, "2025-04-01T00:00:00Z")
	base, err := c.Between(from, to, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.Between(from, to, nil, Limits{})
			if err != nil || len(got) != len(base) {
				t.Errorf("concurrent Between: %d vs %d, %v", len(got), len(base), err)
			}
		}()
	}
	wg.Wait()
}

// TestCompletionAnchorRebase pins the contract the completion-anchored
// cadence relies on: a new Spec started at the completion wall, then
// NextAfter(completion), gives the next occurrence after it.
func TestCompletionAnchorRebase(t *testing.T) {
	done := inst(t, "2025-03-04T18:20:00Z") // 13:20 EST
	rebased := compile(t, Spec{Start: mustWall(t, "2025-03-04T13:20"), Zone: nyZone, Rule: "FREQ=WEEKLY;INTERVAL=2"})
	o, ok, err := rebased.NextAfter(done, nil, Limits{})
	if err != nil || !ok || o.At.Format(time.RFC3339) != "2025-03-18T17:20:00Z" {
		t.Fatalf("got %v %v %v; want 2025-03-18T17:20Z (13:20 EDT, two weeks on, across the DST change)", o.At, ok, err)
	}
}

func TestAllDayOccurrencesAreDates(t *testing.T) {
	c := compile(t, Spec{Start: mustWall(t, "2025-03-02"), Zone: nyZone, Rule: "FREQ=DAILY;COUNT=3"})
	occ, _ := c.Between(inst(t, "2025-03-01T00:00:00Z"), inst(t, "2025-03-10T00:00:00Z"), nil, Limits{})
	wantAts(t, occ, "2025-03-02T05:00:00Z", "2025-03-03T05:00:00Z", "2025-03-04T05:00:00Z")
	for _, o := range occ {
		if !o.AllDay() || !o.Wall.AllDay {
			t.Errorf("%v: want an all-day occurrence", o)
		}
	}
}

func TestAllDayFloatingFollowsView(t *testing.T) {
	c := compile(t, Spec{Start: mustWall(t, "2025-03-02"), Floating: true, Rule: "FREQ=DAILY;COUNT=2"})
	for zone, want := range map[string]string{"America/New_York": "2025-03-02T05:00:00Z", "Asia/Tokyo": "2025-03-01T15:00:00Z"} {
		o, _, err := c.NextAfter(inst(t, "2025-02-01T00:00:00Z"), mustLoc(t, zone), Limits{})
		if err != nil || o.At.Format(time.RFC3339) != want {
			t.Errorf("%s: %v %v; want %s", zone, o.At, err, want)
		}
	}
}

func TestAllDayWeeklyNoDriftAcrossDST(t *testing.T) {
	c := compile(t, Spec{Start: mustWall(t, "2025-03-02"), Zone: nyZone, Rule: "FREQ=WEEKLY;COUNT=3"})
	occ, _ := c.Between(inst(t, "2025-03-01T00:00:00Z"), inst(t, "2025-04-01T00:00:00Z"), nil, Limits{})
	wantAts(t, occ, "2025-03-02T05:00:00Z", "2025-03-09T05:00:00Z", "2025-03-16T04:00:00Z")
	for i, d := range []string{"2025-03-02", "2025-03-09", "2025-03-16"} {
		if occ[i].Wall.String() != d {
			t.Errorf("Wall %d = %s; want %s", i, occ[i].Wall, d)
		}
	}
}

// TestAllDayMidnightGapShiftsForward uses America/Havana, whose DST starts at
// midnight: 2024-03-10 00:00 does not exist, so the date's At is 01:00 CDT,
// which is 05:00Z (verified against tzdata).
func TestAllDayMidnightGapShiftsForward(t *testing.T) {
	c := compile(t, Spec{Start: mustWall(t, "2024-03-09"), Zone: "America/Havana", Rule: "FREQ=DAILY;COUNT=3"})
	occ, err := c.Between(inst(t, "2024-03-01T00:00:00Z"), inst(t, "2024-03-20T00:00:00Z"), nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	wantAts(t, occ, "2024-03-09T05:00:00Z", "2024-03-10T05:00:00Z", "2024-03-11T04:00:00Z")
}

func TestExDateOnAllDay(t *testing.T) {
	s := Spec{Start: mustWall(t, "2025-03-02"), Zone: nyZone, Rule: "FREQ=DAILY;COUNT=3", ExDates: []Wall{mustWall(t, "2025-03-03")}}
	occ, _ := compile(t, s).Between(inst(t, "2025-03-01T00:00:00Z"), inst(t, "2025-03-10T00:00:00Z"), nil, Limits{})
	wantAts(t, occ, "2025-03-02T05:00:00Z", "2025-03-04T05:00:00Z")
}

func TestRDateAllDayMismatchRefused(t *testing.T) {
	for _, s := range []Spec{
		{Start: mustWall(t, "2025-03-02"), Zone: nyZone, RDates: []Wall{mustWall(t, "2025-03-03T09:00")}},
		{Start: mustWall(t, "2025-03-02T09:00"), Zone: nyZone, ExDates: []Wall{mustWall(t, "2025-03-03")}},
	} {
		_, err := Compile(s)
		var me *MalformedError
		if !errors.As(err, &me) || me.Code != CodeCombination {
			t.Errorf("err = %v; want CodeCombination", err)
		}
	}
}
