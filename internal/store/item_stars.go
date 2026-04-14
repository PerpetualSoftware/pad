package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xarmian/pad/internal/models"
)

// StarItem stars an item for a user. Idempotent — re-starring is a no-op.
func (s *Store) StarItem(userID, itemID string) error {
	ts := now()
	_, err := s.db.Exec(s.q(`
		INSERT INTO item_stars (user_id, item_id, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT (user_id, item_id) DO NOTHING
	`), userID, itemID, ts)
	if err != nil {
		return fmt.Errorf("star item: %w", err)
	}
	return nil
}

// UnstarItem removes a star from an item for a user. Returns sql.ErrNoRows if not starred.
func (s *Store) UnstarItem(userID, itemID string) error {
	result, err := s.db.Exec(
		s.q("DELETE FROM item_stars WHERE user_id = ? AND item_id = ?"),
		userID, itemID,
	)
	if err != nil {
		return fmt.Errorf("unstar item: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// IsItemStarred checks whether a specific item is starred by a user.
func (s *Store) IsItemStarred(userID, itemID string) (bool, error) {
	var count int
	err := s.db.QueryRow(
		s.q("SELECT COUNT(*) FROM item_stars WHERE user_id = ? AND item_id = ?"),
		userID, itemID,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check item starred: %w", err)
	}
	return count > 0, nil
}

// AreItemsStarred returns a set of item IDs that are starred by the given user.
// Pass the item IDs you want to check; the returned map contains only the starred ones.
func (s *Store) AreItemsStarred(userID string, itemIDs []string) (map[string]bool, error) {
	result := make(map[string]bool)
	if len(itemIDs) == 0 {
		return result, nil
	}

	// Build placeholders for IN clause
	placeholders := make([]string, len(itemIDs))
	args := make([]any, 0, len(itemIDs)+1)
	args = append(args, userID)
	for i, id := range itemIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}

	query := fmt.Sprintf(
		"SELECT item_id FROM item_stars WHERE user_id = ? AND item_id IN (%s)",
		strings.Join(placeholders, ", "),
	)

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("check items starred: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var itemID string
		if err := rows.Scan(&itemID); err != nil {
			return nil, err
		}
		result[itemID] = true
	}
	return result, rows.Err()
}

// ListStarredItems returns all items starred by a user in a workspace, enriched with
// collection and assignment info. Only non-deleted items are returned.
// If includeTerminal is false, items in terminal statuses are excluded.
func (s *Store) ListStarredItems(userID, workspaceID string, includeTerminal bool) ([]models.Item, error) {
	query := `
		SELECT i.id, i.workspace_id, i.collection_id, i.title, i.slug, i.content, i.fields, i.tags,
		       i.pinned, i.sort_order, i.parent_id, i.assigned_user_id, i.agent_role_id, i.role_sort_order,
		       i.created_by, i.last_modified_by, i.source,
		       i.item_number, i.created_at, i.updated_at,
		       c.slug, c.name, c.icon, c.prefix,
		       COALESCE(au.name, ''), COALESCE(au.email, ''),
		       COALESCE(ar.name, ''), COALESCE(ar.slug, ''), COALESCE(ar.icon, '')
		FROM item_stars s
		JOIN items i ON i.id = s.item_id
		JOIN collections c ON c.id = i.collection_id
		LEFT JOIN users au ON au.id = i.assigned_user_id
		LEFT JOIN agent_roles ar ON ar.id = i.agent_role_id
		WHERE s.user_id = ?
		  AND i.workspace_id = ?
		  AND i.deleted_at IS NULL
		ORDER BY s.created_at DESC
	`

	rows, err := s.db.Query(s.q(query), userID, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list starred items: %w", err)
	}
	defer rows.Close()

	items, err := scanItems(rows)
	if err != nil {
		return nil, err
	}

	if !includeTerminal {
		// Build a schema map from workspace collections for per-collection terminal status checks
		schemaMap, err := s.buildCollectionSchemaMap(workspaceID)
		if err != nil {
			return nil, fmt.Errorf("list starred items: build schema map: %w", err)
		}

		filtered := make([]models.Item, 0, len(items))
		for _, item := range items {
			status := extractStatusFromFields(item.Fields)
			if !isTerminalWithSchema(status, item.CollectionID, schemaMap) {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}

	return items, nil
}

// CountStarredItems returns the number of starred items for a user in a workspace.
func (s *Store) CountStarredItems(userID, workspaceID string) (int, error) {
	var count int
	err := s.db.QueryRow(s.q(`
		SELECT COUNT(*)
		FROM item_stars s
		JOIN items i ON i.id = s.item_id
		WHERE s.user_id = ? AND i.workspace_id = ? AND i.deleted_at IS NULL
	`), userID, workspaceID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count starred items: %w", err)
	}
	return count, nil
}

// buildCollectionSchemaMap loads all collections for a workspace and returns
// a map from collection ID to parsed schema, for terminal status lookups.
func (s *Store) buildCollectionSchemaMap(workspaceID string) (map[string]models.CollectionSchema, error) {
	collections, err := s.ListCollections(workspaceID)
	if err != nil {
		return nil, err
	}
	m := make(map[string]models.CollectionSchema, len(collections))
	for _, c := range collections {
		var schema models.CollectionSchema
		if err := json.Unmarshal([]byte(c.Schema), &schema); err == nil {
			m[c.ID] = schema
		}
	}
	return m, nil
}

// extractStatusFromFields extracts the "status" value from an item's fields JSON.
func extractStatusFromFields(fields string) string {
	if fields == "" || fields == "{}" {
		return ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(fields), &m); err != nil {
		return ""
	}
	status, _ := m["status"].(string)
	return status
}

// isTerminalWithSchema checks if a status is terminal using the collection's schema.
// Falls back to default terminal statuses if the collection is not in the schema map.
func isTerminalWithSchema(status, collectionID string, schemaMap map[string]models.CollectionSchema) bool {
	if status == "" {
		return false
	}
	if schema, ok := schemaMap[collectionID]; ok {
		return models.IsTerminalStatus(status, schema)
	}
	return models.IsTerminalStatusDefault(status)
}

// DeleteStarsForItem removes all stars for a given item (used when deleting an item).
func (s *Store) DeleteStarsForItem(itemID string) error {
	_, err := s.db.Exec(s.q("DELETE FROM item_stars WHERE item_id = ?"), itemID)
	if err != nil {
		return fmt.Errorf("delete stars for item: %w", err)
	}
	return nil
}

