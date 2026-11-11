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

// CommitmentAccessItemsQ returns the seed component over links and gates,
// plus owners of the seed journal lineage. Deleted rows retain their marker.
func (s *Store) CommitmentAccessItemsQ(q Queryer, workspaceID, itemID string) ([]CommitmentAccessItem, error) {
	rows, err := q.Query(s.q(`WITH RECURSIVE edges(a,b) AS (
 SELECT p.item_id,c.item_id FROM schedule_links l
 JOIN item_schedules p ON p.id=l.parent_schedule_id
 JOIN item_schedules c ON c.id=l.child_schedule_id WHERE l.workspace_id=?
 UNION SELECT blocked_item_id,blocker_item_id FROM schedule_gates WHERE workspace_id=?
 ), component(id) AS (
 SELECT id FROM items WHERE workspace_id=? AND id=?
 UNION SELECT CASE WHEN e.a=c.id THEN e.b ELSE e.a END
 FROM component c JOIN edges e ON e.a=c.id OR e.b=c.id
 )
 SELECT id,collection_id,deleted_at FROM items WHERE workspace_id=? AND id IN (
 SELECT id FROM component
 UNION SELECT lineage.item_id FROM schedule_changes own JOIN schedule_changes lineage ON lineage.id=own.root_change_id
 WHERE own.workspace_id=? AND own.item_id=?
 UNION SELECT lineage.item_id FROM schedule_changes own JOIN schedule_changes lineage ON lineage.id=own.parent_change_id
 WHERE own.workspace_id=? AND own.item_id=?
 ) ORDER BY id`), workspaceID, workspaceID, workspaceID, itemID, workspaceID, workspaceID, itemID, workspaceID, itemID)
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

// reloadGraphAccess checks both ends after the pin. Direct store callers may
// keep their supplied snapshot; HTTP always supplies a transaction loader.
func reloadGraphAccess(q Queryer, access models.CommitmentAccess, loaders []CommitmentAccessLoader, ends ...string) (models.CommitmentAccess, error) {
	if len(loaders) > 0 && loaders[0] != nil {
		var err error
		access, err = loaders[0](q)
		if err != nil {
			return models.CommitmentAccess{}, err
		}
	}
	for _, id := range ends {
		if !access.Readable[id] {
			return access, ErrScheduleLinkNotFound
		}
	}
	for _, id := range ends {
		if !access.Editable[id] {
			return access, ErrLinkEditRequired
		}
	}
	return access, nil
}
