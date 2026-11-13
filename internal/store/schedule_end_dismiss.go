package store

import (
	"errors"
	"fmt"
)

// ErrScheduleNotEnded refuses a dismissal of a schedule that still has an
// outstanding occurrence: there is no ending to dismiss.
var ErrScheduleNotEnded = errors.New("the schedule has an outstanding occurrence; there is nothing ended to dismiss")

// DismissEndedSchedule records that an editor dismissed an ended schedule from
// Outstanding while its item stays open. The dismissal is shared, like an
// acknowledgement. It reports changed=false when the schedule was already
// dismissed, and ErrScheduleNotEnded when it has not ended. A timing edit that
// revives the schedule clears the dismissal.
func (s *Store) DismissEndedSchedule(workspaceID, itemID string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("dismiss ended schedule: %w", err)
	}
	defer tx.Rollback()
	if err := s.pinScheduleTx(tx, workspaceID, itemID); err != nil {
		return false, err
	}
	sc, err := s.getScheduleQ(tx, workspaceID, itemID, false)
	if err != nil {
		return false, err
	}
	if sc == nil {
		return false, ErrScheduleNotFound
	}
	if sc.EndedAt == nil {
		return false, ErrScheduleNotEnded
	}
	if sc.EndedDismissedAt != nil {
		return false, nil
	}
	ts := now()
	if _, err := tx.Exec(s.q(`UPDATE item_schedules SET ended_dismissed_at = ?, updated_at = ?
		WHERE id = ? AND ended_at IS NOT NULL AND ended_dismissed_at IS NULL`), ts, ts, sc.ID); err != nil {
		return false, fmt.Errorf("dismiss ended schedule: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("dismiss ended schedule: %w", err)
	}
	return true, nil
}
