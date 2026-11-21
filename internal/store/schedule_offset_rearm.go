package store

import (
	"errors"
	"fmt"
	"time"
)

// RetimeTargetOffsetReminders re-arms every armed managed reminder of a live,
// unoverridden schedule with a native target offset at its target minus the
// lead (TASK-4713). Rows armed before the reminder followed the target were
// armed at the start minus the lead. It is idempotent: a row already at its
// instant is not written, so a second run returns 0. It returns how many rows
// moved.
func (s *Store) RetimeTargetOffsetReminders() (int, error) {
	return s.retimeTargetOffsetRemindersAt(time.Now().UTC())
}

// retimeTargetOffsetRemindersAt is RetimeTargetOffsetReminders at a given
// clock. When target minus the lead is not after nowT the row is armed at the
// target itself, the fallback a move inside the lead uses (armAfterPastMoveTx),
// so a restart never pulls a row that fallback armed back into the past. A
// schedule whose instant cannot be computed (an unloadable zone, an unparsable
// stored wall) is skipped and reported, and the others are still repaired: one
// bad row must not block the repair on every boot.
func (s *Store) retimeTargetOffsetRemindersAt(nowT time.Time) (int, error) {
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
		off := roles.target()
		fire, err := storedFireInstant(sc, sc.LeadMinutes, off)
		if err == nil && !fire.After(nowT) {
			fire, err = storedFireInstant(sc, 0, off)
		}
		if err != nil {
			skipped = append(skipped, fmt.Errorf("schedule %s: %w", id, err))
			continue
		}
		ok, err := s.retimeArmedRowTx(tx, sc.ID, fire)
		if err != nil {
			return 0, fmt.Errorf("retime schedule %s: %w", id, err)
		}
		if ok {
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
