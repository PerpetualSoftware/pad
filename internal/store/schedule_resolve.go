package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// Resolving a schedule's outstanding occurrence (TASK-4103 C2b, DOC-4098).
//
// done and skip close the occurrence and advance the schedule to the next one;
// snooze and move keep it outstanding and re-arm the same managed row at a new
// instant. Everything is ONE transaction: the optional comment and its activity,
// the occurrence rows, the schedule and the managed reminder commit together or
// not at all.
//
// LOCK ORDER (Postgres): resolve enters through the same pin statement as a
// fire (item, workspace, schedule, in the OF-list order), so a fire and a
// resolve of one schedule conflict at the first lock and the loser waits there
// holding nothing the winner needs. After the pin it takes the comment and
// activity inserts (their FK locks are on rows already held), the occurrence
// rows (FK parent: the held schedule), the schedule UPDATE and the managed row,
// the order every schedule writer uses. Nothing requests an item or workspace
// row after the schedule. Two resolves serialise on the pin and the second
// finds the advanced schedule and is stale; the exactly-once guarantee rests on
// the pin and the stale check inside the transaction, not on a unique index.

// maxResolveUntil bounds a snooze or move: `until` is after now and at most this
// far ahead.
const maxResolveUntil = 366 * 24 * time.Hour

var (
	// ErrScheduleNotFound is the answer when the item has no schedule.
	ErrScheduleNotFound = errors.New("item has no schedule")
)

// ErrStaleOccurrence is the answer when the occurrence_at a caller names is not
// the schedule's outstanding one (already resolved, moved on, the schedule
// ended, or a floating one read in another zone). Schedule is the current
// state, so the caller can retry with the occurrence it carries.
type ErrStaleOccurrence struct {
	Schedule *models.ItemSchedule
}

func (e *ErrStaleOccurrence) Error() string { return "occurrence is not the outstanding one" }

// Ended reports whether the schedule has no outstanding occurrence.
func (e *ErrStaleOccurrence) Ended() bool {
	return e.Schedule.EndedAt != nil || e.Schedule.NextAt == nil
}

// ErrScheduleBroken is the answer for a schedule whose stored rule no longer
// compiles (error_code is set). Nothing is advanced or ended silently; a timing
// edit clears it.
type ErrScheduleBroken struct {
	Code string
}

func (e *ErrScheduleBroken) Error() string { return "schedule rule is broken: " + e.Code }

// ResolveComment is the optional comment of a resolution: written in the same
// transaction, with the activity row it links to.
type ResolveComment struct {
	UserID   string
	Input    models.CommentCreate
	Activity models.Activity
}

// ResolveInput is what a caller resolves: the occurrence it saw and what to do.
type ResolveInput struct {
	OccurrenceAt time.Time
	State        string     // done | skip | snooze | move
	Until        *time.Time // snooze: the re-fire instant. move: the new due instant
	// View is the caller's own zone: the caller must pass ITS zone, not the
	// recipient's (the HTTP handler passes viewerZone(r).Loc). A floating occurrence that has not fired is
	// read in the zone of whoever looks, so the instant the caller saw is
	// accepted in either its zone or the recipient's.
	View    *time.Location
	Actor   string // schedule_occurrences.actor: "user" or "agent"
	Comment *ResolveComment
}

// ResolveResult is the committed resolution.
type ResolveResult struct {
	Schedule       *models.ItemSchedule
	Comment        *models.Comment
	MissedRecorded int
}

// ResolveOccurrence resolves the outstanding occurrence of an item's schedule.
func (s *Store) ResolveOccurrence(workspaceID, itemID string, in ResolveInput) (*ResolveResult, error) {
	return s.resolveOccurrenceAt(workspaceID, itemID, in, time.Now())
}

