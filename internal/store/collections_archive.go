package store

import (
	"database/sql"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2189: archiving a collection (DeleteCollection) is a soft delete of the
// collection row; its items are untouched and only hidden by that row's
// deleted_at, and nothing purges an archived collection. These two make that
// reversible: list what is archived, and restore one by clearing deleted_at.
//
// A restore cannot collide: slugs are allocated unique across archived rows
// too (uniqueSlugQ), so a newer collection with the same name took `x-2`; and
// prefixes are not unique even among live collections (item numbers are
// workspace-unique, so refs stay unambiguous).

// ListArchivedCollections returns the workspace's archived collections, most
// recently archived first, each with its count of live items.
func (s *Store) ListArchivedCollections(workspaceID string) ([]models.ArchivedCollection, error) {
	rows, err := s.db.Query(s.q(`
		SELECT c.id, c.name, c.slug, c.prefix, c.icon, c.deleted_at,
		       (SELECT COUNT(*) FROM items i WHERE i.collection_id = c.id AND i.deleted_at IS NULL)
		FROM collections c
		WHERE c.workspace_id = ? AND c.deleted_at IS NOT NULL
		ORDER BY c.deleted_at DESC, c.id`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list archived collections: %w", err)
	}
	defer rows.Close()
	out := []models.ArchivedCollection{}
	for rows.Next() {
		var a models.ArchivedCollection
		var icon sql.NullString
		if err := rows.Scan(&a.ID, &a.Name, &a.Slug, &a.Prefix, &icon, &a.ArchivedAt, &a.ItemCount); err != nil {
			return nil, fmt.Errorf("scan archived collection: %w", err)
		}
		a.Icon = icon.String
		out = append(out, a)
	}
	return out, rows.Err()
}

// ResolveArchivedCollection returns the id of this workspace's ARCHIVED
// collection named by ref, its id or its slug (slugs are unique per workspace
// across archived rows too, so either names at most one). sql.ErrNoRows when
// ref names none (unknown, another workspace's, or live), so a caller cannot
// tell those apart.
func (s *Store) ResolveArchivedCollection(workspaceID, ref string) (string, error) {
	var id string
	err := s.db.QueryRow(s.q(`
		SELECT id FROM collections
		WHERE workspace_id = ? AND (id = ? OR slug = ?) AND deleted_at IS NOT NULL`), workspaceID, ref, ref).Scan(&id)
	return id, err
}

// RestoreCollection un-archives this workspace's archived collection id.
// sql.ErrNoRows when it is not one (already restored by a racing call, or
// never archived).
func (s *Store) RestoreCollection(workspaceID, id string) (*models.Collection, error) {
	ts := now()
	res, err := s.db.Exec(s.q(`
		UPDATE collections SET deleted_at = NULL, updated_at = ?
		WHERE id = ? AND workspace_id = ? AND deleted_at IS NOT NULL`), ts, id, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("restore collection: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("restore collection: %w", err)
	} else if n == 0 {
		return nil, sql.ErrNoRows
	}
	return s.GetCollection(id)
}
