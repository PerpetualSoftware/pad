package store

import (
	"database/sql"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// exportScheduleGatesQ carries every gate of a live blocked item. Deleted
// blockers travel as item rows only, so their gates fall back to item status.
func (s *Store) exportScheduleGatesQ(q Queryer, workspaceID string) ([]models.ScheduleGateExport, error) {
	rows, err := q.Query(s.q(`SELECT g.id,g.blocked_item_id,g.blocker_item_id,
 CASE WHEN blocker.deleted_at IS NULL THEN g.blocker_schedule_id ELSE NULL END,
 CASE WHEN blocker.deleted_at IS NULL THEN g.blocker_occurrence_key ELSE '' END,
 g.opened_at,g.created_at
 FROM schedule_gates g
 JOIN items blocked ON blocked.id=g.blocked_item_id AND blocked.workspace_id=g.workspace_id
 JOIN items blocker ON blocker.id=g.blocker_item_id
 WHERE g.workspace_id=? AND blocked.deleted_at IS NULL ORDER BY g.created_at,g.id`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("export schedule gates: %w", err)
	}
	defer rows.Close()
	var out []models.ScheduleGateExport
	for rows.Next() {
		var g models.ScheduleGateExport
		var sc, opened sql.NullString
		if err := rows.Scan(&g.ID, &g.BlockedItemID, &g.BlockerItemID, &sc, &g.BlockerOccurrenceKey, &opened, &g.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan schedule gate: %w", err)
		}
		if sc.Valid {
			g.BlockerScheduleID = &sc.String
		}
		if opened.Valid {
			g.OpenedAt = &opened.String
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
