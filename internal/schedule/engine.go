package schedule

import (
	"errors"
	"sort"
	"time"

	"github.com/PerpetualSoftware/pad/internal/schedule/internal/rrule"
)

// wallMargin is how far past the query bound the wall-clock walk continues.
// Resolve is not monotonic inside a DST gap, and the offsets of a zone differ
// by at most 26 hours, so a wall reading 48 hours past a bound resolves past it
// (TASK-4100). The walk is bounded on the wall reading, never on an instant.
const wallMargin = 48 * time.Hour

var (
	rruleFreq = map[string]rrule.Frequency{
		freqYearly: rrule.YEARLY, freqMonthly: rrule.MONTHLY, freqWeekly: rrule.WEEKLY,
		freqDaily: rrule.DAILY, freqHourly: rrule.HOURLY, freqMinutely: rrule.MINUTELY,
	}
	rruleDay = [7]rrule.Weekday{rrule.SU, rrule.MO, rrule.TU, rrule.WE, rrule.TH, rrule.FR, rrule.SA}
)

// readingOf is the wall clock of an instant in loc, labelled UTC.
func readingOf(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
}

func wallOf(t time.Time, allDay bool) Wall {
	w := Wall{Year: t.Year(), Month: t.Month(), Day: t.Day(), AllDay: allDay}
	if !allDay {
		w.Hour, w.Minute, w.Second = t.Hour(), t.Minute(), t.Second()
	}
	return w
}

// zoneFor is the zone the wall readings resolve in.
func (c *Compiled) zoneFor(view *time.Location) (*time.Location, error) {
	if c.zone != nil {
		return c.zone, nil
	}
	if view == nil {
		return nil, ErrViewRequired
	}
	return view, nil
}

// bound is the walk key past which nothing can still land at or before t.
func (c *Compiled) bound(t time.Time, loc *time.Location) time.Time {
	if c.elapsed {
		return t
	}
	return readingOf(t, loc).Add(wallMargin)
}

// until converts UNTIL into the engine's time space: the instant for an
// elapsed rule, a UTC-labelled wall for the wall-clock path. A Z UNTIL is an
// instant bound, so on the wall-clock path the library gets a generous wall
// (the UNTIL reading plus wallMargin) and afterUntil filters each resolved
// instant exactly (TASK-4100 r1).
func (c *Compiled) until(loc *time.Location) time.Time {
	u := c.rule.Until
	if u.Kind == untilKindInst {
		if c.elapsed {
			return u.Instant
		}
		return readingOf(u.Instant, loc).Add(wallMargin)
	}
	w := u.Wall
	if u.Kind == untilKindDate { // inclusive through the end of that day
		w.Hour, w.Minute, w.Second, w.AllDay = 23, 59, 59, false
	}
	if c.elapsed {
		return Resolve(loc, w)
	}
	return w.naive()
}

// untilSpent says the rule's UNTIL lies wholly before q, so the rule can add
// nothing at or after it (TASK-4100 r1). The wall-clock test carries a
// wallMargin of slack for the non-monotonic gap.
func (c *Compiled) untilSpent(loc *time.Location, q time.Time) bool {
	if c.rule == nil || c.rule.Until == nil {
		return false
	}
	if c.elapsed {
		return c.until(loc).Before(q)
	}
	return c.until(loc).Add(wallMargin).Before(readingOf(q, loc))
}

// afterUntil says an occurrence of the rule falls past a Z UNTIL, compared on
// the resolved instant, inclusive. An RDATE is not bound by UNTIL. Elapsed
// rules compare instants in the library already (TASK-4100 r1).
func (c *Compiled) afterUntil(o Occurrence) bool {
	if c.elapsed || c.rule == nil || c.rule.Until == nil || c.rule.Until.Kind != untilKindInst || !o.At.After(c.rule.Until.Instant) {
		return false
	}
	for _, d := range c.spec.RDates {
		if d == o.Wall {
			return false
		}
	}
	return true
}

