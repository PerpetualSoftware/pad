package schedule

import (
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

// conv maps a wall reading into the library's time space: a UTC-labelled wall
// on the wall-clock path (so time.Date never guesses in a gap and Resolve maps
// each result), a real instant on the elapsed path.
func (c *Compiled) conv(loc *time.Location) func(Wall) time.Time {
	if c.elapsed {
		return func(w Wall) time.Time { return Resolve(loc, w) }
	}
	return func(w Wall) time.Time { return w.naive() }
}

// newRule builds the library's rule generator alone, budgeted at steps (TASK-4100
// H1). The elapsed path fast-forwards it to base so the distance from Start costs
// nothing. It is nil when there is no rule or the rule is spent before base.
func (c *Compiled) newRule(loc *time.Location, base time.Time, steps int64) (*rrule.RRule, error) {
	r := c.rule
	if r == nil || c.untilSpent(loc, base) {
		return nil, nil
	}
	dtstart, count := c.conv(loc)(c.spec.Start), r.Count
	if c.elapsed && base.After(dtstart) {
		step := time.Duration(max(r.Interval, 1)) * periodUnit[r.Freq]
		k := int64(base.Sub(dtstart) / step)
		if r.Count > 0 {
			if k >= int64(r.Count) {
				return nil, nil
			}
			count = r.Count - int(k)
		}
		dtstart = dtstart.Add(time.Duration(k) * step)
	}
	rr, err := rrule.NewRRule(c.options(loc, dtstart, count))
	if err != nil {
		return nil, malformed(CodeUnsupported, "", "", "the rule could not be expanded")
	}
	rr.SetBudget(steps)
	return rr, nil
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
	rr, err := c.newRule(loc, time.Time{}, DefaultLimits.MaxSteps)
	if err != nil || rr == nil {
		return false, err
	}
	synced := false
	err = guard(func() error {
		t, ok := rr.Iterator()()
		synced = ok && t.Equal(c.spec.Start.naive()) &&
			!c.afterUntil(Occurrence{At: Resolve(loc, c.spec.Start), Wall: c.spec.Start})
		return nil
	})
	return err == nil && synced, err
}

// merger merges the three streams of a schedule in library order: the rule's
// generator, budgeted, alone; the RDATEs, a sorted slice; and the EXDATEs, a set.
// The library's rrule.Set is not used to expand, so the schedule's correctness
// does not rest on what the set does inside (TASK-4100 r3).
type merger struct {
	rule     rrule.Next
	rr       *rrule.RRule // nil: no rule stream
	head     time.Time    // the rule's next candidate, when has
	has      bool
	ruleDone bool // the rule stream returned nothing more: it ended, or it stalled
	rdates   []time.Time
	ex       map[int64]bool
	last     time.Time
	lim      Limits
	deadline time.Time
}

func (c *Compiled) newMerger(loc *time.Location, base time.Time, lim Limits) (*merger, error) {
	rr, err := c.newRule(loc, base, lim.MaxSteps)
	if err != nil {
		return nil, err
	}
	conv := c.conv(loc)
	m := &merger{rr: rr, ex: map[int64]bool{}, lim: lim, deadline: time.Now().Add(lim.MaxDuration)}
	if rr != nil {
		m.rule = rr.Iterator()
	} else {
		m.ruleDone = true
	}
	for _, w := range c.spec.RDates {
		m.rdates = append(m.rdates, conv(w))
	}
	if c.rule == nil {
		m.rdates = append(m.rdates, conv(c.spec.Start))
	}
	sort.Slice(m.rdates, func(i, j int) bool { return m.rdates[i].Before(m.rdates[j]) })
	for _, w := range c.spec.ExDates {
		m.ex[conv(w).Unix()] = true // exact instant, in the space the candidates are in
	}
	return m, nil
}

// next is the next occurrence in library order, rule and RDATE merged, a repeat
// dropped, an EXDATE skipped. final is the key past which the caller stops
// reading, when bounded. A stalled rule is a state, not a failure: the RDATEs
// below its stall position still flow, and the stream is stuck only when the
// next thing in it could lie in the unexamined part. CapTime is checked on every
// pass, so a long run of skipped EXDATEs is bounded too (TASK-4100 r3 D1).
func (m *merger) next(final time.Time, bounded bool) (time.Time, bool, error) {
	for {
		if time.Now().After(m.deadline) {
			return time.Time{}, false, &CapError{Kind: CapTime, Limit: int64(m.lim.MaxDuration)}
		}
		if !m.has && !m.ruleDone {
			if m.head, m.has = m.rule(); !m.has {
				m.ruleDone = true
			}
		}
		var t time.Time
		switch {
		case m.has && (len(m.rdates) == 0 || !m.rdates[0].Before(m.head)):
			t, m.has = m.head, false
			if len(m.rdates) > 0 && m.rdates[0].Equal(t) {
				m.rdates = m.rdates[1:]
			}
		case len(m.rdates) > 0 && (m.has || !m.stalled() || m.rdates[0].Before(m.rr.StallPos())):
			t, m.rdates = m.rdates[0], m.rdates[1:]
		case m.stalled():
			return m.stuck(final, bounded)
		default:
			return time.Time{}, false, nil
		}
		if !m.last.IsZero() && t.Equal(m.last) {
			continue
		}
		m.last = t
		if !m.ex[t.Unix()] {
			return t, true, nil
		}
	}
}

func (m *merger) stalled() bool { return m.rr != nil && m.ruleDone && m.rr.Exhausted() }

// stuck is the only place a CapStall comes from (TASK-4100 r3 D3). The stream
// has nothing below the rule's stall position that it has not yielded. If the
// caller stops reading before that position, what it has is final and the stream
// ends for it; otherwise an occurrence may be hiding in the unexamined part, and
// the answer is a CapStall, never a partial one.
func (m *merger) stuck(final time.Time, bounded bool) (time.Time, bool, error) {
	if bounded && m.rr.StallPos().After(final) {
		return time.Time{}, false, nil
	}
	return time.Time{}, false, &CapError{Kind: CapStall, Limit: m.lim.MaxSteps}
}

// walk feeds visit every occurrence in library order, with the key the walk is
// bounded on: the wall reading (wall-clock path) or the instant (elapsed path).
// visit returns true to stop. final is the key past which the caller reads no
// further, when bounded; it is asked before each read, since the caller's bound
// tightens as it finds answers. The wall-clock order is not the instant order
// inside a DST gap, so callers sort and de-duplicate what they keep.
func (c *Compiled) walk(loc *time.Location, base time.Time, lim Limits, final func() (time.Time, bool), visit func(o Occurrence, key time.Time) bool) error {
	m, err := c.newMerger(loc, base, lim)
	if err != nil {
		return err
	}
	for {
		fin, bounded := final()
		t, ok, err := m.next(fin, bounded)
		if err != nil || !ok {
			return err
		}
		var o Occurrence
		if c.elapsed {
			o = Occurrence{At: t.UTC(), Wall: wallOf(t.In(loc), c.spec.Start.AllDay)}
		} else {
			w := wallOf(t, c.spec.Start.AllDay)
			o = Occurrence{At: Resolve(loc, w), Wall: w}
		}
		if o.Wall.Year > maxYear {
			return nil
		}
		if !c.afterUntil(o) && visit(o, t) {
			return nil
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
	err = guard(func() error {
		final := func() (time.Time, bool) { return c.bound(best.At, loc), found }
		return c.walk(loc, t, lim, final, func(o Occurrence, key time.Time) bool {
			if found && key.After(c.bound(best.At, loc)) {
				return true
			}
			if o.At.After(t) && (!found || o.At.Before(best.At)) {
				best, found = o, true
			}
			return false
		})
	})
	if err != nil {
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
	end := c.bound(to, loc)
	final := func() (time.Time, bool) {
		if !stop.IsZero() && stop.Before(end) {
			return stop, true
		}
		return end, true
	}
	err = guard(func() error {
		return c.walk(loc, from, lim, final, func(o Occurrence, key time.Time) bool {
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
	})
	if err != nil {
		return nil, err
	}
	out = settle(out)
	if len(out) > lim.MaxInstances {
		return out[:lim.MaxInstances], &CapError{Kind: CapInstances, Limit: int64(lim.MaxInstances)}
	}
	return out, nil
}

// settle sorts by instant and drops a repeated instant. It also masks a repeat
// the merge itself might yield, so nothing at the public API shows one; the merge
// is held to yielding each instant once by TestMergeYieldsEachInstantOnce
// (TASK-4100 r3 f3).
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
