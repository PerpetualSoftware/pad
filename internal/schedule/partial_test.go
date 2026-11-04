package schedule

import (
	"errors"
	"testing"
	"time"
)

// TASK-4236: a CapTime or CapStall that stops Between returns the occurrences
// it examined plus Resume, the instant every earlier occurrence was examined up
// to. The contract is checked against the same call run without the budget.

// checkPartial holds a Between that stopped on a cap to the Resume contract.
func checkPartial(t *testing.T, c *Compiled, from, to time.Time, got []Occurrence, err error, kind CapKind) {
	t.Helper()
	var ce *CapError
	if !errors.As(err, &ce) || ce.Kind != kind {
		t.Fatalf("err = %v; want a %s cap", err, kind)
	}
	if ce.Resume.IsZero() {
		if len(got) != 0 {
			t.Fatalf("%d occurrences returned with no Resume: a partial answer must say where it is good to", len(got))
		}
		return
	}
	if !ce.Resume.After(from) || ce.Resume.After(to) {
		t.Fatalf("Resume %v not in (%v, %v]", ce.Resume, from, to)
	}
	for i, o := range got {
		if !o.At.Before(ce.Resume) {
			t.Fatalf("occurrence %d at %v is not before Resume %v", i, o.At, ce.Resume)
		}
		if i > 0 && !o.At.After(got[i-1].At) {
			t.Fatalf("occurrences not strictly ascending at %d: %v then %v", i, got[i-1].At, o.At)
		}
	}
	// Everything before Resume must be there: compare with the unbudgeted answer.
	full := DefaultLimits
	full.MaxInstances, full.MaxDuration, full.MaxSteps, full.MaxScan = 1<<30, time.Minute, 1<<40, 1<<40
	want, werr := c.Between(from, ce.Resume, nil, full)
	if werr != nil {
		t.Fatalf("unbudgeted Between: %v", werr)
	}
	if !sameTimes(gotTimes(got), gotTimes(want)) {
		t.Fatalf("partial answer has %d occurrences before Resume %v; the unbudgeted answer has %d", len(got), ce.Resume, len(want))
	}
}

func TestBetweenCapTimeReturnsWhatItExaminedAndWhereToResume(t *testing.T) {
	c := compile(t, pinned(t, "UTC", "2031-01-01T00:00", "FREQ=MINUTELY"))
	from := inst(t, "2031-01-02T00:00:00Z")
	to := from.AddDate(0, 0, 90)
	lim := Limits{MaxInstances: 1 << 30, MaxDuration: 3 * time.Millisecond, MaxWindow: 92 * 24 * time.Hour}
	got, err := c.Between(from, to, nil, lim)
	checkPartial(t, c, from, to, got, err, CapTime)
	var ce *CapError
	if errors.As(err, &ce) && !ce.Resume.IsZero() && len(got) == 0 {
		t.Errorf("a CapTime with a Resume past from returned no occurrences from a dense rule")
	}
}

func TestBetweenCapStallReturnsTheRDateBelowTheStallAndResume(t *testing.T) {
	// A daily 03:00 rule read from 2031-05-01T00:00 needs 180 minute steps to its
	// first match; a budget of 100 stalls at about 01:40, past the RDATE at 01:00
	// and short of the answer.
	spec := pinned(t, "UTC", "2031-05-01T00:00", "FREQ=MINUTELY;BYHOUR=3;BYMINUTE=0")
	spec.RDates = []Wall{mustWall(t, "2031-05-01T01:00")}
	c := compile(t, spec)
	from, to := inst(t, "2031-05-01T00:00:00Z"), inst(t, "2031-05-03T00:00:00Z")
	got, err := c.Between(from, to, nil, Limits{MaxSteps: 100})
	checkPartial(t, c, from, to, got, err, CapStall)
	var ce *CapError
	if !errors.As(err, &ce) || ce.Resume.IsZero() {
		t.Fatalf("the stall inside the window promised no Resume: %v", err)
	}
	if len(got) != 1 || !got[0].At.Equal(inst(t, "2031-05-01T01:00:00Z")) {
		t.Errorf("got %v; want the RDATE below the stall", gotTimes(got))
	}
}

// A stall at or before the window's start examined nothing in the window, so it
// promises nothing: no Resume, no occurrences.
func TestBetweenCapStallBeforeTheWindowPromisesNothing(t *testing.T) {
	spec := pinned(t, "UTC", "2025-01-01T00:00", "FREQ=MINUTELY;BYMONTH=1;BYYEARDAY=200")
	spec.RDates = []Wall{mustWall(t, "2025-06-01T09:00")}
	c := compile(t, spec)
	from, to := inst(t, "2025-05-01T00:00:00Z"), inst(t, "2025-07-01T00:00:00Z")
	got, err := c.Between(from, to, nil, Limits{MaxSteps: 5})
	checkPartial(t, c, from, to, got, err, CapStall)
	var ce *CapError
	if errors.As(err, &ce) && !ce.Resume.IsZero() {
		t.Errorf("Resume = %v; the stall was before the window", ce.Resume)
	}
}