func (s *Store) resolveOccurrenceAt(workspaceID, itemID string, in ResolveInput, nowT time.Time) (*ResolveResult, error) {
	// Every stamp and walk of a resolve is a whole second: the engine's walls
	// are, and an elapsed grid must read back exactly from its start.
	nowT = nowT.UTC().Truncate(time.Second)
	switch in.State {
	case "done", "skip":
		if in.Until != nil {
			return nil, fieldErr("until", "combination", "until is for snooze and move")
		}
	case "snooze", "move":
		if in.Until == nil {
			return nil, fieldErr("until", "required", "snooze and move need until")
		}
		if !in.Until.After(nowT) || in.Until.After(nowT.Add(maxResolveUntil)) {
			return nil, fieldErr("until", "range", "until must be after now and within 366 days")
		}
	default:
		return nil, fieldErr("state", "enum", "state must be one of done, skip, snooze, move")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("resolve schedule: %w", err)
	}
	defer tx.Rollback()

	if err := s.pinScheduleTx(tx, workspaceID, itemID); err != nil {
		return nil, err
	}
	sc, err := s.getScheduleQ(tx, workspaceID, itemID, false)
	if err != nil {
		return nil, err
	}
	if sc == nil {
		return nil, ErrScheduleNotFound
	}

	if in.State == "move" {
		switch sc.Kind {
		case "deadline_hard":
			return nil, fieldErr("state", "combination", "a hard deadline cannot be moved; snooze it, or change its kind")
		case "deadline_soft":
			if in.Comment == nil || strings.TrimSpace(in.Comment.Input.Body) == "" {
				return nil, fieldErr("comment", "required", "moving a soft deadline needs a comment saying why")
			}
		}
	}

	if sc.EndedAt != nil || sc.NextAt == nil || sc.NextWall == nil {
		return nil, &ErrStaleOccurrence{Schedule: sc}
	}
	var rec *time.Location // the recipient's zone; nil for a pinned schedule
	if sc.Floating {
		if rec, err = s.scheduleRecipientZone(tx, itemID); err != nil {
			return nil, err
		}
	}
	cands := []*time.Location{rec}
	if sc.Floating && !sc.ReminderFired && sc.NextOverrideAt == nil && in.View != nil {
		cands = append(cands, in.View)
	}
	want := in.OccurrenceAt.UTC().Truncate(time.Second)
	var x, xRec time.Time // xRec: the recipient's reading, the first candidate
	matched := false
	for i, v := range cands {
		cur, ok, err := OutstandingInstant(sc, v)
		if err != nil {
			return nil, err
		}
		if i == 0 && ok {
			xRec = cur.Truncate(time.Second)
		}
		if ok && cur.Truncate(time.Second).Equal(want) {
			x, matched = cur.Truncate(time.Second), true
			break
		}
	}
	if !matched {
		return nil, &ErrStaleOccurrence{Schedule: sc}
	}
	if sc.ErrorCode != nil {
		return nil, &ErrScheduleBroken{Code: *sc.ErrorCode}
	}

	ts := nowT.Format(time.RFC3339)
	res := &ResolveResult{}
	commentID := ""
	if in.Comment != nil {
		in.Comment.Input.ActivityID, err = s.createActivityQ(tx, in.Comment.Activity)
		if err != nil {
			return nil, fmt.Errorf("insert activity for resolution: %w", err)
		}
		if commentID, err = s.createCommentTx(tx, workspaceID, itemID, in.Comment.UserID, in.Comment.Input); err != nil {
			return nil, err
		}
		if res.Comment, err = s.getCommentQ(tx, commentID); err != nil {
			return nil, err
		}
	}
	xAt, xWall := x.Format(time.RFC3339), *sc.NextWall

	if in.State == "snooze" || in.State == "move" {
		until := time.Time{}
		if p, err := time.Parse(time.RFC3339, NormalizeInstant(*in.Until)); err == nil {
			until = p
		}
		override := until // move: the new DUE instant, the lead is subtracted at fire
		if in.State == "snooze" {
			// A snooze names the RE-FIRE instant: the lead is added back so
			// fireInstant, unchanged, lands exactly on until.
			override = until.Add(time.Duration(sc.LeadMinutes) * time.Minute)
		}
		fireLoc := boundZone
		if !sc.Floating {
			if fireLoc, err = time.LoadLocation(*sc.Tz); err != nil {
				return nil, fmt.Errorf("load schedule zone: %w", err)
			}
		}
		wall, err := schedule.ParseWall(xWall)
		if err != nil {
			return nil, fmt.Errorf("parse next_wall: %w", err)
		}
		// The occurrence is frozen at the instant the caller saw (next_at), so a
		// later zone change cannot move the re-fire off it (C2b-1).
		fire := fireInstant(x, wall, fireLoc, sc.LeadMinutes, &override)
		if _, err := tx.Exec(s.q(`UPDATE item_schedules SET next_at = ?, next_override_at = ?, updated_at = ? WHERE id = ?`),
			xAt, override.UTC().Format(time.RFC3339), ts, sc.ID); err != nil {
			return nil, fmt.Errorf("snooze schedule: %w", err)
		}
		if ok, err := s.rearmManagedReminderTx(tx, sc.ID, NormalizeInstant(fire)); err != nil {
			return nil, err
		} else if !ok {
			return nil, errors.New("schedule has no managed reminder")
		}
		if in.State == "move" {
			if err := s.upsertOccurrenceTx(tx, sc.ID, xAt, xWall, "moved", commentID, in.Actor, ts); err != nil {
				return nil, err
			}
		}
		return s.commitResolve(tx, workspaceID, itemID, res)
	}

	// done / skip: the passed-over instances, then the next occurrence.
	missed, _, err := missedOccurrences(sc, rec, nowT)
	if err != nil {
		return nil, err
	}
	for _, m := range missed {
		n, err := s.insertMissedTx(tx, sc.ID, m, ts)
		if err != nil {
			return nil, err
		}
		res.MissedRecorded += n
	}
	state := "done"
	if in.State == "skip" {
		state = "skipped"
	}
	// The history row is the recipient's reading; the caller's zone only gated
	// acceptance (D2), and snooze/move above keep the matched instant.
	if !xRec.IsZero() {
		x, xAt = xRec, xRec.Format(time.RFC3339)
	}
	if err := s.upsertOccurrenceTx(tx, sc.ID, xAt, xWall, state, commentID, in.Actor, ts); err != nil {
		return nil, err
	}

	occ, found, anchorWall, c, err := s.nextAfterResolve(sc, rec, x, nowT)
	if errors.Is(err, errNoExactStart) {
		// Unreachable for a real zone (see exactElapsedStart): the schedule goes
		// to its error state, like a stored rule that no longer compiles, not a 500.
		tx.Rollback()
		code := string(schedule.CodeUnsupported)
		// Guarded by the snapshot read: a timing edit landing after the rollback is not marked broken (r4-1).
		if _, uerr := s.db.Exec(s.q(`UPDATE item_schedules SET error_code = ?, updated_at = ? WHERE id = ? AND updated_at = ?`), code, ts, sc.ID, sc.UpdatedAt); uerr != nil {
			return nil, fmt.Errorf("record schedule error_code: %w", uerr)
		}
		return nil, &ErrScheduleBroken{Code: code}
	}
	if err != nil {
		return nil, err
	}
	var nextAt, nextWall, endedAt any
	var remindAt string
	if found {
		arm, err := armSchedule(c, occ, sc.LeadMinutes, nil)
		if err != nil {
			return nil, err
		}
		nextAt, nextWall, remindAt = NormalizeInstant(arm.NextAt), arm.NextWall.String(), NormalizeInstant(arm.RemindAt)
	} else {
		endedAt = ts
	}
	if _, err := tx.Exec(s.q(`
		UPDATE item_schedules SET next_at = ?, next_wall = ?, next_override_at = NULL, anchor_wall = ?, ended_at = ?, updated_at = ?
		WHERE id = ?`), nextAt, nextWall, nullStr(anchorWall), endedAt, ts, sc.ID); err != nil {
		return nil, fmt.Errorf("advance schedule: %w", err)
	}
	if !found {
		if err := s.deleteManagedReminderTx(tx, sc.ID); err != nil {
			return nil, err
		}
	} else if ok, err := s.rearmManagedReminderTx(tx, sc.ID, remindAt); err != nil {
		return nil, err
	} else if !ok {
		if err := s.insertManagedReminderTx(tx, workspaceID, itemID, sc.ID, remindAt); err != nil {
			return nil, err
		}
	}
	return s.commitResolve(tx, workspaceID, itemID, res)
}

