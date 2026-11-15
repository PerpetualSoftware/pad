package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// ScheduleSnoozedUntil derives the re-fire instant for the outstanding occurrence.
// occurrenceState is its history row state, or empty when no row exists. A move
// (including a linked move) and a resolved occurrence are never snoozed, and a
// snooze whose re-fire instant is at or before now has ended, so it reads nil.
func ScheduleSnoozedUntil(sc *models.ItemSchedule, occurrenceState string, now time.Time) *time.Time {
	if sc.EndedAt != nil || sc.NextWall == nil || sc.NextOverrideAt == nil || occurrenceState != "" {
		return nil
	}
	at, err := time.Parse(time.RFC3339, *sc.NextOverrideAt)
	if err != nil {
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
	if sc.EndedAt != nil || sc.NextWall == nil || sc.NextAt == nil || sc.NextOverrideAt == nil {
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
	return ScheduleSnoozedUntil(sc, state, now), nil
}
