package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
	"github.com/PerpetualSoftware/pad/internal/usertz"
)

// Firing a schedule's managed reminder row (TASK-4102 PR1b, DOC-4098).
//
// fireOneReminder takes the row exactly as it takes any reminder: the arbiter
// UPDATE, the outbox event and the notification are the same statements. This
// file is what a MANAGED row adds around that: the PG pin that also holds the
// schedule, the exact-instant check for a floating schedule, the stored-rule
// compile with its error_code, the re-anchor of the schedule's start, and the
// two hooks that keep a floating row's cached instant honest.
//
// LOCK ORDER (Postgres), the population table is in the PR body: item and
// workspace, then the schedule, then the reminder row, in one pin statement and
// the arbiter. Every other writer of a schedule or a managed row takes the
// schedule before the reminder, and none takes the item after the schedule.

// pinManagedReminderTx holds the item, the workspace and the schedule of a
// managed reminder for the rest of the transaction, in that order, in one
// statement (the rows are locked in the order of the OF list). It reports false
// when the item or workspace was archived, or the row or schedule is gone, since
// the scan.
func (s *Store) pinManagedReminderTx(tx *sql.Tx, id string) (bool, error) {
	var pinned string
	err := tx.QueryRow(s.q(`
		SELECT i.id FROM item_reminders r
		JOIN items i ON i.id = r.item_id AND i.workspace_id = r.workspace_id
		JOIN workspaces w ON w.id = i.workspace_id
		JOIN item_schedules sc ON sc.id = r.schedule_id
		WHERE r.id = ? AND i.deleted_at IS NULL AND w.deleted_at IS NULL
		FOR NO KEY UPDATE OF i, w, sc
	`), id).Scan(&pinned)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// getScheduleByIDQ reads a schedule by id, nil when there is none.
func (s *Store) getScheduleByIDQ(q Queryer, id string) (*models.ItemSchedule, error) {
	sc, err := scanSchedule(q.QueryRow(s.q(`SELECT `+scheduleColumns+` FROM item_schedules WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get schedule: %w", err)
	}
	return sc, nil
}

// storedSpecInput is the schedule's timing as stored, with the start moved to
// the re-anchor when there is one.
func storedSpecInput(sc *models.ItemSchedule, start string) scheduleSpecInput {
	in := scheduleSpecInput{Dtstart: start, Floating: sc.Floating, RDates: sc.RDates, ExDates: sc.ExDates}
	if sc.Tz != nil {
		in.Tz = *sc.Tz
	}
	if sc.Rrule != nil {
		in.Rrule = *sc.Rrule
	}
	return in
}

// fireManagedTx decides what a won managed row does. It runs after the arbiter
// UPDATE and the item load, on the pinned transaction, and reports whether the
// caller should emit the event and notification. false means the transaction is
// to be committed as it stands with no event:
//
//   - a floating schedule whose exact instant, in the recipient's zone, is still
//     ahead: the row was armed at the UTC+14 bound and is moved to the exact
//     instant and un-fired (the deferral write);
//   - a stored rule that no longer compiles: the row stays fired, error_code
//     says why, and the next tick does not see it. Never ended silently.
//
// On true, r.OccurrenceAt is set and the schedule's start has been re-anchored
// when it may be.
func (s *Store) fireManagedTx(tx *sql.Tx, r *models.Reminder, nowTS string) (bool, error) {
	sc, err := s.getScheduleByIDQ(tx, *r.ScheduleID)
	if err != nil {
		return false, err
	}
	if sc == nil || sc.NextAt == nil || sc.NextWall == nil {
		return false, errors.New("managed reminder has no outstanding occurrence")
	}
	wall, err := schedule.ParseWall(*sc.NextWall)
	if err != nil {
		return false, fmt.Errorf("parse next_wall: %w", err)
	}
	var override *time.Time
	if sc.NextOverrideAt != nil {
		o, err := time.Parse(time.RFC3339, *sc.NextOverrideAt)
		if err != nil {
			return false, fmt.Errorf("parse next_override_at: %w", err)
		}
		override = &o
	}

	var loc *time.Location
	var at time.Time
	if sc.Floating {
		// The recipient's zone NOW, not when the row was armed: reassignment and
		// zone edits since then are honoured here.
		if loc, err = s.scheduleRecipientZone(tx, r.ItemID); err != nil {
			return false, err
		}
		at = schedule.Resolve(loc, wall)
		if precise := NormalizeInstant(fireInstant(at, wall, loc, sc.LeadMinutes, override)); precise > nowTS {
			if _, err := tx.Exec(s.q(`UPDATE item_reminders SET remind_at = ?, fired_at = NULL, updated_at = ? WHERE id = ?`),
				precise, now(), r.ID); err != nil {
				return false, fmt.Errorf("defer floating reminder: %w", err)
			}
			return false, nil
		}
	}

	start := sc.Dtstart
	if sc.AnchorWall != nil {
		start = *sc.AnchorWall
	}
	c, err := compileScheduleSpec(storedSpecInput(sc, start))
	if err != nil {
		code := ScheduleErrorCode(err)
		if code == "" {
			code = string(schedule.CodeUnsupported)
		}
		// The code and the ids only; the rule text stays out of the log.
		slog.Warn("schedule fire: stored rule does not compile; left fired with no event",
			"schedule_id", sc.ID, "item_id", r.ItemID, "error_code", code)
		if _, err := tx.Exec(s.q(`UPDATE item_schedules SET error_code = ?, updated_at = ? WHERE id = ?`),
			code, now(), sc.ID); err != nil {
			return false, fmt.Errorf("record schedule error_code: %w", err)
		}
		return false, nil
	}
	spec := c.Spec()

	if !sc.Floating {
		if loc, err = time.LoadLocation(spec.Zone); err != nil {
			return false, fmt.Errorf("load schedule zone: %w", err)
		}
		if at, err = time.Parse(time.RFC3339, *sc.NextAt); err != nil {
			return false, fmt.Errorf("parse next_at: %w", err)
		}
	}
	r.OccurrenceAt = ptr(at.UTC().Format(time.RFC3339))

	if err := s.reanchorTx(tx, sc, spec, wall, at, loc); err != nil {
		return false, err
	}
	return true, nil
}

func ptr[T any](v T) *T { return &v }

// reanchorTx moves the schedule's start to the occurrence that just fired, so
// the next walk of the rule starts near now instead of at a dtstart years back
// (which would hit CapScan or CapStall). It is written only inside the winning
// fire transaction, and only where the moved start provably yields the same
// occurrences:
//
//   - the rule has no COUNT (COUNT counts from the start);
//   - the fired wall is not one of the RDATEs (the rule did not produce it);
//   - the wall resolves to the occurrence's instant (not the repeated half of a
//     fall-back hour on an elapsed rule);
//   - the rule itself produces the new start (Compile's StartSynced), so the
//     INTERVAL and BYSETPOS phase is kept.
//
// Otherwise the old anchor stays. spec is the compiled stored spec.
func (s *Store) reanchorTx(tx *sql.Tx, sc *models.ItemSchedule, spec schedule.Spec, wall schedule.Wall, at time.Time, loc *time.Location) error {
	if spec.Rule == "" || spec.Start.String() == wall.String() {
		return nil
	}
	rule, err := schedule.ParseRule(spec.Rule)
	if err != nil || rule.Count != 0 {
		return nil
	}
	if slices.Contains(wallStrings(spec.RDates), wall.String()) || !schedule.Resolve(loc, wall).Equal(at) {
		return nil
	}
	cand, err := compileScheduleSpec(storedSpecInput(sc, wall.String()))
	if err != nil || !cand.StartSynced() {
		return nil
	}
	if _, err := tx.Exec(s.q(`UPDATE item_schedules SET anchor_wall = ? WHERE id = ?`), wall.String(), sc.ID); err != nil {
		return fmt.Errorf("re-anchor schedule: %w", err)
	}
	return nil
}

// fillOccurrenceAt sets OccurrenceAt on the managed rows among rs in one query:
// the outstanding occurrence's instant. A pinned schedule stores it; a floating
// one stores only the early bound, so it is read from next_wall in the current
// recipient's zone, the instant it will fire at.
func (s *Store) fillOccurrenceAt(q Queryer, rs ...*models.Reminder) error {
	byID := map[string][]*models.Reminder{}
	var ids []any
	for _, r := range rs {
		if r == nil || r.ScheduleID == nil {
			continue
		}
		if _, ok := byID[*r.ScheduleID]; !ok {
			ids = append(ids, *r.ScheduleID)
		}
		byID[*r.ScheduleID] = append(byID[*r.ScheduleID], r)
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(s.q(`
		SELECT sc.id, sc.next_at, sc.next_wall, sc.floating, u.timezone
		FROM item_schedules sc
		JOIN items i ON i.id = sc.item_id AND i.workspace_id = sc.workspace_id
		LEFT JOIN users u ON u.id = COALESCE(NULLIF(i.assigned_user_id, ''), NULLIF(i.created_by_user_id, ''))
		WHERE sc.id IN (`+placeholders(len(ids))+`)`), ids...)
	if err != nil {
		return fmt.Errorf("read occurrence instants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var nextAt, nextWall, tz sql.NullString
		var floating bool
		if err := rows.Scan(&id, &nextAt, &nextWall, &floating, &tz); err != nil {
			return fmt.Errorf("scan occurrence instant: %w", err)
		}
		var occ *string
		switch {
		case !nextAt.Valid:
		case !floating:
			occ = &nextAt.String
		case nextWall.Valid:
			w, err := schedule.ParseWall(nextWall.String)
			if err != nil {
				return fmt.Errorf("parse next_wall: %w", err)
			}
			var stored *string
			if tz.Valid {
				stored = &tz.String
			}
			occ = ptr(schedule.Resolve(usertz.Resolve(stored).Loc, w).UTC().Format(time.RFC3339))
		}
		for _, r := range byID[id] {
			r.OccurrenceAt = occ
		}
	}
	return rows.Err()
}

// The two recipient hooks (plan Q2, ruling option B with both hooks).
//
// A floating row is armed at the UTC+14 bound and, once the tick finds it early,
// moved to its exact instant in the recipient's zone. If the recipient's zone
// then moves EAST (an account edits its zone, or the item is reassigned to
// someone further east) the cached exact instant is later than the new truth and
// the row would fire late by up to the difference, 26 h at most. These reset the
// affected ARMED floating rows to the bound, so the next tick re-decides them.
//
// Each hook runs in its OWN transaction after the triggering write has
// committed, so it holds nothing else while it takes locks: the schedule rows
// first (in id order, so two hooks cannot cross), then each reminder row, which
// is the order every schedule writer uses. Correctness never rests on a hook
// (the fire re-resolves the zone), so a failure is logged, not returned: it
// must not fail the user's write.

const recipientOf = `COALESCE(NULLIF(i.assigned_user_id, ''), NULLIF(i.created_by_user_id, ''))`

// resetFloatingForUser resets the armed floating managed rows whose recipient is
// userID, after that user's time zone changed.
func (s *Store) resetFloatingForUser(userID string) {
	if err := s.resetFloatingBound(recipientOf+` = ?`, userID); err != nil {
		slog.Warn("reset floating schedule rows after a time zone change failed", "user_id", userID, "error", err)
	}
}

// resetFloatingForItem resets the item's armed floating managed row, after its
// assignee changed.
func (s *Store) resetFloatingForItem(itemID string) {
	if err := s.resetFloatingBound(`i.id = ?`, itemID); err != nil {
		slog.Warn("reset floating schedule row after an assignee change failed", "item_id", itemID, "error", err)
	}
}

func (s *Store) resetFloatingBound(match string, arg string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	lock := ""
	if s.dialect.Driver() == DriverPostgres {
		lock = ` FOR NO KEY UPDATE OF sc`
	}
	rows, err := tx.Query(s.q(`
		SELECT sc.id FROM item_schedules sc
		JOIN items i ON i.id = sc.item_id AND i.workspace_id = sc.workspace_id
		WHERE sc.floating = ? AND sc.ended_at IS NULL AND `+match+`
		  AND EXISTS (SELECT 1 FROM item_reminders r WHERE r.schedule_id = sc.id AND r.fired_at IS NULL)
		ORDER BY sc.id`+lock), s.dialect.BoolToInt(true), arg)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, id := range ids {
		sc, err := s.getScheduleByIDQ(tx, id)
		if err != nil {
			return err
		}
		if sc == nil || sc.NextAt == nil || sc.NextWall == nil {
			continue
		}
		bound, err := storedFireInstant(sc, sc.LeadMinutes)
		if err != nil {
			return err
		}
		// Only an armed row, and only if it actually moves.
		if _, err := tx.Exec(s.q(`
			UPDATE item_reminders SET remind_at = ?, updated_at = ?
			WHERE schedule_id = ? AND fired_at IS NULL AND remind_at <> ?`),
			NormalizeInstant(bound), now(), id, NormalizeInstant(bound)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