func (s *Store) commitResolve(tx *sql.Tx, workspaceID, itemID string, res *ResolveResult) (*ResolveResult, error) {
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("resolve schedule: %w", err)
	}
	var err error
	if res.Schedule, err = s.GetSchedule(workspaceID, itemID); err != nil {
		return nil, err
	}
	return res, nil
}

// nextAfterResolve is the occurrence a done or skip advances to, and the
// anchor_wall to store. A calendar schedule takes the first instance after both
// now-1s and the resolved occurrence (so an early resolve does not return the
// occurrence it just closed, and an instance at exactly now-1s is missed, never
// also next). A completion schedule takes a recurrence interval from now. Both
// walk the Compiled built from anchor_wall, never from a dtstart years back.
func (s *Store) nextAfterResolve(sc *models.ItemSchedule, rec *time.Location, x, nowT time.Time) (schedule.Occurrence, bool, *string, *schedule.Compiled, error) {
	anchorWall := sc.AnchorWall
	if sc.Anchor == "completion" {
		var hasDates = len(sc.RDates) > 0 || len(sc.ExDates) > 0
		rule, plain, err := plainInterval(derefStr(sc.Rrule), hasDates)
		if err != nil {
			return schedule.Occurrence{}, false, nil, nil, err
		}
		if plain {
			// A legacy row that is not a plain interval falls through to the
			// calendar path rather than being stranded.
			return s.nextAfterCompletion(sc, rule, rec, x, nowT)
		}
	}
	start := sc.Dtstart
	if sc.AnchorWall != nil {
		start = *sc.AnchorWall
	}
	c, err := compileScheduleSpec(storedSpecInput(sc, start))
	if err != nil {
		return schedule.Occurrence{}, false, nil, nil, err
	}
	// Like the fire, a calendar resolve moves the start to the occurrence it
	// closes where that provably changes nothing, so a rule that is always
	// resolved early still walks from near now, not from a dtstart years back.
	loc := rec
	if !sc.Floating {
		if loc, err = time.LoadLocation(c.Spec().Zone); err != nil {
			return schedule.Occurrence{}, false, nil, nil, fmt.Errorf("load schedule zone: %w", err)
		}
	}
	if w, err := schedule.ParseWall(*sc.NextWall); err == nil {
		if cand := reanchorCandidate(sc, c.Spec(), w, x, loc); cand != nil {
			c, anchorWall = cand, ptr(w.String())
		}
	}
	o, _, err := walkOrigin(sc, rec)
	if err != nil {
		return schedule.Occurrence{}, false, nil, nil, err
	}
	t := nowT.Add(-time.Second)
	if o.After(t) {
		t = o
	}
	occ, found, err := c.NextAfter(t, rec, schedule.DefaultLimits)
	return occ, found, anchorWall, c, err
}

