package store

import (
	"database/sql"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// CommitmentAccessLoader reloads authorization on the mutation's own connection.
type CommitmentAccessLoader = models.CommitmentAccessLoader

// CommitmentAccessItem is the minimal row needed to decide graph access.
type CommitmentAccessItem struct {
	ID           string
	CollectionID string
	DeletedAt    *string
}

// CommitmentAccessItemsQ returns the request item, all workspace graph ends,
// and owners of its journal lineage. Deleted rows retain their policy marker.
func (s *Store) CommitmentAccessItemsQ(q Queryer, workspaceID, itemID string) ([]CommitmentAccessItem, error) {
	rows, err := q.Query(s.q(`SELECT id,collection_id,deleted_at FROM items WHERE workspace_id=? AND id IN (
 SELECT ?
 UNION SELECT p.item_id FROM schedule_links l JOIN item_schedules p ON p.id=l.parent_schedule_id WHERE l.workspace_id=?
 UNION SELECT c.item_id FROM schedule_links l JOIN item_schedules c ON c.id=l.child_schedule_id WHERE l.workspace_id=?
 UNION SELECT blocked_item_id FROM schedule_gates WHERE workspace_id=?
 UNION SELECT blocker_item_id FROM schedule_gates WHERE workspace_id=?
 UNION SELECT lineage.item_id FROM schedule_changes own JOIN schedule_changes lineage ON lineage.id=own.root_change_id
 WHERE own.workspace_id=? AND own.item_id=?
 UNION SELECT lineage.item_id FROM schedule_changes own JOIN schedule_changes lineage ON lineage.id=own.parent_change_id
 WHERE own.workspace_id=? AND own.item_id=?
) ORDER BY id`), workspaceID, itemID, workspaceID, workspaceID, workspaceID, workspaceID, workspaceID, itemID, workspaceID, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CommitmentAccessItem{}
	for rows.Next() {
		it := CommitmentAccessItem{}
		var deleted sql.NullString
		if err := rows.Scan(&it.ID, &it.CollectionID, &deleted); err != nil {
			return nil, err
		}
		if deleted.Valid {
			it.DeletedAt = &deleted.String
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// CommitmentLinkPartnersQ is an unfiltered internal authorization query. Unlike
// ListScheduleLinks it retains deleted and unreadable ends; callers never expose it.
func (s *Store) CommitmentLinkPartnersQ(q Queryer, workspaceID, itemID string) ([]string, error) {
	rows, err := q.Query(s.q(`SELECT CASE WHEN p.item_id=? THEN c.item_id ELSE p.item_id END FROM schedule_links l JOIN item_schedules p ON p.id=l.parent_schedule_id JOIN item_schedules c ON c.id=l.child_schedule_id WHERE l.workspace_id=? AND (p.item_id=? OR c.item_id=?)`), itemID, workspaceID, itemID, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