// newSet builds a fresh library set. The wall-clock path hands the library
// UTC-labelled walls, so time.Date never guesses in a gap, and Resolve maps
// each result. The elapsed path hands it real instants, fast-forwarded to
// base so the distance from Start costs nothing.
func (c *Compiled) newSet(loc *time.Location, base time.Time, withDates bool, steps int64) (*rrule.Set, *rrule.RRule, error) {
	set := &rrule.Set{}
	var rr *rrule.RRule
	conv := func(w Wall) time.Time { return w.naive() }
	if c.elapsed {
		conv = func(w Wall) time.Time { return Resolve(loc, w) }
	}
	if r := c.rule; r != nil {
		dtstart, count, skip := conv(c.spec.Start), r.Count, c.untilSpent(loc, base)
		if c.elapsed && base.After(dtstart) {
			step := time.Duration(max(r.Interval, 1)) * periodUnit[r.Freq]
			k := int64(base.Sub(dtstart) / step)
			if r.Count > 0 {
				skip, count = k >= int64(r.Count), r.Count-int(k)
			}
			dtstart = dtstart.Add(time.Duration(k) * step)
		}
		if !skip {
			var err error
			rr, err = rrule.NewRRule(c.options(loc, dtstart, count))
			if err != nil {
				return nil, nil, malformed(CodeUnsupported, "", "", "the rule could not be expanded")
			}
			rr.SetBudget(steps) // TASK-4100 H1
			set.RRule(rr)
		}
	}
	if withDates {
		for _, w := range c.spec.RDates {
			set.RDate(conv(w))
		}
		for _, w := range c.spec.ExDates {
			set.ExDate(conv(w))
		}
		if c.rule == nil {
			set.RDate(conv(c.spec.Start))
		}
	}
	return set, rr, nil
}

func (c *Compiled) options(loc *time.Location, dtstart time.Time, count int) rrule.ROption {
	r := c.rule
	o := rrule.ROption{
		Freq: rruleFreq[r.Freq], Dtstart: dtstart, Interval: r.Interval, Count: count,
		Bysetpos: r.BySetPos, Bymonth: r.ByMonth, Bymonthday: r.ByMonthDay, Byyearday: r.ByYearDay,
		Byweekno: r.ByWeekNo, Byhour: r.ByHour, Byminute: r.ByMinute, Bysecond: r.BySecond,
	}
	if r.Wkst != "" {
		o.Wkst = rruleDay[weekdays[r.Wkst]]
	}
	for _, d := range r.ByDay {
		wd := rruleDay[d.Day]
		o.Byweekday = append(o.Byweekday, wd.Nth(d.N))
	}
	if r.Until != nil {
		o.Until = c.until(loc)
	}
	return o
}

// probeSynced says whether the rule alone produces Start as its first
// instance. A rule that exhausts its step budget before any instance (it never
// matches, or only far away) counts as unsynced, not as an error.
func (c *Compiled) probeSynced() (bool, error) {
	loc := c.zone
	if loc == nil {
		loc = time.UTC
	}
	set, _, err := c.newSet(loc, time.Time{}, false, DefaultLimits.MaxSteps)
	if err != nil {
		return false, err
	}
	synced := false
	err = guard(func() error {
		t, ok := set.Iterator()()
		synced = ok && t.Equal(c.spec.Start.naive()) &&
			!c.afterUntil(Occurrence{At: Resolve(loc, c.spec.Start), Wall: c.spec.Start})
		return nil
	})
	return err == nil && synced, err
}

// walk feeds visit every occurrence in library order, with the key the walk
// is bounded on: the wall reading (wall-clock path) or the instant (elapsed
// path). visit returns true to stop. The wall-clock order is not the instant
// order inside a DST gap, so callers sort and de-duplicate what they keep.
// A stalled expander (CapStall) stopped early, so what it yielded is
// incomplete, except an occurrence the iterator already held when its prefetch
// of the successor stalled: that one is visited, and walk returns the stall
// after it together with where the expander stood (reached), so a caller whose
// window ends before that can set the stall aside (covers). With RDATEs the
// set drops the stalled rule and may reorder what follows, so there a stall is
// always an error (TASK-4100 r2).
func (c *Compiled) walk(loc *time.Location, base time.Time, lim Limits, visit func(o Occurrence, key time.Time) bool) (time.Time, error) {
	set, rr, err := c.newSet(loc, base, true, lim.MaxSteps)
	if err != nil {
		return time.Time{}, err
	}
	deadline := time.Now().Add(lim.MaxDuration)
	stalled := func() bool { return rr != nil && rr.Exhausted() }
	stall := &CapError{Kind: CapStall, Limit: lim.MaxSteps}
	next := set.Iterator()
	if stalled() {
		return time.Time{}, stall
	}
	for {
		if time.Now().After(deadline) {
			return time.Time{}, &CapError{Kind: CapTime, Limit: int64(lim.MaxDuration)}
		}
		t, ok := next()
		held := ok && stalled() && len(c.spec.RDates) == 0
		if stalled() && !held {
			return time.Time{}, stall
		}
		if !ok {
			return time.Time{}, nil
		}
		var o Occurrence
		if c.elapsed {
			o = Occurrence{At: t.UTC(), Wall: wallOf(t.In(loc), c.spec.Start.AllDay)}
		} else {
			w := wallOf(t, c.spec.Start.AllDay)
			o = Occurrence{At: Resolve(loc, w), Wall: w}
		}
		if o.Wall.Year > maxYear {
			return time.Time{}, nil
		}
		if !c.afterUntil(o) && visit(o, t) {
			return time.Time{}, nil
		}
		if held {
			return rr.Reached(), stall
		}
	}
}