func (s *Store) nextAfterCompletion(sc *models.ItemSchedule, rule schedule.Rule, rec *time.Location, x, nowT time.Time) (schedule.Occurrence, bool, *string, *schedule.Compiled, error) {
	none := func(err error) (schedule.Occurrence, bool, *string, *schedule.Compiled, error) {
		return schedule.Occurrence{}, false, nil, nil, err
	}
	if rule.Freq == "" { // a one-shot: done ends it
		c, err := compileScheduleSpec(storedSpecInput(sc, sc.Dtstart))
		return schedule.Occurrence{}, false, nil, c, err
	}
	loc := rec
	if !sc.Floating {
		var err error
		if loc, err = time.LoadLocation(*sc.Tz); err != nil {
			return none(fmt.Errorf("load schedule zone: %w", err))
		}
	}
	w, ok, err := completionWall(sc, rule, loc, nowT)
	if err != nil {
		return none(err)
	}
	if !ok {
		c, err := compileScheduleSpec(storedSpecInput(sc, sc.Dtstart))
		return schedule.Occurrence{}, false, nil, c, err
	}
	// The grid is re-phased to the completion. A calendar-unit rule starts at
	// the candidate; an elapsed grid starts at an instant whose wall reads back
	// exactly (see exactElapsedStart), and still contains done+span.
	span := completionSpan(rule)
	if span > 0 {
		st, ok := exactElapsedStart(loc, span, nowT)
		if !ok {
			return none(errNoExactStart)
		}
		w = wallOf(st.In(loc))
	}
	start := w.String()
	c, err := compileScheduleSpec(storedSpecInput(sc, start))
	var mal *schedule.MalformedError
	if errors.As(err, &mal) && mal.Part == "UNTIL" {
		// The candidate is already past the rule's UNTIL: nothing follows.
		old, oerr := compileScheduleSpec(storedSpecInput(sc, sc.Dtstart))
		return schedule.Occurrence{}, false, nil, old, oerr
	}
	if err != nil {
		return none(err)
	}
	// HOURLY and MINUTELY are exact: the floor is done+span itself, since
	// Resolve of its wall reads a fall-back hour's wall as the earlier half.
	// Every search is also floored at X, so the next is strictly after the
	// occurrence this resolve closed, as on the calendar path.
	floor := schedule.Resolve(loc, w)
	if span > 0 {
		floor = nowT.Add(span)
	}
	floor = floor.Add(-time.Second)
	if x.After(floor) {
		floor = x
	}
	occ, found, err := c.NextAfter(floor, rec, schedule.DefaultLimits)
	if err != nil || !found {
		return none(err)
	}
	return occ, true, &start, c, nil
}

var errNoExactStart = errors.New("no elapsed start reads back exactly")

