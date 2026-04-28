package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
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
		// Build a (schema + settings) context per collection so terminal
		// checks honor each collection's configured done field.
		ctxMap, err := s.buildCollectionDoneContextMap(workspaceID)
		if err != nil {
			return nil, fmt.Errorf("list starred items: build done-context map: %w", err)
		}

		filtered := make([]models.Item, 0, len(items))
		for _, item := range items {
			if !isTerminalWithContext(item.Fields, item.CollectionID, ctxMap) {
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

// collectionDoneContext pairs a collection's parsed schema with its
// parsed settings so terminal-state checks can honor the configured
// done field (board_group_by) — not just the hardcoded `status` column.
type collectionDoneContext struct {
	schema   models.CollectionSchema
	settings models.CollectionSettings
}

// buildCollectionDoneContextMap loads schema + settings for every
// collection in a workspace. Used to evaluate "is terminal?" for a
// batch of items from potentially different collections.
func (s *Store) buildCollectionDoneContextMap(workspaceID string) (map[string]collectionDoneContext, error) {
	rows, err := s.db.Query(
		s.q("SELECT id, schema, settings FROM collections WHERE workspace_id = ?"),
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("load collection done-context: %w", err)
	}
	defer rows.Close()

	m := make(map[string]collectionDoneContext)
	for rows.Next() {
		var id, rawSchema string
		var rawSettings sql.NullString
		if err := rows.Scan(&id, &rawSchema, &rawSettings); err != nil {
			return nil, err
		}
		var ctx collectionDoneContext
		if err := json.Unmarshal([]byte(rawSchema), &ctx.schema); err != nil {
			// Don't drop the entry entirely — we still want an empty
			// context so the membership test falls back to defaults
			// rather than silently treating the item as non-terminal.
			ctx.schema = models.CollectionSchema{}
		}
		if rawSettings.Valid && rawSettings.String != "" {
			_ = json.Unmarshal([]byte(rawSettings.String), &ctx.settings)
		}
		m[id] = ctx
	}
	return m, rows.Err()
}

// isTerminalWithContext reports whether an item is in a terminal state
// given its collection's done-context. When the collection isn't in the
// map (e.g. deleted collection), falls back to default-terminal checks
// against the item's `status` field.
func isTerminalWithContext(
	fields string,
	collectionID string,
	ctxMap map[string]collectionDoneContext,
) bool {
	if fields == "" || fields == "{}" {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(fields), &m); err != nil {
		return false
	}
	if ctx, ok := ctxMap[collectionID]; ok {
		return models.IsTerminalItem(m, ctx.schema, ctx.settings)
	}
	// No collection context available → check the status field against
	// the global default list. Matches the legacy fallback behavior.
	status, _ := m["status"].(string)
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
