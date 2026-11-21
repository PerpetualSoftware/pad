package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// ScheduleSnoozedUntil derives the re-fire instant for the outstanding occurrence.
// occurrenceState is its history row state, or empty when no row exists. A
// resolved occurrence is never snoozed. A moved one (including a linked move) is
// snoozed only when next_override_at no longer holds its moved target
// (movedTarget, the role=target override): a snooze after the move rewrote it to
// the re-fire instant (TASK-4590). A moved row with no target override reads as
// a plain move. A snooze whose re-fire instant is at or before now has ended, so
// it reads nil.
func ScheduleSnoozedUntil(sc *models.ItemSchedule, occurrenceState string, movedTarget *time.Time, now time.Time) *time.Time {
	if sc.EndedAt != nil || sc.NextWall == nil || sc.NextOverrideAt == nil {
		return nil
	}
	at, err := time.Parse(time.RFC3339, *sc.NextOverrideAt)
	if err != nil {
		return nil
	}
	switch occurrenceState {
	case "":
	case "moved":
		if movedTarget == nil || movedTarget.Equal(at) {
			return nil
		}
	default:
		return nil
	}
	until := at.Add(-time.Duration(sc.LeadMinutes) * time.Minute).UTC()
	if !until.After(now) {
		return nil
	}
	return &until
}

// ReadScheduleSnoozedUntil is for reads without occurrence history already in
// hand. Agenda expansion calls ScheduleSnoozedUntil with its loaded rows instead.
func (s *Store) ReadScheduleSnoozedUntil(sc *models.ItemSchedule, now time.Time) (*time.Time, error) {
	// No override, or one already past: nothing to look up.
	if sc.NextAt == nil || ScheduleSnoozedUntil(sc, "", nil, now) == nil {
		return nil, nil
	}
	column, identity := "occurrence_at", *sc.NextAt
	if sc.Floating || len(*sc.NextWall) == 10 {
		column, identity = "occurrence_wall", *sc.NextWall
	}
	var state string
	err := s.db.QueryRow(s.q(`SELECT state FROM schedule_occurrences WHERE schedule_id = ? AND `+column+` = ?`), sc.ID, identity).Scan(&state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read snoozed occurrence: %w", err)
	}
	var target *time.Time
	if state == "moved" {
		if target, err = s.movedTargetQ(s.db, sc.ID, OccurrenceKey(sc, *sc.NextAt, *sc.NextWall)); err != nil {
			return nil, err
		}
	}
	return ScheduleSnoozedUntil(sc, state, target, now), nil
}

// movedTargetQ reads a moved occurrence's target, the role=target override the
// move wrote; nil when the occurrence has none. next_override_at holds it too
// until a later snooze rewrites that to the re-fire instant.
func (s *Store) movedTargetQ(q Queryer, scheduleID, key string) (*time.Time, error) {
	var value string
	err := q.QueryRow(s.q(`SELECT value_at FROM schedule_occurrence_overrides WHERE schedule_id=? AND occurrence_key=? AND role='target'`), scheduleID, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read moved target: %w", err)
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, fmt.Errorf("parse moved target: %w", err)
	}
	return &t, nil
}
