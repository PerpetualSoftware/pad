package store

import (
	"fmt"

	"github.com/xarmian/pad/internal/models"
)

type SearchResult struct {
	Item    models.Item `json:"item"`
	Snippet string      `json:"snippet"`
	Rank    float64     `json:"rank"`
}

type SearchParams struct {
	Query     string
	Workspace string // workspace slug, optional
}

func (s *Store) Search(params SearchParams) ([]SearchResult, error) {
	query := `
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.created_by, i.last_modified_by, i.source,
		       i.created_at, i.updated_at,
		       c.slug, c.name, c.icon,
		       snippet(items_fts, 1, '<mark>', '</mark>', '...', 32) as snippet,
		       rank
		FROM items_fts fts
		JOIN items i ON i.rowid = fts.rowid
		JOIN collections c ON c.id = i.collection_id
		WHERE items_fts MATCH ?
		AND i.deleted_at IS NULL
	`
	args := []interface{}{params.Query}

	if params.Workspace != "" {
		query += `
			AND i.workspace_id = (
				SELECT id FROM workspaces WHERE slug = ? AND deleted_at IS NULL
			)
		`
		args = append(args, params.Workspace)
	}

	query += " ORDER BY rank LIMIT 50"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var createdAt, updatedAt string
		var pinned int

		if err := rows.Scan(
			&r.Item.ID, &r.Item.WorkspaceID, &r.Item.CollectionID, &r.Item.Title, &r.Item.Slug,
			&r.Item.Content, &r.Item.Fields, &r.Item.Tags,
			&pinned, &r.Item.SortOrder, &r.Item.ParentID, &r.Item.CreatedBy, &r.Item.LastModifiedBy,
			&r.Item.Source, &createdAt, &updatedAt,
			&r.Item.CollectionSlug, &r.Item.CollectionName, &r.Item.CollectionIcon,
			&r.Snippet, &r.Rank,
		); err != nil {
			return nil, err
		}
		r.Item.Pinned = pinned == 1
		r.Item.CreatedAt = parseTime(createdAt)
		r.Item.UpdatedAt = parseTime(updatedAt)
		// Don't include full content in search results
		r.Item.Content = ""
		results = append(results, r)
	}
	return results, rows.Err()
}
