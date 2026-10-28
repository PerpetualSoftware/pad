package schedule

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/schedule/internal/rrule"
)

// oracleConv maps a wall reading into the library's time space: a UTC-labelled
// wall on the wall-clock path, a real instant on the elapsed path.
func oracleConv(c *Compiled, loc *time.Location) func(Wall) time.Time {
	if c.elapsed {
		return func(w Wall) time.Time { return Resolve(loc, w) }
	}
	return func(w Wall) time.Time { return w.naive() }
}

// oracleRule builds the vendored rule alone, as the engine hands it to the
// library (elapsed rules fast-forwarded to base). steps is the budget; 0 is
// unbounded. It is nil when the rule is spent before base. TASK-4100 r3.
func oracleRule(t *testing.T, c *Compiled, loc *time.Location, base time.Time, steps int64) *rrule.RRule {
	t.Helper()
	r := c.rule
	if r == nil || c.untilSpent(loc, base) {
		return nil
	}
	dtstart, count := oracleConv(c, loc)(c.spec.Start), r.Count
	if c.elapsed && base.After(dtstart) {
		step := time.Duration(max(r.Interval, 1)) * periodUnit[r.Freq]
		k := int64(base.Sub(dtstart) / step)
		if r.Count > 0 {
			if k >= int64(r.Count) {
				return nil
			}
			count = r.Count - int(k)
		}
		dtstart = dtstart.Add(time.Duration(k) * step)
	}
	rr, err := rrule.NewRRule(c.options(loc, dtstart, count))
	if err != nil {
		t.Fatal(err)
	}
	rr.SetBudget(steps)
	return rr
}

// oracleNext is the vendored rrule.Set, unbudgeted: the test oracle the engine's
// own merge is held to. withRule false leaves the rule out, for a rule known
// never to fire (TASK-4100 r3 D4).
func oracleNext(t *testing.T, c *Compiled, loc *time.Location, base time.Time, withRule bool) func() (time.Time, bool) {
	t.Helper()
	conv := oracleConv(c, loc)
	set := &rrule.Set{}
	if withRule {
		if rr := oracleRule(t, c, loc, base, 0); rr != nil {
			set.RRule(rr)
		}
	}
	for _, w := range c.spec.RDates {
		set.RDate(conv(w))
	}
	for _, w := range c.spec.ExDates {
		set.ExDate(conv(w))
	}
	if c.rule == nil {
		set.RDate(conv(c.spec.Start))
	}
	return set.Iterator()
}

// refWindow is the oracle's answer: every occurrence in [q, q+w), settled, or
// for w == 0 the first one strictly after q (NextAfter's contract).
func refWindow(t *testing.T, c *Compiled, q time.Time, w time.Duration, withRule bool) []Occurrence {
	t.Helper()
	loc := c.zone
	var out []Occurrence
	next := oracleNext(t, c, loc, q, withRule)
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

// ruleStallsBy says the rule alone, budgeted at steps from base, stops on its
// budget with nothing left to examine below limit: the one case in which a
// CapStall for an answer final at limit is legitimate. A rule that produces a
// candidate past limit, or ends, or stalls at a position past limit, makes the
// answer final, so a CapStall there is a false cap (TASK-4100 r3 D3).
func ruleStallsBy(t *testing.T, c *Compiled, loc *time.Location, base time.Time, steps int64, limit time.Time) bool {
	t.Helper()
	rr := oracleRule(t, c, loc, base, steps)
	if rr == nil {
		return false
	}
	next := rr.Iterator()
	for {
		tm, ok := next()
		if !ok {
			return rr.Exhausted() && !stallPos(rr).After(limit)
		}
		if tm.After(limit) {
			return false
		}
	}
}
