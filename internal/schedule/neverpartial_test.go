package schedule

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// neverCase is one schedule the instrument holds to the contract.
type neverCase struct {
	name  string
	spec  Spec
	never bool          // the rule never fires: it needs an RDATE to compile, and the oracle leaves it out. That it never fires is proved by TestReachabilityCheckIsExact (Compile refuses IFF the vendored engine, run directly, yields nothing within the lcm horizon), so a rule-less Set is the oracle
	group string        // named, rows or grid: for the coverage table
	win   time.Duration // Between window; 0 picks one by FREQ
	offs  []time.Duration
}

var neverSteps = []int64{1, 2, 5, 50, 1000, 0} // 0 is the default

// TestNeverAPartialAnswer is the instrument for the "CapStall, never a partial
// answer" contract (TASK-4100 r3 D4). Rounds r1 to r3 each found a partial
// answer or a starved stream by hand; this enumerates them. Every case runs
// NextAfter and Between under every MaxSteps, with no RDATE, an RDATE before the
// query point, one after it, and one that coincides with a rule occurrence; with
// no EXDATE, one on the first answer, and one on the RDATE; at three query
// points. The result must EITHER equal the vendored rrule.Set run unbudgeted
// (oracle_test.go) OR be a CapStall with Limit MaxSteps, and a CapStall is a
// failure where the rule alone gives no reason for it: any satisfiable case at
// the default MaxSteps, and any answer final below the rule's stall position.
// The oracle (oracle_test.go) runs the same vendored generator unbudgeted, so
// this test holds the merge, the stall position and the budget against the
// generator, not its recurrence output; that is the conformance table's job.
func TestNeverAPartialAnswer(t *testing.T) {
	day := 24 * time.Hour
	all := []time.Duration{0, 400 * day, -3} // -3: three years, by calendar
	stats := struct{ cases, stalls, answers int }{}
	neverCov = map[string]map[string]int{}
	defer func() { neverCov = nil }()

	t.Run("named", func(t *testing.T) {
		for _, tc := range []neverCase{
			// M1 (r3): MaxSteps=1 once dropped 03-01 from Between with no error.
			{name: "M1 monthly utc", spec: pinned(t, "UTC", "2096-01-20T00:00", "FREQ=MONTHLY;BYMONTH=1,3;BYMONTHDAY=1,25"), win: 45 * day, offs: []time.Duration{0}},
			{name: "M1 monthly tokyo", spec: pinned(t, "Asia/Tokyo", "2096-01-20T09:00", "FREQ=MONTHLY;BYMONTH=1,3;BYMONTHDAY=1,25"), win: 45 * day, offs: all},
			{name: "M1 monthly new_york", spec: pinned(t, nyZone, "2096-01-20T09:00", "FREQ=MONTHLY;BYMONTH=1,3;BYMONTHDAY=1,25"), win: 45 * day, offs: all},
			{name: "yearly feb29 tokyo", spec: pinned(t, "Asia/Tokyo", "2093-03-01T08:30", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29"), win: 60 * day, offs: all},
			{name: "weekly new_york", spec: pinned(t, nyZone, "2096-03-01T02:30", "FREQ=WEEKLY;BYDAY=SU;INTERVAL=3"), win: 45 * day, offs: all},
			// M2 (r3): an RDATE behind a rule that never fires.
			{name: "M2 minutely", spec: pinned(t, "UTC", "2025-01-01T00:00", "FREQ=MINUTELY;BYMONTH=1;BYYEARDAY=200"), never: true, win: 6 * time.Hour, offs: all},
			{name: "M2 hourly tokyo", spec: pinned(t, "Asia/Tokyo", "2025-01-01T00:00", "FREQ=HOURLY;BYMONTH=1;BYYEARDAY=200"), never: true, win: 6 * time.Hour, offs: all},
		} {
			stats.cases++
			tc.group = "named"
			runNever(t, tc, &stats.stalls, &stats.answers)
		}
		// M2's exact repro: the RDATE lies below the stall position at default limits.
		spec := pinned(t, "UTC", "2025-01-01T00:00", "FREQ=MINUTELY;BYMONTH=1;BYYEARDAY=200")
		spec.RDates = []Wall{mustWall(t, "2025-06-01T09:00")}
		c := compile(t, spec)
		q := inst(t, "2025-03-01T00:00:00Z")
		want := inst(t, "2025-06-01T09:00:00Z")
		if o, ok, err := c.NextAfter(q, nil, patient); err != nil || !ok || !o.At.Equal(want) {
			t.Errorf("M2 NextAfter = %v %v %v; want the RDATE %v", o.At, ok, err, want)
		}
		if occ, err := c.Between(inst(t, "2025-05-01T00:00:00Z"), inst(t, "2025-07-01T00:00:00Z"), nil, patient); err != nil || !sameTimes(gotTimes(occ), []time.Time{want}) {
			t.Errorf("M2 Between = %v %v; want [%v]", gotTimes(occ), err, want)
		}
	})

	t.Run("rows", func(t *testing.T) {
		rows := append(append(append([]Row{}, RFCRows...), RFCErratumRows...), ExtraRows...)
		for i, r := range rows {
			if raceSample && i%3 != 0 {
				continue
			}
			stats.cases++
			runNever(t, neverCase{name: r.Name, spec: specFromICal(t, r.ICal), offs: all, group: "rows"}, &stats.stalls, &stats.answers)
		}
	})

	t.Run("grid", func(t *testing.T) {
		n := 0
		reachabilityGrid(func(freq string, iv int, hs, ms []int, st [2]int) {
			if n++; !gridPick(n) {
				return
			}
			gridCover(freq, iv, hs, ms, st)
			w := Wall{Year: 2025, Month: time.March, Day: 3, Hour: st[0], Minute: st[1]}
			rule := fmt.Sprintf("FREQ=%s;INTERVAL=%d%s%s", freq, iv, list("BYHOUR", hs), list("BYMINUTE", ms))
			tc := neverCase{name: fmt.Sprintf("%s from %02d:%02d", rule, st[0], st[1]), spec: Spec{Start: w, Zone: "UTC", Rule: rule}, offs: all[:2], group: "grid"}
			if _, err := Compile(tc.spec); err != nil {
				tc.never = true
			}
			stats.cases++
			runNever(t, tc, &stats.stalls, &stats.answers)
		})
	})
	t.Logf("coverage (axis value: samples)\n%s", covTable(neverCov))
	for _, a := range []string{"freq", "interval", "byhour", "byminute", "start"} {
		if len(neverCov["grid "+a]) != gridAxisLen[a] {
			t.Errorf("grid axis %s: %d of %d values sampled", a, len(neverCov["grid "+a]), gridAxisLen[a])
		}
	}
	for a, n := range map[string]int{"rd": 4, "ex": 3, "steps": len(neverSteps), "rows off": 3, "grid off": 2, "named off": 3} {
		if len(neverCov[a]) != n {
			t.Errorf("axis %s: %d of %d values run", a, len(neverCov[a]), n)
		}
	}
	t.Logf("%d cases; %d CapStalls (all permitted), %d answers equal to the oracle", stats.cases, stats.stalls, stats.answers)
}

// runNever runs the variants of one case.
func runNever(t *testing.T, tc neverCase, stalls, answers *int) {
	t.Helper()
	freq := ""
	if tc.spec.Rule != "" {
		r, err := ParseRule(tc.spec.Rule)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		freq = r.Freq
	}
	win := tc.win
	if win == 0 {
		win = 6 * time.Hour
		switch freq {
		case freqYearly, freqMonthly, freqWeekly:
			win = 45 * 24 * time.Hour
		case freqDaily, "":
			win = 3 * 24 * time.Hour
		}
	}
	plain := tc.spec
	if tc.never {
		plain.RDates = []Wall{{Year: 2025, Month: time.January, Day: 2, Hour: 12, AllDay: tc.spec.Start.AllDay}}
	}
	pc, err := Compile(plain)
	if err != nil {
		t.Fatalf("%s: %v", tc.name, err)
	}
	zone := pc.zone
	start := Resolve(zone, tc.spec.Start)
	wallAt := func(at time.Time) Wall {
		at = at.Truncate(time.Minute)
		if tc.spec.Start.AllDay {
			at = at.Truncate(24 * time.Hour)
		}
		return wallOf(at.In(zone), tc.spec.Start.AllDay)
	}
	for _, off := range tc.offs {
		q := start.Add(off)
		if off == -3 {
			q = start.AddDate(3, 0, 0)
		}
		for _, rd := range []string{"none", "before", "after", "coincide"} {
			var rdates []Wall
			switch rd {
			case "none":
				if tc.never {
					continue
				}
			case "before":
				rdates = []Wall{wallAt(q.Add(-50 * time.Hour))}
			case "after":
				rdates = []Wall{wallAt(q.Add(win / 3))}
			case "coincide":
				if tc.never {
					continue
				}
				ref := refWindow(t, pc, q, 0, true)
				if len(ref) == 0 || tc.spec.Start.AllDay != ref[0].Wall.AllDay {
					continue
				}
				rdates = []Wall{ref[0].Wall}
			}
			for _, ex := range []string{"none", "first", "onRDATE"} {
				if ex == "onRDATE" && len(rdates) == 0 {
					continue
				}
				spec := tc.spec
				spec.RDates = append(append([]Wall(nil), spec.RDates...), rdates...)
				if tc.never && len(spec.RDates) == 0 {
					continue
				}
				c := compile(t, spec)
				switch ex {
				case "first":
					ref := refWindow(t, c, q, 0, !tc.never)
					if len(ref) == 0 {
						continue
					}
					spec.ExDates = append(append([]Wall(nil), spec.ExDates...), ref[0].Wall)
				case "onRDATE":
					spec.ExDates = append(append([]Wall(nil), spec.ExDates...), rdates[0])
				}
				c = compile(t, spec)
				cover("rd", rd)
				cover("ex", ex)
				offName := fmt.Sprint(off)
				if off == -3 {
					offName = "+3y"
				}
				cover(tc.group+" off", offName)
				name := fmt.Sprintf("%s rd=%s ex=%s q=%s", tc.name, rd, ex, q.Format("2006-01-02T15:04Z07:00"))
				neverCheck(t, name, c, tc.never, q, win, stalls, answers)
			}
		}
	}
}

// neverCheck holds one compiled schedule at one query point to the contract
// under every MaxSteps.
func neverCheck(t *testing.T, name string, c *Compiled, never bool, q time.Time, win time.Duration, stalls, answers *int) {
	t.Helper()
	wantN := refWindow(t, c, q, 0, !never)
	wantB := refWindow(t, c, q, win, !never)
	for _, steps := range neverSteps {
		cover("steps", fmt.Sprint(steps))
		lim := Limits{MaxSteps: steps, MaxDuration: 10 * time.Minute}
		eff := lim.fill().MaxSteps
		// verdict classifies an error: nil when it is a permitted CapStall.
		verdict := func(what string, err error, final time.Time, bounded bool) string {
			var ce *CapError
			if !errors.As(err, &ce) || ce.Kind != CapStall || ce.Limit != eff {
				return fmt.Sprintf("%s: %s err = %v; want the oracle's answer or a CapStall at MaxSteps %d", name, what, err, eff)
			}
			if steps == 0 && !never {
				return fmt.Sprintf("%s: %s CapStall at the default MaxSteps on a satisfiable case", name, what)
			}
			if bounded && !ruleStallsBy(t, c, c.zone, q, eff, final) {
				return fmt.Sprintf("%s: %s CapStall (MaxSteps %d) but the rule reaches past the answer's bound %s: a false cap", name, what, eff, final.Format(time.RFC3339))
			}
			*stalls++
			return ""
		}
		o, ok, err := c.NextAfter(q, nil, lim)
		switch {
		case err != nil:
			final := q
			if len(wantN) == 1 {
				final = c.bound(wantN[0].At, c.zone)
			}
			if msg := verdict("NextAfter", err, final, len(wantN) == 1); msg != "" {
				t.Error(msg)
			}
		case ok != (len(wantN) == 1) || (ok && !o.At.Equal(wantN[0].At)):
			t.Errorf("%s: NextAfter(MaxSteps %d) = %v %v; the oracle has %v", name, steps, o.At, ok, gotTimes(wantN))
		default:
			*answers++
		}
		occ, err := c.Between(q, q.Add(win), nil, lim)
		over := len(wantB) > DefaultLimits.MaxInstances
		switch {
		case err != nil && errors.Is(err, ErrCapHit) && !over && asKind(err) == CapStall:
			if msg := verdict("Between", err, c.bound(q.Add(win), c.zone), true); msg != "" {
				t.Error(msg)
			}
		case over:
			if asKind(err) != CapInstances || !sameTimes(gotTimes(occ), gotTimes(wantB[:DefaultLimits.MaxInstances])) {
				if asKind(err) != CapStall {
					t.Errorf("%s: Between(MaxSteps %d) = %d occurrences, %v; the oracle has %d, want the first %d and CapInstances", name, steps, len(occ), err, len(wantB), DefaultLimits.MaxInstances)
				}
			}
		case err != nil || !sameTimes(gotTimes(occ), gotTimes(wantB)):
			t.Errorf("%s: Between(MaxSteps %d) = %v %v; the oracle has %v", name, steps, gotTimes(occ), err, gotTimes(wantB))
		default:
			*answers++
		}
	}
}

// asKind is the kind of a CapError in err, or "".
func asKind(err error) CapKind {
	var ce *CapError
	if errors.As(err, &ce) {
		return ce.Kind
	}
	return ""
}

// TestMergeYieldsEachInstantOnce holds the merge itself, below the settle the
// public calls apply: an RDATE equal to a rule occurrence, and an RDATE repeated,
// come out of the stream once, and an EXDATE on an RDATE removes it (TASK-4100
// r3 D1). Through NextAfter and Between a repeat would be invisible, so this
// reads the walk.
func TestMergeYieldsEachInstantOnce(t *testing.T) {
	spec := pinned(t, "UTC", "2025-01-01T09:00", "FREQ=DAILY;COUNT=6")
	spec.RDates = []Wall{mustWall(t, "2025-01-03T09:00"), mustWall(t, "2025-01-03T09:00"), mustWall(t, "2025-01-04T12:00"), mustWall(t, "2025-01-05T12:00")}
	spec.ExDates = []Wall{mustWall(t, "2025-01-05T12:00"), mustWall(t, "2025-01-02T09:00")}
	c := compile(t, spec)
	var got []string
	err := c.walk(time.UTC, time.Time{}, patient.fill(), func() (time.Time, bool) { return time.Time{}, false }, func(o Occurrence, _ time.Time) bool {
		got = append(got, o.At.Format("01-02T15:04"))
		return false
	})
	want := []string{"01-01T09:00", "01-03T09:00", "01-04T09:00", "01-04T12:00", "01-05T09:00", "01-06T09:00"}
	if err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("walk = %v %v; want %v", got, err, want)
	}
}

// The grid sample takes every gridStride-th cell of reachabilityGrid. The stride
// is coprime to every axis length (2, 24, 5, 6, 4), so it cannot alias with an
// axis and drop a value: 24 and 150 did, sampling one Start and one BYMINUTE
// (TASK-4100 r3).
func gridStride() int {
	if raceSample {
		return 149
	}
	return 23
}

func gridPick(n int) bool { return n%gridStride() == 0 }

var gridAxisLen = map[string]int{"freq": 2, "interval": 24, "byhour": 5, "byminute": 6, "start": 4}

// neverCov counts, per axis, how often each value was run; nil outside the test.
var neverCov map[string]map[string]int

func cover(axis, v string) {
	if neverCov != nil {
		if neverCov[axis] == nil {
			neverCov[axis] = map[string]int{}
		}
		neverCov[axis][v]++
	}
}

func gridCover(freq string, iv int, hs, ms []int, st [2]int) {
	cover("grid freq", freq)
	cover("grid interval", fmt.Sprint(iv))
	cover("grid byhour", fmt.Sprint(hs))
	cover("grid byminute", fmt.Sprint(ms))
	cover("grid start", fmt.Sprint(st))
}

func covTable(m map[string]map[string]int) string {
	axes := make([]string, 0, len(m))
	for a := range m {
		axes = append(axes, a)
	}
	sort.Strings(axes)
	var b strings.Builder
	for _, a := range axes {
		vals := make([]string, 0, len(m[a]))
		for v, n := range m[a] {
			vals = append(vals, fmt.Sprintf("%s:%d", v, n))
		}
		sort.Strings(vals)
		fmt.Fprintf(&b, "  %-14s %d values  %s\n", a, len(vals), strings.Join(vals, " "))
	}
	return b.String()
}
