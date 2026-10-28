package schedule

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// patient lifts only the wall-clock check, which is the one limit a loaded or
// -race run can trip; MaxSteps and MaxScan stay at their defaults, so a step or
// scan cap still shows (TASK-4100 r2).
var patient = Limits{MaxDuration: 10 * time.Minute}

// nextGrid is the first n instants after q on a grid of d seconds anchored at
// the epoch: the independent answer for a rule that fires on every grid point.
func nextGrid(q time.Time, d, n int64) []time.Time {
	var out []time.Time
	for k := q.Unix()/d + 1; int64(len(out)) < n; k++ {
		out = append(out, time.Unix(k*d, 0).UTC())
	}
	return out
}

func gotTimes(occ []Occurrence) []time.Time {
	out := make([]time.Time, len(occ))
	for i, o := range occ {
		out[i] = o.At
	}
	return out
}

func sameTimes(a, b []time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

// TestStallDenseRules is the R2-H1 regression (TASK-4100 r2): rules that fire
// every few minutes were refused with a cap once the query passed the day the
// per-call step count ran out (+129d, +133d, +68d and +4165d from Start). The
// stall budget restarts at every candidate, so each is answered far past that.
func TestStallDenseRules(t *testing.T) {
	rows := []struct {
		rule string
		d    int64 // seconds between occurrences
		days int   // query offset from Start, past the rule's former first-cap day
	}{
		{"FREQ=MINUTELY;BYMINUTE=0,15,30,45", 900, 130},
		{"FREQ=MINUTELY;BYMINUTE=0,15,30,45", 900, 3 * 365},
		{"FREQ=MINUTELY;BYMINUTE=0,30", 1800, 134},
		{"FREQ=MINUTELY;BYSECOND=0", 60, 69},
		{"FREQ=HOURLY;BYMINUTE=0,30", 1800, 4170},
	}
	start := inst(t, "2025-01-01T00:00:00Z")
	for _, r := range rows {
		t.Run(fmt.Sprintf("%s+%dd", r.rule, r.days), func(t *testing.T) {
			c := compile(t, pinned(t, "UTC", "2025-01-01T00:00", r.rule))
			q := start.AddDate(0, 0, r.days).Add(7*time.Minute + 13*time.Second)
			want := nextGrid(q, r.d, 5)
			o, ok, err := c.NextAfter(q, nil, patient)
			if err != nil || !ok || !o.At.Equal(want[0]) {
				t.Fatalf("NextAfter = %v %v %v; want %v", o.At, ok, err, want[0])
			}
			occ, err := c.Between(q, want[4].Add(time.Second), nil, patient)
			if err != nil || !sameTimes(gotTimes(occ), want) {
				t.Fatalf("Between = %v %v; want %v", gotTimes(occ), err, want)
			}
		})
	}
}

// TestStallFilteredCandidatesBounded: the reset makes the expander look busy
// whenever it produces a candidate, so a candidate our wrapper then discards
// must not run unbounded (TASK-4100 r2).
func TestStallFilteredCandidatesBounded(t *testing.T) {
	// Produced and discarded by the walk: MINUTELY;BYSECOND=0 yields a candidate
	// a minute, to year 9999, and visit never stops. Only the CapTime check
	// between yields ends it, and it does, after visiting some.
	c := compile(t, pinned(t, "UTC", "2025-01-01T00:00", "FREQ=MINUTELY;BYSECOND=0"))
	seen := 0
	_, err := c.walk(time.UTC, inst(t, "2025-01-01T00:00:00Z"), Limits{MaxDuration: 20 * time.Millisecond}.fill(), func(Occurrence, time.Time) bool {
		seen++
		return false
	})
	asCap(t, err, CapTime)
	if seen == 0 {
		t.Error("the walk visited nothing before CapTime")
	}

	// Past a Z UNTIL: the rule is spent, nothing is produced.
	u := compile(t, pinned(t, "UTC", "2025-01-01T00:00", "FREQ=MINUTELY;BYSECOND=0;UNTIL=20250102T000000Z"))
	if o, ok, err := u.NextAfter(inst(t, "2030-01-01T00:00:00Z"), nil, patient); err != nil || ok {
		t.Errorf("past UNTIL: NextAfter = %v %v %v; want none", o.At, ok, err)
	}
	// Inside the UNTIL margin: produced, then dropped by the instant filter.
	if o, ok, err := u.NextAfter(inst(t, "2025-01-02T00:00:00Z"), nil, patient); err != nil || ok {
		t.Errorf("at UNTIL: NextAfter = %v %v %v; want none", o.At, ok, err)
	}

	// 500 consecutive EXDATEs on a dense rule: skipped inside the set.
	spec := pinned(t, "UTC", "2025-01-01T00:00", "FREQ=MINUTELY;BYSECOND=0")
	for i := 0; i < maxDates; i++ {
		spec.ExDates = append(spec.ExDates, mustWall(t, time.Date(2025, 1, 1, 0, i, 0, 0, time.UTC).Format("2006-01-02T15:04:05")))
	}
	e := compile(t, spec)
	q := inst(t, "2025-01-01T00:00:00Z")
	want := nextGrid(inst(t, "2025-01-01T08:19:30Z"), 60, 3) // 08:20 on, 500 minutes after Start
	if o, ok, err := e.NextAfter(q, nil, patient); err != nil || !ok || !o.At.Equal(want[0]) {
		t.Errorf("EXDATE run: NextAfter = %v %v %v; want %v", o.At, ok, err, want[0])
	}
	occ, err := e.Between(q, want[2].Add(time.Second), nil, patient)
	if err != nil || !sameTimes(gotTimes(occ), want) {
		t.Errorf("EXDATE run: Between = %v %v; want %v", gotTimes(occ), err, want)
	}
}

// TestStallPrefetchKeepsAnswer (R2-L1): the set prefetches each successor, so
// the stall after the last reachable occurrence (the next Feb 29 is four years
// on) must not turn that occurrence into an error (TASK-4100 r2).
func TestStallPrefetchKeepsAnswer(t *testing.T) {
	c := compile(t, pinned(t, "UTC", "2103-03-01T00:00", "FREQ=MINUTELY;BYMONTH=2;BYMONTHDAY=29;BYHOUR=0;BYMINUTE=0"))
	lim := Limits{MaxSteps: 2000} // covers 2103-03 to 2104-02-29, not the four years after
	want := inst(t, "2104-02-29T00:00:00Z")
	o, ok, err := c.NextAfter(inst(t, "2104-02-01T00:00:00Z"), nil, lim)
	if err != nil || !ok || !o.At.Equal(want) {
		t.Fatalf("NextAfter = %v %v %v; want %v", o.At, ok, err, want)
	}
	// A window that ends inside the examined stretch holds the occurrence too.
	occ, err := c.Between(inst(t, "2104-02-01T00:00:00Z"), inst(t, "2104-03-05T00:00:00Z"), nil, lim)
	if err != nil || !sameTimes(gotTimes(occ), []time.Time{want}) {
		t.Fatalf("Between = %v %v; want [%v]", gotTimes(occ), err, want)
	}
	// A window reaching past what was examined may be missing an occurrence: a CapStall.
	_, err = c.Between(inst(t, "2104-02-01T00:00:00Z"), inst(t, "2107-06-01T00:00:00Z"), nil, Limits{MaxSteps: 2000, MaxWindow: 5 * 366 * 24 * time.Hour, MaxScan: 10_000_000})
	asCap(t, err, CapStall)
	// Past the last reachable occurrence there is nothing to keep.
	_, _, err = c.NextAfter(want, nil, lim)
	asCap(t, err, CapStall)
}

// TestStallKinds (R2-L2): CapStall and CapScan are different caps with
// different limits (TASK-4100 r2).
func TestStallKinds(t *testing.T) {
	c := compile(t, pinned(t, "UTC", "2025-01-01T00:00", "FREQ=MINUTELY;BYMONTH=1;BYYEARDAY=200"))
	_, _, err := c.NextAfter(inst(t, "2025-01-01T00:00:00Z"), nil, Limits{})
	if ce := asCap(t, err, CapStall); ce.Limit != DefaultLimits.MaxSteps {
		t.Errorf("CapStall.Limit = %d; want MaxSteps %d", ce.Limit, DefaultLimits.MaxSteps)
	}
	d := compile(t, pinned(t, "UTC", "2025-01-01T00:00", "FREQ=MINUTELY;BYSECOND=0"))
	_, _, err = d.NextAfter(inst(t, "2045-01-01T00:00:00Z"), nil, Limits{})
	if ce := asCap(t, err, CapScan); ce.Limit != DefaultLimits.MaxScan {
		t.Errorf("CapScan.Limit = %d; want MaxScan %d", ce.Limit, DefaultLimits.MaxScan)
	}
}

// refWindow is the vendored engine's own answer with an effectively unlimited
// budget (0 is unbounded): every occurrence in [q, q+w), settled, plus the
// first one strictly after q when w is 0 (NextAfter's contract) (TASK-4100 r2).
func refWindow(t *testing.T, c *Compiled, q time.Time, w time.Duration) []Occurrence {
	t.Helper()
	loc := c.zone
	set, _, err := c.newSet(loc, q, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []Occurrence
	next := set.Iterator()
	for {
		tm, ok := next()
		if !ok {
			break
		}
		var o Occurrence
		if c.elapsed {
			o = Occurrence{At: tm.UTC(), Wall: wallOf(tm.In(loc), c.spec.Start.AllDay)}
		} else {
			wl := wallOf(tm, c.spec.Start.AllDay)
			o = Occurrence{At: Resolve(loc, wl), Wall: wl}
		}
		if o.Wall.Year > maxYear || c.afterUntil(o) {
			continue
		}
		if !o.At.Before(q) && (w != 0 || o.At.After(q)) {
			out = append(out, o)
		}
		end := c.bound(q.Add(w), loc)
		if w == 0 && len(out) > 0 {
			end = c.bound(settle(append([]Occurrence(nil), out...))[0].At, loc)
		}
		if tm.After(end) {
			break
		}
	}
	out = settle(out)
	if w == 0 {
		if len(out) > 1 {
			out = out[:1]
		}
		return out
	}
	kept := out[:0]
	for _, o := range out {
		if o.At.Before(q.Add(w)) {
			kept = append(kept, o)
		}
	}
	return kept
}

// boundsCheck holds one compiled schedule to the bound: at default Limits (but
// for the wall-clock check) NextAfter and Between from q return what the
// unlimited engine returns. A CapScan is allowed only where MaxScan really
// applies (not elapsed, no COUNT, more than MaxScan periods), and is counted.
func boundsCheck(t *testing.T, name string, c *Compiled, q time.Time) (scanned bool) {
	t.Helper()
	const w = 6 * time.Hour
	capOK := func(e error, at time.Time) {
		var ce *CapError
		if !errors.As(e, &ce) {
			t.Errorf("%s: %v", name, e)
		} else if ce.Kind != CapScan || c.rule == nil || c.elapsed || c.rule.Count > 0 || int64(at.Sub(Resolve(c.zone, c.spec.Start))/periodUnit[c.rule.Freq]) < DefaultLimits.MaxScan {
			t.Errorf("%s: %v at %s; only a CapScan past MaxScan periods is allowed", name, e, at.Format(time.RFC3339))
		} else {
			scanned = true
		}
	}
	o, ok, err := c.NextAfter(q, nil, patient)
	if err != nil {
		capOK(err, q)
		return
	}
	ref := refWindow(t, c, q, 0)
	if ok != (len(ref) == 1) || (ok && !o.At.Equal(ref[0].At)) {
		t.Errorf("%s: NextAfter(%s) = %v %v; the unlimited engine has %v", name, q.Format(time.RFC3339), o.At, ok, gotTimes(ref))
	}
	// Between at q, and at the first occurrence after it, so a sparse rule's window is not empty.
	anchors := []time.Time{q}
	if ok {
		anchors = append(anchors, o.At)
	}
	for _, from := range anchors {
		occ, err := c.Between(from, from.Add(w), nil, patient)
		if err != nil {
			capOK(err, from)
		} else if ref := refWindow(t, c, from, w); !sameTimes(gotTimes(occ), gotTimes(ref)) {
			t.Errorf("%s: Between(%s, +6h) = %v; the unlimited engine has %v", name, from.Format(time.RFC3339), gotTimes(occ), gotTimes(ref))
		}
	}
	return
}

// TestBoundsPropertyOverConformanceGrid is the instrument for the stall and
// scan bounds (TASK-4100 r2), after two rounds in which a fix to the budget
// shipped a defect of its own. Over every conformance row and every
// satisfiable cell of the reachability grid, a query a year or more past
// Start returns exactly what the vendored engine produces with no budget; and
// over every unsatisfiable cell the budget ends the walk with a CapStall.
func TestBoundsPropertyOverConformanceGrid(t *testing.T) {
	rows := append(append(append([]Row{}, RFCRows...), RFCErratumRows...), ExtraRows...)
	checked, scans := 0, 0
	for _, r := range rows {
		c, err := Compile(specFromICal(t, r.ICal))
		if err != nil {
			t.Fatalf("%s: %v", r.Name, err)
		}
		start := Resolve(c.zone, c.spec.Start)
		for _, q := range []time.Time{start.AddDate(0, 0, 400), start.AddDate(3, 0, 0)} {
			if boundsCheck(t, r.Name, c, q) {
				scans++
			}
			checked++
		}
	}
	t.Logf("%d conformance rows, %d checks, %d legitimate CapScan", len(rows), checked, scans)

	cells, sat, unsat := 0, 0, 0
	reachabilityGrid(func(freq string, iv int, hs, ms []int, st [2]int) {
		w := Wall{Year: 2025, Month: time.March, Day: 3, Hour: st[0], Minute: st[1]}
		rule := fmt.Sprintf("FREQ=%s;INTERVAL=%d%s%s", freq, iv, list("BYHOUR", hs), list("BYMINUTE", ms))
		name := fmt.Sprintf("%s from %02d:%02d", rule, st[0], st[1])
		q := Resolve(time.UTC, w).AddDate(0, 0, 366)
		cells++
		c, err := Compile(Spec{Start: w, Zone: "UTC", Rule: rule})
		if err == nil {
			sat++
			if boundsCheck(t, name, c, q) {
				scans++
			}
			return
		}
		// Unsatisfiable: an RDATE lets Compile through, and the rule alone must
		// stall at the default budget.
		unsat++
		c, err = Compile(Spec{Start: w, Zone: "UTC", Rule: rule, RDates: []Wall{{Year: 2025, Month: time.March, Day: 3, Hour: 12}}})
		if err != nil {
			t.Fatalf("%s with an RDATE: %v", name, err)
		}
		if _, _, err := c.NextAfter(q, nil, patient); !errors.Is(err, ErrCapHit) {
			t.Errorf("%s: NextAfter err = %v; want a CapStall", name, err)
		} else if ce := asCap(t, err, CapStall); ce.Limit != DefaultLimits.MaxSteps {
			t.Errorf("%s: Limit = %d", name, ce.Limit)
		}
	})
	t.Logf("%d grid cells: %d satisfiable (bound checked), %d unsatisfiable (CapStall at %d steps); %d legitimate CapScan in all", cells, sat, unsat, DefaultLimits.MaxSteps, scans)
	if sat == 0 || unsat == 0 {
		t.Fatalf("grid exercises only one side: %d satisfiable, %d unsatisfiable", sat, unsat)
	}
}