// NextAfter returns the first occurrence strictly after t, or found=false
// when the set is exhausted. A pinned schedule ignores view.
func (c *Compiled) NextAfter(t time.Time, view *time.Location, lim Limits) (Occurrence, bool, error) {
	loc, err := c.zoneFor(view)
	if err != nil {
		return Occurrence{}, false, err
	}
	lim = lim.fill()
	if err := c.checkScan(loc, t, t, lim); err != nil {
		return Occurrence{}, false, err
	}
	var best Occurrence
	found := false
	var reached time.Time
	err = guard(func() (err error) {
		reached, err = c.walk(loc, t, lim, func(o Occurrence, key time.Time) bool {
			if found && key.After(c.bound(best.At, loc)) {
				return true
			}
			if o.At.After(t) && (!found || o.At.Before(best.At)) {
				best, found = o, true
			}
			return false
		})
		return err
	})
	if err != nil && !(found && covers(err, reached, c.bound(best.At, loc))) {
		return Occurrence{}, false, err
	}
	return best, found, nil
}

// Between returns the occurrences with from <= At < to, strictly ascending
// with no two at one instant. A window longer than MaxWindow is CapWindow. More
// than MaxInstances occurrences returns the first MaxInstances and CapInstances.
func (c *Compiled) Between(from, to time.Time, view *time.Location, lim Limits) ([]Occurrence, error) {
	loc, err := c.zoneFor(view)
	if err != nil || !to.After(from) {
		return nil, err
	}
	lim = lim.fill()
	if to.Sub(from) > lim.MaxWindow {
		return nil, &CapError{Kind: CapWindow, Limit: int64(lim.MaxWindow)}
	}
	if err := c.checkScan(loc, from, to, lim); err != nil {
		return nil, err
	}
	var out []Occurrence
	// Once MaxInstances+1 distinct instants are held, the walk goes on until its
	// key is past the bound of the (MaxInstances+1)-th smallest, so a later wall
	// that resolves earlier (a DST gap) still makes the cut (TASK-4100 r1).
	var stop time.Time
	settleAt := lim.MaxInstances
	var reached time.Time
	end := c.bound(to, loc)
	err = guard(func() (err error) {
		reached, err = c.walk(loc, from, lim, func(o Occurrence, key time.Time) bool {
			if key.After(end) || (!stop.IsZero() && key.After(stop)) {
				return true
			}
			if !o.At.Before(from) && o.At.Before(to) {
				out = append(out, o)
			}
			if len(out) > settleAt {
				if out = settle(out); len(out) > lim.MaxInstances {
					out = out[:lim.MaxInstances+1]
					stop = c.bound(out[lim.MaxInstances].At, loc)
				}
				settleAt = max(lim.MaxInstances, 2*len(out))
			}
			return false
		})
		return err
	})
	if err != nil && !covers(err, reached, end) {
		return nil, err
	}
	out = settle(out)
	if len(out) > lim.MaxInstances {
		return out[:lim.MaxInstances], &CapError{Kind: CapInstances, Limit: int64(lim.MaxInstances)}
	}
	return out, nil
}

// covers says err is the stall after a held occurrence and the expander had
// examined everything up to end, so the answer ending there is complete.
func covers(err error, reached, end time.Time) bool {
	var ce *CapError
	return errors.As(err, &ce) && ce.Kind == CapStall && !reached.Before(end)
}

// settle sorts by instant and drops a repeated instant.
func settle(out []Occurrence) []Occurrence {
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	kept := out[:0]
	for _, o := range out {
		if len(kept) == 0 || o.At.After(kept[len(kept)-1].At) {
			kept = append(kept, o)
		}
	}
	return kept
}
