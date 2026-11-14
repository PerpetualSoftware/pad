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
// resolved occurrences do not, a COPY drops its snooze, and its outstanding occurrence is
// the first one strictly after the copy instant. A copy never fires an occurrence that is
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
//     "exhausted" on a move is terminal only when the source's own outstanding
//     occurrence (read in the source recipient's zone) is absent or not after
//     now (floating schedules only: a pinned one reads one zone on both sides);
//     if the destination zone finds nothing after now but the source still
//     has a future occurrence, that is "exhausted_in_destination_zone", which is
//     NOT terminal: the move is refused. A copy reports "exhausted" instead.
//   - anything else (a rule that no longer compiles, a zone, any validation
//     reason): the schedule is live and the destination cannot take it. A copy
//     skips and reports; a MOVE is refused (ScheduleNotCarriedError) and rolls
//     back, so the item stays where it was. "schedule:unreadable" means the
//     source's outstanding occurrence cannot be read; it is move-only, since
//     a copy does not read that occurrence and keeps its usual carry result.
// timing:cap_time is not a property of the schedule but of the load, so it
// refuses a copy as well as a move, as a *schedule.CapError (503 schedule_busy).

// ScheduleCarryReport says what a cross-workspace copy or move did with the
// source schedule and gates. A gate count can exist without a schedule.
type ScheduleCarryReport struct {
	// HasSchedule is true when the source item has a schedule at all.
	HasSchedule bool
	// Carried is true when the destination got a schedule.
	Carried bool
	// HistoryDropped is how many resolved occurrences the source schedule has:
	// copies and moves drop resolved occurrences.
	HistoryDropped int
	// ChangesDropped is how many journal rows of the source schedule do not
	// reach the destination: every row on a copy or when nothing is carried, and
	// on a carried move the rows the restore refuses.
	ChangesDropped int
	// ForecastDropped is 1 when the source schedule has a forecast: a copy and a
	// cross-workspace move leave the forecast, the selected children and their
	// completion evidence behind. ChildrenDropped counts the selected children
	// the actor can read, so it never reveals one they cannot.
	ForecastDropped int
	ChildrenDropped int
	// LinksDropped is how many time links touch the source schedule whose other
	// end the mover can read. None is carried (links stay within one workspace):
	// a copy starts with none and the source keeps hers; a move deletes all the
	// source's, the ones to hidden items too, which are not counted. A count
	// only, so it names no item.
	LinksDropped int
	GatesDropped int
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

// reasonExhaustedInDestZone is the non-terminal "exhausted": the destination
// zone sees no occurrence after now, but the source's outstanding one is still
// ahead in its own zone. A move is refused; a copy reports "exhausted" instead.
const reasonExhaustedInDestZone = "exhausted_in_destination_zone"

// reasonUnreadableOccurrence refuses a move whose source occurrence cannot be read.
const reasonUnreadableOccurrence = "schedule:unreadable"

// scheduleCarryTerminal reports a skip reason that means the schedule has
// nothing left to fire.
func scheduleCarryTerminal(reason string) bool { return reason == "ended" || reason == "exhausted" }

// scheduleCarryPlan is what a copy would do with the source schedule.
type scheduleCarryPlan struct {
	src     *models.ItemSchedule
	changes []models.ScheduleChangeExport
	in      carriedSchedule
	report  ScheduleCarryReport
}

// carryLimits bounds the search for the destination's first occurrence. A
// variable only so a test can make the clock cap bite (a real one depends on
// load).
var carryLimits = schedule.DefaultLimits

// carriedFromLive builds the destination schedule from the source's stored one.
// It is pure. view is the destination recipient's zone, read only for a
// floating schedule. srcView is the SOURCE recipient's zone, read on a move only,
// to place the source's own outstanding occurrence. A non-empty reason means the
// schedule is not carried.
func carriedFromLive(sc *models.ItemSchedule, roles *models.ScheduleRolesExport, move bool, view, srcView *time.Location, nowT time.Time) (carriedSchedule, string) {
	if sc.EndedAt != nil || sc.NextWall == nil {
		return carriedSchedule{}, "ended"
	}
	in := carriedSchedule{
		Roles: roles,
		Kind:  sc.Kind, Dtstart: sc.Dtstart, Dtend: derefStr(sc.Dtend), Tz: derefStr(sc.Tz), Floating: sc.Floating,
		Rrule: derefStr(sc.Rrule), RDates: sc.RDates, ExDates: sc.ExDates, Anchor: sc.Anchor, Audience: sc.Audience,
		LeadMinutes: sc.LeadMinutes, AnchorWall: derefStr(sc.AnchorWall),
	}
	srcOut, srcOK, srcErr := time.Time{}, false, error(nil)
	if move {
		srcOut, srcOK, srcErr = OutstandingInstant(sc, srcView)
		if srcErr != nil {
			return carriedSchedule{}, reasonUnreadableOccurrence
		}
	}
	if move && (sc.ReminderFired || (srcErr == nil && srcOK && !srcOut.After(nowT))) {
		// The one catch-up: the outstanding occurrence that has fired, or is due
		// and has not fired yet (a lagging tick), armed in the destination so the
		// next tick fires it once.
		in.NextWall, in.NextAt = *sc.NextWall, derefStr(sc.NextAt)
		in.NextOverrideAt = liveOverride(sc, nowT)
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
		if move && sc.Floating && srcErr == nil && srcOK && srcOut.After(nowT) {
			// The rule is spent in the destination's zone but the source still
			// holds a future occurrence in its own: ending the source would lose it.
			return in, reasonExhaustedInDestZone
		}
		return in, "exhausted"
	}
	in.NextWall, in.NextAt = occ.Wall.String(), NormalizeInstant(occ.At)
	samePending := in.NextAt == derefStr(sc.NextAt)
	if sc.Floating {
		samePending = in.NextWall == *sc.NextWall
	}
	if move && samePending {
		// Match the source's pending occurrence by wall for floating schedules,
		// instant for pinned schedules: a move carries that occurrence's snooze.
		in.NextOverrideAt = liveOverride(sc, nowT)
	}
	return in, ""
}

// liveOverride is the source's snooze when it is still ahead of nowT, else "".
func liveOverride(sc *models.ItemSchedule, nowT time.Time) string {
	if sc.NextOverrideAt == nil {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, *sc.NextOverrideAt); err != nil || !t.After(nowT) {
		return ""
	}
	return *sc.NextOverrideAt
}

// carryLinkCount is how many links touch scheduleID whose other end's item
// canRead accepts. A nil predicate grants nothing.
func (s *Store) carryLinkCount(q Queryer, scheduleID string, canRead func(string) bool) (int, error) {
	rows, err := q.Query(s.q(`SELECT CASE WHEN l.parent_schedule_id = ? THEN c.item_id ELSE p.item_id END
 FROM schedule_links l JOIN item_schedules p ON p.id = l.parent_schedule_id JOIN item_schedules c ON c.id = l.child_schedule_id
 WHERE l.parent_schedule_id = ? OR l.child_schedule_id = ?`), scheduleID, scheduleID, scheduleID)
	if err != nil {
		return 0, fmt.Errorf("count schedule links: %w", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var other string
		if err := rows.Scan(&other); err != nil {
			return 0, err
		}
		if canRead != nil && canRead(other) {
			n++
		}
	}
	return n, rows.Err()
}

// planScheduleCarry reads the source schedule on q (FOR UPDATE when lock, on
// Postgres) and decides what the copy does with it, validating the destination
// schedule without writing. It returns nil when the item has no schedule.
// who is the destination item's recipient, whose zone reads a floating schedule.
func (s *Store) planScheduleCarry(q Queryer, workspaceID, itemID string, who scheduleRecipient, move, lock bool, nowT time.Time, canRead, linkEndReadable func(string) bool) (*scheduleCarryPlan, error) {
	sc, err := s.getScheduleQ(q, workspaceID, itemID, lock)
	if err != nil || sc == nil {
		return nil, err
	}
	roles, err := s.scheduleRolesQ(q, sc.ID)
	if err != nil {
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
	if p.report.ForecastDropped, p.report.ChildrenDropped, err = s.carryDropCounts(q, workspaceID, sc.ID, canRead); err != nil {
		return nil, err
	}
	// Links stay within one workspace, so none is carried. Only a count is
	// reported, and only of the links whose other end the mover can read, as
	// ChildrenDropped counts only readable children: a link to a hidden item is
	// deleted with a moved schedule all the same but shows in no report.
	if p.report.LinksDropped, err = s.carryLinkCount(q, sc.ID, linkEndReadable); err != nil {
		return nil, err
	}
	var view *time.Location
	if sc.Floating {
		if view, err = s.scheduleZoneOf(q, who.assignee, who.creator); err != nil {
			return nil, err
		}
	}
	var srcView *time.Location
	if sc.Floating && move {
		if srcView, err = s.scheduleRecipientZone(q, itemID); err != nil {
			return nil, err
		}
	}
	in, skip := carriedFromLive(sc, roles, move, view, srcView, nowT)
	if move && skip == "" {
		samePending := in.NextAt == derefStr(sc.NextAt)
		if sc.Floating {
			samePending = in.NextWall == derefStr(sc.NextWall)
		}
		if samePending {
			overrides, err := s.scheduleOverridesQ(q, workspaceID, sc.ID)
			if err != nil {
				return nil, err
			}
			pendingKey := OccurrenceKey(sc, derefStr(sc.NextAt), derefStr(sc.NextWall))
			for _, o := range overrides[sc.ID] {
				key, err := canonicalCarriedKey(o.OccurrenceKey, sc.Floating)
				if err == nil && key == pendingKey {
					o.OccurrenceKey = key
					in.Overrides = append(in.Overrides, o)
				}
			}
		}
		if p.changes, err = s.scheduleChangesQ(q, workspaceID, sc.ID); err != nil {
			return nil, err
		}
	}
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
	if p.report.Carried && move {
		// The commit restores these rows with the same classifier, so the
		// preview and the commit agree. The destination ids are placeholders:
		// the classifier reads only whether a known schedule maps to one.
		_, drops := classifyScheduleChanges(p.changes, map[string]string{itemID: itemID}, map[string]string{sc.ID: sc.ID}, map[string]models.ScheduleExport{sc.ID: {ItemID: itemID, Floating: sc.Floating}})
		p.report.ChangesDropped = len(drops)
	} else if err := q.QueryRow(s.q(`SELECT COUNT(*) FROM schedule_changes WHERE schedule_id = ?`), sc.ID).Scan(&p.report.ChangesDropped); err != nil {
		return nil, fmt.Errorf("count schedule journal: %w", err)
	}
	return p, nil
}

// PreviewScheduleCarry is what the copy preflight reports: the same plan the
// copy runs, read without a lock and written nowhere.
//
// assignee is the assignee the copy would carry ("" when it is dropped or the
// source has none) and creator the copier: the destination item's recipient.
func (s *Store) PreviewScheduleCarry(q Queryer, workspaceID, itemID, assignee, creator string, move bool, nowT time.Time) (ScheduleCarryReport, error) {
	return s.PreviewScheduleCarryFor(q, workspaceID, itemID, assignee, creator, move, nowT, nil, nil)
}

// PreviewScheduleCarryFor is PreviewScheduleCarry for a reader who may see only
// some selected children (canRead nil: all) and link ends or gate blockers
// (linkEndReadable nil: none).
func (s *Store) PreviewScheduleCarryFor(q Queryer, workspaceID, itemID, assignee, creator string, move bool, nowT time.Time, canRead, linkEndReadable func(string) bool) (ScheduleCarryReport, error) {
	p, err := s.planScheduleCarry(q, workspaceID, itemID, scheduleRecipient{assignee, creator}, move, false, nowT, canRead, linkEndReadable)
	if err != nil {
		return ScheduleCarryReport{}, err
	}
	var report ScheduleCarryReport
	if p != nil {
		report = p.report
	}
	report.GatesDropped, err = s.gatesDropped(q, workspaceID, itemID, linkEndReadable)
	return report, err
}

// PreviewItemScheduleCarry is PreviewScheduleCarry on the store's own connection
// at the current time, for the preflight handler.
func (s *Store) PreviewItemScheduleCarry(workspaceID, itemID, assignee, creator string, move bool, canRead, linkEndReadable func(string) bool) (ScheduleCarryReport, error) {
	return s.PreviewScheduleCarryFor(s.db, workspaceID, itemID, assignee, creator, move, time.Now(), canRead, linkEndReadable)
}

// carrySourceScheduleTx runs inside the copy transaction after the destination
// item exists and, on a move, after the source was archived (so the item row is
// already held and the FOR UPDATE read below sees any committed edit).
func (s *Store) carrySourceScheduleTx(tx *sql.Tx, sourceWorkspaceID, sourceItemID, targetWorkspaceID, targetItemID, assignee, creator string, move bool, nowT time.Time, canRead, linkEndReadable func(string) bool) (ScheduleCarryReport, error) {
	p, err := s.planScheduleCarry(tx, sourceWorkspaceID, sourceItemID, scheduleRecipient{assignee, creator}, move, move, nowT, canRead, linkEndReadable)
	if err != nil || p == nil {
		return ScheduleCarryReport{}, err
	}
	if p.report.Carried {
		prepared, err := prepareCarriedSchedule(p.in)
		if err != nil {
			return ScheduleCarryReport{}, err
		}
		marker := "carry"
		if move {
			marker = ""
		}
		id, err := s.writePreparedSchedule(tx, targetWorkspaceID, targetItemID, p.in, prepared, marker)
		if err != nil {
			return ScheduleCarryReport{}, fmt.Errorf("copy item across workspaces: carry schedule: %w", err)
		}
		if move {
			_, err = s.restoreScheduleChangesTx(tx, targetWorkspaceID, p.changes, map[string]string{sourceItemID: targetItemID}, map[string]string{p.src.ID: id}, map[string]models.ScheduleExport{p.src.ID: {ItemID: sourceItemID, Floating: p.src.Floating}})
			if err != nil {
				return ScheduleCarryReport{}, err
			}
			if err := s.journalScheduleAfterTx(tx, id, "carry", "", "", "", "system"); err != nil {
				return ScheduleCarryReport{}, err
			}
		}
	}
	if move {
		// The item leaves the workspace and the destination schedule has no
		// links, so the source's go with it, whether or not the schedule was
		// carried or had already ended. The schedule row is locked by the plan
		// above, as every link writer locks it before it touches a link.
		if _, err := tx.Exec(s.q(`DELETE FROM schedule_links WHERE parent_schedule_id = ? OR child_schedule_id = ?`), p.src.ID, p.src.ID); err != nil {
			return ScheduleCarryReport{}, fmt.Errorf("copy item across workspaces: delete source links: %w", err)
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
// The ending is dismissed at once: the schedule moved with its item, so a
// restored source must not show it in Outstanding as ended.
func (s *Store) endScheduleTx(tx *sql.Tx, scheduleID string) error {
	before, err := s.scheduleSnapshotTx(tx, scheduleID)
	if err != nil {
		return err
	}
	ts := now()
	if _, err := tx.Exec(s.q(`
		UPDATE item_schedules SET ended_at = ?, ended_dismissed_at = ?, next_at = NULL, next_wall = NULL, next_override_at = NULL,
			anchor_wall = NULL, error_code = NULL, updated_at = ?
		WHERE id = ?`), ts, ts, ts, scheduleID); err != nil {
		return fmt.Errorf("copy item across workspaces: end source schedule: %w", err)
	}
	if err := s.deleteManagedReminderTx(tx, scheduleID); err != nil {
		return err
	}
	return s.journalScheduleAfterTx(tx, scheduleID, "end", "", "", before, "system")
}