// The same contract in a zone with DST, where the walk is ordered by wall
// reading and Resume carries the 26-hour slack.
func TestBetweenCapTimeInADSTZoneKeepsTheResumeContract(t *testing.T) {
	c := compile(t, pinned(t, "America/New_York", "2031-01-01T00:00", "FREQ=MINUTELY"))
	from := inst(t, "2031-03-08T00:00:00Z") // spans the 2031-03-09 spring-forward
	to := from.AddDate(0, 0, 10)
	lim := Limits{MaxInstances: 1 << 30, MaxDuration: 3 * time.Millisecond, MaxWindow: 92 * 24 * time.Hour}
	got, err := c.Between(from, to, nil, lim)
	checkPartial(t, c, from, to, got, err, CapTime)
}

// An RDATE window MinGap cannot read is an error, never a silent pass: the
// floor would otherwise depend on how slow the walk to that RDATE was. A rule
// that fires daily at 09:00 (written as MINUTELY, so its gap is a day and the
// sample reads fine) walked from 1970 to an RDATE in 2200 is past the scan
// limit at the RDATE, and that is the cap MinGap reports.
func TestMinGapRefusesAnRDateWindowItCannotRead(t *testing.T) {
	spec := pinned(t, "UTC", "1970-01-01T09:00", "FREQ=MINUTELY;BYHOUR=9;BYMINUTE=0")
	spec.RDates = []Wall{mustWall(t, "2200-01-01T09:05")}
	c := compile(t, spec)
	_, _, err := c.MinGap(15*time.Minute, DefaultLimits)
	var ce *CapError
	if !errors.As(err, &ce) || ce.Kind != CapScan {
		t.Fatalf("err = %v; want the scan cap that stopped the RDATE window", err)
	}
}

// RDATEs inside the sampled range are already in the merged stream the sample
// reads, so they need no window of their own: a schedule with 1000 RDATEs close
// to dtstart is measured by the sample alone and passes even with no window
// budget at all (a 1ns budget trips on the first window it would open).
func TestMinGapNeedsNoWindowForRDatesInsideTheSample(t *testing.T) {
	spec := pinned(t, "UTC", "2031-01-01T00:00", "FREQ=HOURLY")
	for i := 0; i < 1000; i++ {
		spec.RDates = append(spec.RDates, mustWall(t, time.Date(2031, 1, 1, 0, 30, 0, 0, time.UTC).Add(time.Duration(i)*time.Hour).Format("2006-01-02T15:04")))
	}
	c := compile(t, spec)
	gap, n, err := c.MinGap(15*time.Minute, Limits{MaxGapWork: time.Nanosecond})
	if err != nil || n < 2000 || gap != 30*time.Minute {
		t.Fatalf("gap = %v n = %d err = %v; want 30m over the sample with no budget error", gap, n, err)
	}
}

// The gap AFTER the last RDATE is measured too. The only RDATE sits one minute
// before a rule instant, past the 2000th occurrence; the sample must read on to
// that rule instant instead of stopping on the RDATE, or the 1-minute gap is
// never seen and only the 59-minute gap before the RDATE is.
func TestMinGapMeasuresTheGapAfterTheLastRDate(t *testing.T) {
	spec := pinned(t, "UTC", "2031-01-01T00:00", "FREQ=HOURLY")
	spec.RDates = []Wall{mustWall(t, "2031-04-15T15:59")}
	c := compile(t, spec)
	gap, _, err := c.MinGap(15*time.Minute, DefaultLimits)
	if err != nil || gap != time.Minute {
		t.Fatalf("gap = %v err = %v; want the 1 minute between the RDATE and the next rule instant", gap, err)
	}
}

// The walk enforces MaxDuration between yields (merger.next), and MinGap, which
// walks the sample with the caller's limits, refuses a schedule it cannot read
// in time with that cap: no occurrence found, so nothing is known, so it is an
// error rather than a pass.
func TestMinGapRefusesWhenTheWalkExceedsMaxDuration(t *testing.T) {
	c := compile(t, pinned(t, "UTC", "2031-01-01T00:00", "FREQ=HOURLY"))
	_, n, err := c.MinGap(15*time.Minute, Limits{MaxDuration: time.Nanosecond})
	var ce *CapError
	if !errors.As(err, &ce) || ce.Kind != CapTime || n != 0 {
		t.Fatalf("n = %d err = %v; want a time cap with nothing read", n, err)
	}
}
