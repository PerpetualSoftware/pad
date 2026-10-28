package schedule

import (
	"errors"
	"strconv"
	"sync/atomic"
	"time"
)

// Limits bound one NextAfter or Between call. A zero field means the
// DefaultLimits value; there is no "unlimited" (TASK-4100).
type Limits struct {
	MaxWindow    time.Duration // Between only
	MaxInstances int           // Between only
	MaxScan      int64         // FREQ periods between Start and the query point that a call may walk
	MaxDuration  time.Duration // wall-clock guard on one call
}

// DefaultLimits is what a zero Limits field falls back to.
var DefaultLimits = Limits{MaxWindow: 92 * 24 * time.Hour, MaxInstances: 500, MaxScan: 2_000_000, MaxDuration: 250 * time.Millisecond}

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
	return d
}

// CapKind says which limit a CapError hit.
type CapKind string

const (
	CapWindow    CapKind = "window"    // to-from > MaxWindow; nothing expanded
	CapInstances CapKind = "instances" // more than MaxInstances occurrences in the window
	CapScan      CapKind = "scan"      // query point too far from Start for the rule's step
	CapTime      CapKind = "time"      // MaxDuration exceeded
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
// walks from Start; the elapsed path fast-forwards.
func (c *Compiled) checkScan(loc *time.Location, q time.Time, lim Limits) error {
	if c.rule == nil || c.elapsed {
		return nil
	}
	step := periodUnit[c.rule.Freq] * time.Duration(max(c.rule.Interval, 1))
	if int64(readingOf(q, loc).Sub(c.spec.Start.naive())/step) > lim.MaxScan {
		return &CapError{Kind: CapScan, Limit: lim.MaxScan}
	}
	return nil
}

// guard runs fn and gives up on it after limit. The library cannot be
// interrupted, so a timed-out fn is told to stop through the flag and left to
// finish on its own; the caller must not read what it wrote. A panic in the
// library becomes a MalformedError instead of crashing the caller.
func guard(limit time.Duration, fn func(stop *atomic.Bool) error) error {
	var stop atomic.Bool
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- malformed(CodeUnsupported, "", "", "the rule could not be expanded")
			}
		}()
		done <- fn(&stop)
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		stop.Store(true)
		return &CapError{Kind: CapTime, Limit: int64(limit)}
	}
}