// exactElapsedStart is the start of an HOURLY or MINUTELY completion grid. The
// engine reads a Start as Resolve(loc, wall), the EARLIER half of a repeated
// wall, so a start must read back to itself. The first of done, done+span and
// done-k*span (k*span >= 1h, one step back out of a repeated hour) that does is
// taken; each has done+span on its grid. None can fail for a real zone, whose
// repeated walls are at most an hour wide.
func exactElapsedStart(loc *time.Location, span time.Duration, done time.Time) (time.Time, bool) {
	for _, s := range []time.Time{done, done.Add(span), done.Add(-span * ((time.Hour + span - 1) / span))} {
		if schedule.Resolve(loc, wallOf(s.In(loc))).Equal(s) {
			return s, true
		}
	}
	return time.Time{}, false
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// pinScheduleTx holds the item, the workspace and the schedule for the rest of
// the transaction, in that order, in one statement: the same shape as a fire's
// pin, so the two conflict at the first lock. SQLite serialises writers and
// takes no lock clause. It answers ErrScheduleItemGone when the item is not live
// and ErrScheduleNotFound when it has no schedule.
func (s *Store) pinScheduleTx(tx *sql.Tx, workspaceID, itemID string) error {
	lock := ""
	if s.dialect.Driver() == DriverPostgres {
		lock = ` FOR NO KEY UPDATE OF i, w, sc`
	}
	var id string
	err := tx.QueryRow(s.q(`
		SELECT sc.id FROM item_schedules sc
		JOIN items i ON i.id = sc.item_id AND i.workspace_id = sc.workspace_id
		JOIN workspaces w ON w.id = i.workspace_id
		WHERE sc.item_id = ? AND sc.workspace_id = ? AND i.deleted_at IS NULL AND w.deleted_at IS NULL`+lock),
		itemID, workspaceID).Scan(&id)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("pin schedule: %w", err)
	}
	var one int
	switch err := tx.QueryRow(s.q(`SELECT 1 FROM items WHERE id = ? AND workspace_id = ? AND deleted_at IS NULL`),
		itemID, workspaceID).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrScheduleItemGone
	case err != nil:
		return fmt.Errorf("check schedule item: %w", err)
	}
	return ErrScheduleNotFound
}

// upsertOccurrenceTx writes the resolved occurrence's row, replacing a `moved`
// row an earlier move left for it. The identity is the UTC instant; an all-day
// occurrence is also identified by its date, because its instant shifts with the
// zone (the partial unique index).
func (s *Store) upsertOccurrenceTx(tx *sql.Tx, scheduleID, at, wall, state, commentID, actor, ts string) error {
	// An all-day row is matched on its date first and on the instant only when
	// no row has that date, so the UPDATE can never meet two rows (one by
	// instant, one by date) and set a duplicate wall.
	keys := []struct{ col, val string }{{"occurrence_at", at}}
	if len(wall) == 10 {
		keys = []struct{ col, val string }{{"occurrence_wall", wall}, {"occurrence_at", at}}
	}
	for _, k := range keys {
		res, err := tx.Exec(s.q(`
			UPDATE schedule_occurrences SET occurrence_wall = ?, state = ?, comment_id = ?, actor = ?, resolved_at = ?
			WHERE schedule_id = ? AND `+k.col+` = ?`), wall, state, nilIfEmpty(commentID), actor, ts, scheduleID, k.val)
		if err != nil {
			return fmt.Errorf("update occurrence: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n > 0 {
			return nil
		}
	}
	if _, err := tx.Exec(s.q(`
		INSERT INTO schedule_occurrences (id, schedule_id, occurrence_at, occurrence_wall, state, comment_id, actor, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		newID(), scheduleID, at, wall, state, nilIfEmpty(commentID), actor, ts); err != nil {
		return fmt.Errorf("insert occurrence: %w", err)
	}
	return nil
}

// insertMissedTx records one passed-over instance; ON CONFLICT DO NOTHING covers
// both unique indexes, so a row already there is left alone. It reports whether
// a row was written.
func (s *Store) insertMissedTx(tx *sql.Tx, scheduleID string, o schedule.Occurrence, ts string) (int, error) {
	res, err := tx.Exec(s.q(`
		INSERT INTO schedule_occurrences (id, schedule_id, occurrence_at, occurrence_wall, state, comment_id, actor, resolved_at)
		VALUES (?, ?, ?, ?, 'missed', NULL, '', ?)
		ON CONFLICT DO NOTHING`),
		newID(), scheduleID, o.At.UTC().Format(time.RFC3339), o.Wall.String(), ts)
	if err != nil {
		return 0, fmt.Errorf("insert missed occurrence: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}
