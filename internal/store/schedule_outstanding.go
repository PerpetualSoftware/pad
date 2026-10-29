package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// The outstanding occurrence's instant, and how many occurrences passed after
// it (TASK-4103 C2a).

// maxMissedWindow is how far past the outstanding occurrence the missed count
// looks: the engine's window cap.
const maxMissedWindow = 92 * 24 * time.Hour

// outstandingInstant is the one reading of the outstanding occurrence's instant,
// shared by the reminder reads (fillOccurrenceAt) and the schedule view so they
// cannot disagree. A pinned schedule stores it. A floating one stores it only
// once it is fired or overridden (next_at is then the exact instant, see
// models.ItemSchedule.NextAt); while it is armed with no override, next_at is
// the early bound and the instant is next_wall read in view, the zone the fire
// will use.
func outstandingInstant(nextAt, nextWall *string, floating, fired, overridden bool, view *time.Location) (time.Time, bool, error) {
	if nextAt == nil {
		return time.Time{}, false, nil
	}
	if !floating || fired || overridden {
		t, err := time.Parse(time.RFC3339, *nextAt)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("parse next_at: %w", err)
		}
		return t.UTC(), true, nil
	}
	if nextWall == nil {
		return time.Time{}, false, nil
	}
	w, err := schedule.ParseWall(*nextWall)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse next_wall: %w", err)
	}
	return schedule.Resolve(view, w).UTC(), true, nil
}

// OutstandingInstant is the instant of sc's outstanding occurrence. view is the
// zone an armed floating schedule is read in; it is ignored otherwise. ok is
// false when the schedule has none (ended).
func OutstandingInstant(sc *models.ItemSchedule, view *time.Location) (time.Time, bool, error) {
	if sc.EndedAt != nil {
		return time.Time{}, false, nil
	}
	return outstandingInstant(sc.NextAt, sc.NextWall, sc.Floating, sc.ReminderFired, sc.NextOverrideAt != nil, view)
}

// ScheduleMissed counts the occurrences that passed unresolved AFTER the
// outstanding one: instances strictly after it and at or before nowT-1s. The
// outstanding occurrence itself is not counted. It is computed on read and never
// stored. truncated is true when the count is a floor: the window is capped at
// 92 days past the outstanding occurrence (or the anchor floor), and at 500 instances.
//
// The walk never starts before the schedule's anchor: the rule is compiled from
// anchor_wall when there is one (history before it is not preserved) and the
// window's start is clamped to it, so a rule that is only cheap near its
// re-anchor cannot hit CapScan from the original dtstart (TASK-4102 PR1b r1).
// view is the zone a floating schedule is read in.
func ScheduleMissed(sc *models.ItemSchedule, view *time.Location, nowT time.Time) (count int, truncated bool, err error) {
	if sc.EndedAt != nil || sc.ErrorCode != nil {
		return 0, false, nil
	}
	o, ok, err := OutstandingInstant(sc, view)
	if err != nil || !ok {
		return 0, false, err
	}
	cut := nowT.UTC().Add(-time.Second)
	if !cut.After(o) {
		return 0, false, nil
	}
	start := sc.Dtstart
	if sc.AnchorWall != nil {
		start = *sc.AnchorWall
	}
	c, err := compileScheduleSpec(storedSpecInput(sc, start))
	if err != nil {
		return 0, false, err
	}
	loc := view
	if !sc.Floating {
		if loc, err = time.LoadLocation(c.Spec().Zone); err != nil {
			return 0, false, fmt.Errorf("load schedule zone: %w", err)
		}
	}
	from := o.Add(time.Nanosecond)
	if floor := schedule.Resolve(loc, c.Spec().Start); floor.After(from) {
		from = floor
	}
	to := cut.Add(time.Nanosecond)
	if limit := from.Add(maxMissedWindow); to.After(limit) {
		to, truncated = limit, true
	}
	occs, err := c.Between(from, to, view, schedule.DefaultLimits)
	var capErr *schedule.CapError
	if errors.As(err, &capErr) && capErr.Kind == schedule.CapInstances {
		return len(occs), true, nil
	}
	if err != nil {
		return 0, false, err
	}
	return len(occs), truncated, nil
}
