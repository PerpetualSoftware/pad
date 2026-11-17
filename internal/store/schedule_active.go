package store

import (
	"database/sql"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
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
	// Recurring means an RRULE or any RDATE: an RDATE-only schedule still has
	// occurrences after the next one. An exhausted series has EndedAt set.
	if sc == nil || sc.EndedAt != nil || ((sc.Rrule == nil || *sc.Rrule == "") && len(sc.RDates) == 0) {
		return nil, nil
	}
	return sc, nil
}
