package schedule

import (
	"fmt"
	"time"
)

const maxDates = 500

// Spec is a schedule: a Start, a zone policy, an optional rule and extra or
// excluded dates. All wall readings are zone-less; the zone is applied by the
// engine.
type Spec struct {
	Start    Wall
	Zone     string // IANA. Required when !Floating; must be "" when Floating
	Floating bool   // false: pinned to Zone; true: resolved in the viewer's zone per call
	Rule     string // RRULE value without "RRULE:"; "" means no rule
	RDates   []Wall // same AllDay-ness as Start; at most 500
	ExDates  []Wall // same; at most 500
}

// Occurrence is one instance of a schedule.
type Occurrence struct {
	At   time.Time // UTC instant; all-day: 00:00 of Wall's date in the effective zone
	Wall Wall      // the wall reading that produced At: the identity key
}

// AllDay reports whether the occurrence is a date.
func (o Occurrence) AllDay() bool { return o.Wall.AllDay }

// Compiled is a validated Spec. It is immutable and safe for concurrent use.
type Compiled struct {
	spec    Spec
	rule    *Rule          // nil: no rule
	zone    *time.Location // nil: floating
	elapsed bool           // bare HOURLY or MINUTELY: steps in elapsed time (ruling Q2)
	synced  bool
}

// checkWall refuses a Wall that ParseWall would not have produced.
func checkWall(w Wall, part string) error {
	if w.Year < minYear || w.Year > maxYear {
		return malformed(CodeWall, part, w.String(), fmt.Sprintf("year out of range %d..%d", minYear, maxYear))
	}
	n := w.naive()
	if n.Year() != w.Year || n.Month() != w.Month || n.Day() != w.Day ||
		(!w.AllDay && (n.Hour() != w.Hour || n.Minute() != w.Minute || n.Second() != w.Second)) ||
		(w.AllDay && w.Hour|w.Minute|w.Second != 0) {
		return malformed(CodeWall, part, w.String(), "not a real calendar date and time")
	}
	return nil
}

// Compile validates the whole Spec. Every refusal is a *MalformedError, and no
// text from the expansion library can reach the caller.
func Compile(s Spec) (*Compiled, error) {
	c := &Compiled{spec: s}
	if s.Floating {
		if s.Zone != "" {
			return nil, malformed(CodeZone, "Zone", s.Zone, "a floating schedule has no zone")
		}
	} else {
		loc, err := time.LoadLocation(s.Zone)
		if s.Zone == "" || s.Zone == "Local" || err != nil {
			return nil, malformed(CodeZone, "Zone", s.Zone, "want an IANA zone name such as America/New_York")
		}
		c.zone = loc
	}
	if err := checkWall(s.Start, "Start"); err != nil {
		return nil, err
	}
	for _, l := range []struct {
		part string
		ws   []Wall
	}{{"RDATE", s.RDates}, {"EXDATE", s.ExDates}} {
		if len(l.ws) > maxDates {
			return nil, malformed(CodeRange, l.part, "", "more than 500 dates")
		}
		for _, w := range l.ws {
			if err := checkWall(w, l.part); err != nil {
				return nil, err
			}
			if w.AllDay != s.Start.AllDay {
				return nil, malformed(CodeCombination, l.part, w.String(), "an all-day Start needs all-day dates and a timed Start needs timed ones")
			}
		}
	}
	c.spec.RDates = append([]Wall(nil), s.RDates...)
	c.spec.ExDates = append([]Wall(nil), s.ExDates...)
	c.synced = true
	if s.Rule == "" {
		return c, nil
	}
	r, err := ParseRule(s.Rule)
	if err == nil {
		err = r.Validate(s.Start, c.zone)
	}
	if err != nil {
		return nil, err
	}
	c.rule, c.spec.Rule = &r, r.String()
	c.elapsed = (r.Freq == freqHourly || r.Freq == freqMinutely) && len(r.ByMonth)+len(r.ByWeekNo)+len(r.ByYearDay)+
		len(r.ByMonthDay)+len(r.ByDay)+len(r.ByHour)+len(r.ByMinute)+len(r.BySecond)+len(r.BySetPos) == 0
	if !c.elapsed { // an elapsed rule starts at Start by construction
		if c.synced, err = c.probeSynced(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Spec returns the schedule as compiled, with the rule canonicalised.
func (c *Compiled) Spec() Spec {
	s := c.spec
	s.RDates = append([]Wall(nil), s.RDates...)
	s.ExDates = append([]Wall(nil), s.ExDates...)
	return s
}

// Bounded reports whether the occurrence set is finite: no rule, or COUNT or UNTIL.
func (c *Compiled) Bounded() bool {
	return c.rule == nil || c.rule.Count > 0 || c.rule.Until != nil
}

// StartSynced reports whether the rule itself produces Start. When it does
// not, Start is not an occurrence (dateutil semantics, ruling Q1): a WEEKLY
// BYDAY=FR rule started on a Tuesday first fires the following Friday.
func (c *Compiled) StartSynced() bool { return c.synced }
