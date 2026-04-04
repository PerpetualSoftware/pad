package store

import (
	"fmt"
	"strings"

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
	var results []SearchResult

	// Check if the query looks like an item ref (e.g. "TASK-5", "BUG-8")
	// and do a direct lookup first so refs are always findable.
	if prefix, number, ok := parseItemRef(strings.TrimSpace(params.Query)); ok {
		refQuery := `
			SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
			       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
			       i.created_by, i.last_modified_by, i.source,
			       i.item_number, i.created_at, i.updated_at,
			       c.slug, c.name, c.icon, c.prefix,
			       COALESCE(au.name, ''), COALESCE(au.email, ''),
			       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, '')
			FROM items i
			JOIN collections c ON c.id = i.collection_id
			LEFT JOIN users au ON au.id = i.assigned_user_id
			LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
			WHERE c.prefix = ? AND i.item_number = ? AND i.deleted_at IS NULL
		`
		refArgs := []interface{}{prefix, number}

		if params.Workspace != "" {
			refQuery += ` AND i.workspace_id = (SELECT id FROM workspaces WHERE slug = ? AND deleted_at IS NULL)`
			refArgs = append(refArgs, params.Workspace)
		}

		refRows, err := s.db.Query(refQuery, refArgs...)
		if err == nil {
			defer refRows.Close()
			for refRows.Next() {
				var r SearchResult
				var createdAt, updatedAt string
				var pinned int
				if err := refRows.Scan(
					&r.Item.ID, &r.Item.WorkspaceID, &r.Item.CollectionID, &r.Item.Title, &r.Item.Slug,
					&r.Item.Content, &r.Item.Fields, &r.Item.Tags,
					&pinned, &r.Item.SortOrder, &r.Item.ParentID, &r.Item.AssignedUserID, &r.Item.AgentRoleID, &r.Item.RoleSortOrder,
					&r.Item.CreatedBy, &r.Item.LastModifiedBy,
					&r.Item.Source, &r.Item.ItemNumber, &createdAt, &updatedAt,
					&r.Item.CollectionSlug, &r.Item.CollectionName, &r.Item.CollectionIcon, &r.Item.CollectionPrefix,
					&r.Item.AssignedUserName, &r.Item.AssignedUserEmail,
					&r.Item.AgentRoleName, &r.Item.AgentRoleSlug, &r.Item.AgentRoleIcon,
				); err != nil {
					continue
				}
				r.Item.Pinned = pinned == 1
				r.Item.CreatedAt = parseTime(createdAt)
				r.Item.UpdatedAt = parseTime(updatedAt)
				hydrateItemComputedMetadata(&r.Item)
				r.Item.Content = ""
				r.Snippet = r.Item.Title
				r.Rank = -1000 // Best possible rank so it sorts first
				results = append(results, r)
			}
		}
		// If no ref matches, fall through to FTS below
	}

	query := `
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.created_at, i.updated_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, ''),
		       snippet(items_fts, 1, '<mark>', '</mark>', '...', 32) as snippet,
		       rank
		FROM items_fts fts
		JOIN items i ON i.rowid = fts.rowid
		JOIN collections c ON c.id = i.collection_id
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		WHERE items_fts MATCH ?
		AND i.deleted_at IS NULL
	`
	args := []interface{}{sanitizeFTSQuery(params.Query)}

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
		// If we already have a ref match, return that instead of failing
		// (FTS5 may reject queries like "TASK-5" due to special syntax)
		if len(results) > 0 {
			return results, nil
		}
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	// Track IDs we already have from the ref lookup to avoid duplicates
	seen := make(map[string]bool)
	for _, r := range results {
		seen[r.Item.ID] = true
	}

	for rows.Next() {
		var r SearchResult
		var createdAt, updatedAt string
		var pinned int

		if err := rows.Scan(
			&r.Item.ID, &r.Item.WorkspaceID, &r.Item.CollectionID, &r.Item.Title, &r.Item.Slug,
			&r.Item.Content, &r.Item.Fields, &r.Item.Tags,
			&pinned, &r.Item.SortOrder, &r.Item.ParentID, &r.Item.AssignedUserID, &r.Item.AgentRoleID, &r.Item.RoleSortOrder,
			&r.Item.CreatedBy, &r.Item.LastModifiedBy,
			&r.Item.Source, &r.Item.ItemNumber, &createdAt, &updatedAt,
			&r.Item.CollectionSlug, &r.Item.CollectionName, &r.Item.CollectionIcon, &r.Item.CollectionPrefix,
			&r.Item.AssignedUserName, &r.Item.AssignedUserEmail,
			&r.Item.AgentRoleName, &r.Item.AgentRoleSlug, &r.Item.AgentRoleIcon,
			&r.Snippet, &r.Rank,
		); err != nil {
			return nil, err
		}
		// Skip if already included from ref lookup
		if seen[r.Item.ID] {
			continue
		}
		r.Item.Pinned = pinned == 1
		r.Item.CreatedAt = parseTime(createdAt)
		r.Item.UpdatedAt = parseTime(updatedAt)
		hydrateItemComputedMetadata(&r.Item)
		// Don't include full content in search results
		r.Item.Content = ""
		results = append(results, r)
	}
	return results, rows.Err()
}

// sanitizeFTSQuery wraps each token in double quotes so FTS5 treats
// special characters (like hyphens) as literals rather than operators.
func sanitizeFTSQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return q
	}
	tokens := strings.Fields(q)
	for i, t := range tokens {
		// Remove any existing double quotes, then wrap in quotes
		t = strings.ReplaceAll(t, `"`, ``)
		tokens[i] = `"` + t + `"`
	}
	return strings.Join(tokens, " ")
}
