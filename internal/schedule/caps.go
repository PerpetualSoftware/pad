package schedule

import (
	"errors"
	"math"
	"strconv"
	"time"
)

// Limits bound one NextAfter or Between call. A zero field means the
// DefaultLimits value; there is no "unlimited" (TASK-4100).
type Limits struct {
	MaxWindow    time.Duration // Between only
	MaxInstances int           // Between only
	MaxScan      int64         // FREQ periods between Start and the query point that a call may walk
	MaxDuration  time.Duration // wall-clock check between occurrences of one call
	// MaxSteps bounds the rule expander's period-loop steps between two
	// candidates it produces (TASK-4100 H1, r2): the count restarts at every
	// candidate, so it ends a no-match stretch and nothing else. A stall is a
	// CapStall only when an answer could lie past the examined part (r3).
	// MaxDuration is checked between yields, so a call can overrun it by one
	// period expansion (limited by maxPeriodCandidates) or one no-match stretch of
	// MaxSteps steps; MaxSteps has no ceiling (TASK-4100 final round H1, 3a H-L1).
	// MaxScan additionally refuses a wall-clock query far from Start, except
	// for COUNT rules and rules whose UNTIL is already spent (TASK-4100 r4).
	MaxSteps int64
}

// DefaultLimits is what a zero Limits field falls back to.
var DefaultLimits = Limits{MaxWindow: 92 * 24 * time.Hour, MaxInstances: 500, MaxScan: 2_000_000, MaxDuration: 250 * time.Millisecond, MaxSteps: 200_000}

func (l Limits) fill() Limits {
	d := DefaultLimits
	if l.MaxWindow > 0 {
		d.MaxWindow = l.MaxWindow
	}
	if l.MaxInstances > 0 {
		d.MaxInstances = l.MaxInstances
	}
	if l.MaxScan > 0 {
		d.MaxScan = l.MaxScan
	}
	if l.MaxDuration > 0 {
		d.MaxDuration = l.MaxDuration
	}
	if l.MaxSteps > 0 {
		d.MaxSteps = l.MaxSteps
	}
	return d
}

// CapKind says which limit a CapError hit.
type CapKind string

const (
	CapWindow    CapKind = "window"    // to-from > MaxWindow; nothing expanded
	CapInstances CapKind = "instances" // more than MaxInstances occurrences in the window
	CapScan      CapKind = "scan"      // total work from Start to the query exceeds MaxScan periods; Limit is MaxScan
	CapStall     CapKind = "stall"     // the rule stalled on MaxSteps before the answer was final; Limit is MaxSteps (TASK-4100 r3)
	CapTime      CapKind = "time"      // MaxDuration exceeded between two occurrences
)

// ErrCapHit matches every *CapError through errors.Is.
var ErrCapHit = errors.New("schedule: cap hit")

// CapError reports a limit that stopped a call.
type CapError struct {
	Kind  CapKind
	Limit int64 // the limit that was hit (ns for window and time)
}

func (e *CapError) Error() string {
	return "schedule: cap hit: " + string(e.Kind) + " limit " + strconv.FormatInt(e.Limit, 10)
}

// Is makes errors.Is(err, ErrCapHit) true for any *CapError.
func (e *CapError) Is(target error) bool { return target == ErrCapHit }

// periodUnit is the length of one FREQ period for the scan estimate.
var periodUnit = map[string]time.Duration{
	freqMinutely: time.Minute, freqHourly: time.Hour, freqDaily: 24 * time.Hour,
	freqWeekly: 7 * 24 * time.Hour, freqMonthly: 730 * time.Hour, freqYearly: 8766 * time.Hour,
}

// checkScan refuses a query point so far from Start that walking the rule
// from Start would take more than MaxScan periods. Only the wall-clock path
// walks from Start; the elapsed path fast-forwards. base is the query start
// and q its far end. A rule whose UNTIL is spent before base, or that ends by
// COUNT, is not estimated: a walk too far from Start ends in CapTime
// (MaxDuration), not CapScan. MaxInstances counts only what Between returns
// inside its window, and NextAfter never applies it; MaxSteps bounds only a no-match stretch because
// the count restarts at every candidate. A spent rule can also finish as
// exhausted without hitting a cap (TASK-4100 r1, r3, r4).
func (c *Compiled) checkScan(loc *time.Location, base, q time.Time, lim Limits) error {
	if c.rule == nil || c.elapsed || c.rule.Count > 0 || c.untilSpent(loc, base) {
		return nil
	}
	step, n := time.Duration(math.MaxInt64), time.Duration(max(c.rule.Interval, 1))
	if per := periodUnit[c.rule.Freq]; per <= step/n { // saturate: period*INTERVAL must not wrap
		step = per * n
	}
	if int64(readingOf(q, loc).Sub(c.spec.Start.naive())/step) > lim.MaxScan {
		return &CapError{Kind: CapScan, Limit: lim.MaxScan}
	}
	return nil
}

// guard runs fn in the calling goroutine, so nothing can be abandoned or leak.
// A panic in the library becomes a MalformedError instead of crashing the caller.
// MaxDuration is checked between yields and MaxSteps ends a no-match stretch inside the
// vendored generator (r2, r4); Limits.MaxSteps states what the two bound together.
func guard(fn func() error) (err error) {
	defer func() {
		if recover() != nil {
			err = malformed(CodeUnsupported, "", "", "the rule could not be expanded")
		}
	}()
	return fn()
}
