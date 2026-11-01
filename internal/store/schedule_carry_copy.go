package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// A cross-workspace copy or move carries the source item's schedule (TASK-4104
// D3) through the same insertCarriedScheduleTx the bundle import uses, and the
// preflight asks the same planScheduleCarry the copy runs.
//
// The destination is armed FRESH: its timing, kind, audience and lead carry, its
// history and snooze do not, and its outstanding occurrence is the first one
// strictly after the copy instant. A copy never fires an occurrence that is
// already due or past. The one exception is a MOVE of a source whose current
// occurrence has FIRED and is still unresolved: the move ends the source, so the
// destination carries that occurrence once and fires it, instead of the user's
// open reminder vanishing. A copy leaves it to the source, which still holds it.
//
// A MOVE also ENDS the source schedule (ended_at set, next_* cleared, history
// kept, managed row deleted), so restoring the archived source cannot fire a
// second copy of the schedule that now lives in the destination.

// ScheduleCarryReport says what a cross-workspace copy or move did with the
// source item's schedule. The zero value is "the source had none".
type ScheduleCarryReport struct {
	// Carried is true when the destination got a schedule.
	Carried bool
	// HistoryDropped is how many resolved occurrences the source schedule has:
	// a copy never carries history.
	HistoryDropped int
	// Skipped is the reason a source schedule was not carried: "ended", a
	// timing code (timing:syntax, timing:cap_scan, ...), "exhausted" (the rule
	// has no occurrence after now) or another carry reason. "" when carried.
	Skipped string
}

// scheduleCarryPlan is what a copy would do with the source schedule.
type scheduleCarryPlan struct {
	src    *models.ItemSchedule
	in     carriedSchedule
	report ScheduleCarryReport
}

// carriedFromLive builds the destination schedule from the source's stored one.
// It is pure. view is the destination recipient's zone, read only for a
// floating schedule. A non-empty reason means the schedule is not carried.
func carriedFromLive(sc *models.ItemSchedule, move bool, view *time.Location, nowT time.Time) (carriedSchedule, string) {
	if sc.EndedAt != nil || sc.NextWall == nil {
		return carriedSchedule{}, "ended"
	}
	in := carriedSchedule{
		Kind: sc.Kind, Dtstart: sc.Dtstart, Dtend: derefStr(sc.Dtend), Tz: derefStr(sc.Tz), Floating: sc.Floating,
		Rrule: derefStr(sc.Rrule), RDates: sc.RDates, ExDates: sc.ExDates, Anchor: sc.Anchor, Audience: sc.Audience,
		LeadMinutes: sc.LeadMinutes, AnchorWall: derefStr(sc.AnchorWall),
	}
	if move && sc.ReminderFired {
		// The one catch-up: the fired, unresolved occurrence, armed in the
		// destination so the next tick fires it once.
		in.NextWall, in.NextAt = *sc.NextWall, derefStr(sc.NextAt)
		return in, ""
	}
	start := sc.Dtstart
	if sc.AnchorWall != nil {
		start = *sc.AnchorWall
	}
	c, err := compileScheduleSpec(storedSpecInput(sc, start))
	if err != nil {
		return in, carryReason(err)
	}
	occ, found, err := c.NextAfter(nowT, view, schedule.DefaultLimits)
	if err != nil {
		return in, carryReason(err)
	}
	if !found {
		return in, "exhausted"
	}
	in.NextWall, in.NextAt = occ.Wall.String(), NormalizeInstant(occ.At)
	return in, ""
}

// planScheduleCarry reads the source schedule on q (FOR UPDATE when lock, on
// Postgres) and decides what the copy does with it, validating the destination
// schedule without writing. It returns nil when the item has no schedule.
// zoneItemID is the item whose recipient zone reads a floating schedule: the new
// destination item for a copy, the source for a preview.
func (s *Store) planScheduleCarry(q Queryer, workspaceID, itemID, zoneItemID string, move, lock bool, nowT time.Time) (*scheduleCarryPlan, error) {
	sc, err := s.getScheduleQ(q, workspaceID, itemID, lock)
	if err != nil || sc == nil {
		return nil, err
	}
	p := &scheduleCarryPlan{src: sc}
	if err := q.QueryRow(s.q(`SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id = ?`), sc.ID).Scan(&p.report.HistoryDropped); err != nil {
		return nil, fmt.Errorf("count schedule history: %w", err)
	}
	var view *time.Location
	if sc.Floating {
		if view, err = s.scheduleRecipientZone(q, zoneItemID); err != nil {
			return nil, err
		}
	}
	in, skip := carriedFromLive(sc, move, view, nowT)
	if skip == "" {
		if _, err := prepareCarriedSchedule(in); err != nil {
			var drop *scheduleCarryDrop
			if !errors.As(err, &drop) {
				return nil, err
			}
			skip = drop.Reason
		}
	}
	p.in, p.report.Skipped, p.report.Carried = in, skip, skip == ""
	return p, nil
}

// PreviewScheduleCarry is what the copy preflight reports: the same plan the
// copy runs, read without a lock and written nowhere.
func (s *Store) PreviewScheduleCarry(q Queryer, workspaceID, itemID string, move bool, nowT time.Time) (ScheduleCarryReport, error) {
	p, err := s.planScheduleCarry(q, workspaceID, itemID, itemID, move, false, nowT)
	if err != nil || p == nil {
		return ScheduleCarryReport{}, err
	}
	return p.report, nil
}

// PreviewItemScheduleCarry is PreviewScheduleCarry on the store's own connection
// at the current time, for the preflight handler.
func (s *Store) PreviewItemScheduleCarry(workspaceID, itemID string, move bool) (ScheduleCarryReport, error) {
	return s.PreviewScheduleCarry(s.db, workspaceID, itemID, move, time.Now())
}

// carrySourceScheduleTx runs inside the copy transaction after the destination
// item exists and, on a move, after the source was archived (so the item row is
// already held and the FOR UPDATE read below sees any committed edit).
func (s *Store) carrySourceScheduleTx(tx *sql.Tx, sourceWorkspaceID, sourceItemID, targetWorkspaceID, targetItemID string, move bool, nowT time.Time) (ScheduleCarryReport, error) {
	p, err := s.planScheduleCarry(tx, sourceWorkspaceID, sourceItemID, targetItemID, move, move, nowT)
	if err != nil || p == nil {
		return ScheduleCarryReport{}, err
	}
	if p.report.Carried {
		if _, err := s.insertCarriedScheduleTx(tx, targetWorkspaceID, targetItemID, p.in); err != nil {
			return ScheduleCarryReport{}, fmt.Errorf("copy item across workspaces: carry schedule: %w", err)
		}
	}
	if move && p.src.EndedAt == nil {
		if err := s.endScheduleTx(tx, p.src.ID); err != nil {
			return ScheduleCarryReport{}, err
		}
	}
	return p.report, nil
}

// endScheduleTx ends a schedule without a rule edit: the schedule row first, then
// its managed reminder, the order every schedule writer uses. History is kept.
func (s *Store) endScheduleTx(tx *sql.Tx, scheduleID string) error {
	ts := now()
	if _, err := tx.Exec(s.q(`
		UPDATE item_schedules SET ended_at = ?, next_at = NULL, next_wall = NULL, next_override_at = NULL,
			anchor_wall = NULL, error_code = NULL, updated_at = ?
		WHERE id = ?`), ts, ts, scheduleID); err != nil {
		return fmt.Errorf("copy item across workspaces: end source schedule: %w", err)
	}
	return s.deleteManagedReminderTx(tx, scheduleID)
}
