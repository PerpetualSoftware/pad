package store

import (
	"database/sql"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// LiveRecurringScheduleTx reads the closing guard under the item update's
// transaction. Pin item then workspace, matching commitment writers. The item
// lock also excludes schedule creation (which takes FOR SHARE), including when
// there is currently no schedule. The workspace lock excludes graph cascades
// that can change this schedule while holding another root item.
func (s *Store) LiveRecurringScheduleTx(tx *sql.Tx, workspaceID, itemID string) (*models.ItemSchedule, error) {
	if s.dialect.Driver() == DriverPostgres {
		var id string
		if err := tx.QueryRow(s.q(`SELECT id FROM items WHERE id=? AND workspace_id=? FOR NO KEY UPDATE`), itemID, workspaceID).Scan(&id); err != nil {
			return nil, fmt.Errorf("pin closing item: %w", err)
		}
		if err := tx.QueryRow(s.q(`SELECT id FROM workspaces WHERE id=? FOR NO KEY UPDATE`), workspaceID).Scan(&id); err != nil {
			return nil, fmt.Errorf("pin closing workspace: %w", err)
		}
	}
	sc, err := s.getScheduleQ(tx, workspaceID, itemID, false)
	if err != nil {
		return nil, err
	}
	// An exhausted series has EndedAt set.
	if sc == nil || sc.EndedAt != nil || !scheduleRecurs(sc) {
		return nil, nil
	}
	return sc, nil
}

// scheduleRecurs reports whether a schedule has more than one occurrence: any
// RRULE, or, with no RRULE, at least two distinct walls among the start and
// the RDATEs once the EXDATEs are removed. An RDATE that repeats the start or
// is excluded leaves a one-shot. A stored spec that no longer compiles counts
// as recurring, so the close is refused and force still overrides it.
func scheduleRecurs(sc *models.ItemSchedule) bool {
	if sc.Rrule != nil && *sc.Rrule != "" {
		return true
	}
	if len(sc.RDates) == 0 {
		return false
	}
	c, err := compileScheduleSpec(storedSpecInput(sc, sc.Dtstart))
	if err != nil {
		return true
	}
	spec := c.Spec()
	excluded := map[string]bool{}
	for _, w := range spec.ExDates {
		excluded[w.String()] = true
	}
	walls := map[string]bool{}
	for _, w := range append([]schedule.Wall{spec.Start}, spec.RDates...) {
		if !excluded[w.String()] {
			walls[w.String()] = true
		}
	}
	return len(walls) > 1
}
