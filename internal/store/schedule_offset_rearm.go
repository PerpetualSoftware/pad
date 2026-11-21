package store

import (
	"errors"
	"fmt"
	"time"
)

// RetimeTargetOffsetReminders repairs the managed reminders armed before the
// reminder followed the target (TASK-4713): on a live, unoverridden schedule
// with a native target offset, a row still armed at exactly the old instant,
// its outstanding occurrence's start minus the lead, moves to its target minus
// the lead. Only that exact old value moves, so a row armed by the current
// code (target minus the lead, a move's inside-the-lead target, a floating
// row deferred to its exact instant) is never touched, and a second run
// returns 0. A schedule whose instant cannot be computed (an unloadable zone,
// an unparsable stored wall) is skipped and reported, and the others are
// still repaired: one bad row must not block the repair on every boot. It
// returns how many rows moved.
func (s *Store) RetimeTargetOffsetReminders() (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	lock := ""
	if s.dialect.Driver() == DriverPostgres {
		lock = ` FOR NO KEY UPDATE OF sc`
	}
	rows, err := tx.Query(s.q(`
		SELECT sc.id FROM item_schedules sc
		WHERE sc.ended_at IS NULL AND sc.next_wall IS NOT NULL AND sc.next_override_at IS NULL
		  AND (sc.target_days <> 0 OR sc.target_secs <> 0)
		  AND EXISTS (SELECT 1 FROM item_reminders r WHERE r.schedule_id = sc.id AND r.fired_at IS NULL)
		ORDER BY sc.id` + lock))
	if err != nil {
		return 0, fmt.Errorf("list target offset schedules: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	moved := 0
	var skipped []error
	for _, id := range ids {
		sc, err := s.getScheduleByIDQ(tx, id)
		if err != nil {
			return 0, err
		}
		if sc == nil || sc.NextAt == nil || sc.NextWall == nil {
			continue
		}
		roles, err := s.scheduleRolesOf(tx, sc)
		if err != nil {
			return 0, err
		}
		// storedFireInstant is pure arithmetic over the row: its errors leave
		// the transaction usable, so the row is skipped and the rest go on.
		stale, err := storedFireInstant(sc, sc.LeadMinutes, targetOffset{})
		var fire time.Time
		if err == nil {
			fire, err = storedFireInstant(sc, sc.LeadMinutes, roles.target())
		}
		if err != nil {
			skipped = append(skipped, fmt.Errorf("schedule %s: %w", id, err))
			continue
		}
		res, err := tx.Exec(s.q(`
			UPDATE item_reminders SET remind_at = ?, updated_at = ?
			WHERE schedule_id = ? AND fired_at IS NULL AND remind_at = ? AND remind_at <> ?`),
			NormalizeInstant(fire), now(), sc.ID, NormalizeInstant(stale), NormalizeInstant(fire))
		if err != nil {
			return 0, fmt.Errorf("retime schedule %s: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if n > 0 {
			if err := s.invalidateScheduleReopenTx(tx, sc.ID); err != nil {
				return 0, err
			}
			moved++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if len(skipped) > 0 {
		return moved, fmt.Errorf("skipped %d schedules: %w", len(skipped), errors.Join(skipped...))
	}
	return moved, nil
}
