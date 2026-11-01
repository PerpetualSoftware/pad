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
// occurrence is OUTSTANDING and due, whether it has fired (and is unresolved) or
// not yet (the tick lags): the move ends the source, so the destination carries
// that occurrence once and fires it, instead of it vanishing or being reported
// "exhausted". A copy leaves it to the source, which still holds it.
//
// A MOVE also ENDS the source schedule (ended_at set, next_* cleared, history
// kept, managed row deleted), so restoring the archived source cannot fire a
// second copy of the schedule that now lives in the destination.
//
// THE RULE: a move never ends a live schedule it did not carry. A schedule that
// is not carried is one of two kinds:
//   - TERMINAL ("ended", "exhausted"): nothing is left to fire, so a copy skips
//     it and a move ends the source (if it is not already) and reports it.
//   - anything else (a rule that no longer compiles, a zone, any validation
//     reason): the schedule is live and the destination cannot take it. A copy
//     skips and reports; a MOVE is refused (ScheduleNotCarriedError) and rolls
//     back, so the item stays where it was.
// timing:cap_time is not a property of the schedule but of the load, so it
// refuses a copy as well as a move, as a *schedule.CapError (503 schedule_busy).

// ScheduleCarryReport says what a cross-workspace copy or move did with the
// source item's schedule. The zero value is "the source had none".
type ScheduleCarryReport struct {
	// HasSchedule is true when the source item has a schedule at all.
	HasSchedule bool
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

// ScheduleNotCarriedError says a MOVE was refused because the source's live
// schedule cannot be carried: ending it would lose it (TASK-4104 D3).
type ScheduleNotCarriedError struct{ Reason string }

func (e *ScheduleNotCarriedError) Error() string {
	return "move refused: the item's live schedule cannot be carried (" + e.Reason + ")"
}

// scheduleRecipient names the users whose zone reads a floating schedule on the
// destination item: its assignee, else its creator. A copy and its preview both
// build one from the same two facts, the carried assignee and the copier.
type scheduleRecipient struct{ assignee, creator string }

// scheduleCarryTerminal reports a skip reason that means the schedule has
// nothing left to fire.
func scheduleCarryTerminal(reason string) bool { return reason == "ended" || reason == "exhausted" }

// scheduleCarryPlan is what a copy would do with the source schedule.
type scheduleCarryPlan struct {
	src    *models.ItemSchedule
	in     carriedSchedule
	report ScheduleCarryReport
}

// carryLimits bounds the search for the destination's first occurrence. A
// variable only so a test can make the clock cap bite (a real one depends on
// load).
var carryLimits = schedule.DefaultLimits

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
	if move && (sc.ReminderFired || scheduleOccurrenceDue(sc, nowT)) {
		// The one catch-up: the outstanding occurrence that has fired, or is due
		// and has not fired yet (a lagging tick), armed in the destination so the
		// next tick fires it once.
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
	occ, found, err := c.NextAfter(nowT, view, carryLimits)
	if err != nil {
		return in, carryReason(err)
	}
	if !found {
		return in, "exhausted"
	}
	in.NextWall, in.NextAt = occ.Wall.String(), NormalizeInstant(occ.At)
	return in, ""
}

// scheduleOccurrenceDue reports whether the schedule's outstanding occurrence is
// at or before nowT, fired or not.
func scheduleOccurrenceDue(sc *models.ItemSchedule, nowT time.Time) bool {
	at, err := time.Parse(time.RFC3339, derefStr(sc.NextAt))
	return err == nil && !at.After(nowT)
}

// planScheduleCarry reads the source schedule on q (FOR UPDATE when lock, on
// Postgres) and decides what the copy does with it, validating the destination
// schedule without writing. It returns nil when the item has no schedule.
// who is the destination item's recipient, whose zone reads a floating schedule.
func (s *Store) planScheduleCarry(q Queryer, workspaceID, itemID string, who scheduleRecipient, move, lock bool, nowT time.Time) (*scheduleCarryPlan, error) {
	sc, err := s.getScheduleQ(q, workspaceID, itemID, lock)
	if err != nil || sc == nil {
		return nil, err
	}
	if lock {
		// The reminder's fired_at reads through a subquery that, on Postgres under
		// read committed, can keep the snapshot from before the row lock was won:
		// a timing edit that re-armed the reminder while this waited would still
		// read as fired. A statement of its own sees the committed row.
		var fired sql.NullString
		if err := q.QueryRow(s.q(`SELECT fired_at FROM item_reminders WHERE schedule_id = ?`), sc.ID).Scan(&fired); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("re-read schedule reminder: %w", err)
		}
		sc.ReminderFired = fired.Valid
	}
	p := &scheduleCarryPlan{src: sc}
	p.report.HasSchedule = true
	if err := q.QueryRow(s.q(`SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id = ?`), sc.ID).Scan(&p.report.HistoryDropped); err != nil {
		return nil, fmt.Errorf("count schedule history: %w", err)
	}
	var view *time.Location
	if sc.Floating {
		if view, err = s.scheduleZoneOf(q, who.assignee, who.creator); err != nil {
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
	if skip == "timing:cap_time" {
		return nil, &schedule.CapError{Kind: schedule.CapTime}
	}
	if skip != "" && move && !scheduleCarryTerminal(skip) {
		return nil, &ScheduleNotCarriedError{Reason: skip}
	}
	p.in, p.report.Skipped, p.report.Carried = in, skip, skip == ""
	return p, nil
}

// PreviewScheduleCarry is what the copy preflight reports: the same plan the
// copy runs, read without a lock and written nowhere.
//
// assignee is the assignee the copy would carry ("" when it is dropped or the
// source has none) and creator the copier: the destination item's recipient.
func (s *Store) PreviewScheduleCarry(q Queryer, workspaceID, itemID, assignee, creator string, move bool, nowT time.Time) (ScheduleCarryReport, error) {
	p, err := s.planScheduleCarry(q, workspaceID, itemID, scheduleRecipient{assignee, creator}, move, false, nowT)
	if err != nil || p == nil {
		return ScheduleCarryReport{}, err
	}
	return p.report, nil
}

// PreviewItemScheduleCarry is PreviewScheduleCarry on the store's own connection
// at the current time, for the preflight handler.
func (s *Store) PreviewItemScheduleCarry(workspaceID, itemID, assignee, creator string, move bool) (ScheduleCarryReport, error) {
	return s.PreviewScheduleCarry(s.db, workspaceID, itemID, assignee, creator, move, time.Now())
}

// carrySourceScheduleTx runs inside the copy transaction after the destination
// item exists and, on a move, after the source was archived (so the item row is
// already held and the FOR UPDATE read below sees any committed edit).
func (s *Store) carrySourceScheduleTx(tx *sql.Tx, sourceWorkspaceID, sourceItemID, targetWorkspaceID, targetItemID, assignee, creator string, move bool, nowT time.Time) (ScheduleCarryReport, error) {
	p, err := s.planScheduleCarry(tx, sourceWorkspaceID, sourceItemID, scheduleRecipient{assignee, creator}, move, move, nowT)
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
