package store

import (
	"database/sql"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// CreateWatch creates a durable watch, or replaces the predicate on an
// existing one for the same (user, item) pair (TASK-2533). Re-running
// `pad watch <ref>` on an already-watched item is an upsert, not a
// duplicate subscription — uq_watches_user_item (migrations 078/056)
// enforces one watch per (user, item) at the DB level; this mirrors that
// with an explicit ON CONFLICT so a repeat call updates the predicate in
// place instead of erroring.
func (s *Store) CreateWatch(workspaceID, userID, itemID, predicate string) (*models.Watch, error) {
	id := newID()
	ts := now()
	_, err := s.db.Exec(s.q(`
		INSERT INTO watches (id, workspace_id, user_id, item_id, predicate, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, item_id) DO UPDATE SET predicate = excluded.predicate, created_at = excluded.created_at
	`), id, workspaceID, userID, itemID, predicate, ts)
	if err != nil {
		return nil, fmt.Errorf("create watch: %w", err)
	}
	return s.GetWatchByUserItem(userID, itemID)
}

// GetWatchByUserItem returns the (at most one) watch a user holds on a
// specific item, or (nil, nil) if none exists.
func (s *Store) GetWatchByUserItem(userID, itemID string) (*models.Watch, error) {
	var w models.Watch
	var predicate sql.NullString
	var createdAt string
	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, user_id, item_id, predicate, created_at
		FROM watches WHERE user_id = ? AND item_id = ?
	`), userID, itemID).Scan(&w.ID, &w.WorkspaceID, &w.UserID, &w.ItemID, &predicate, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get watch: %w", err)
	}
	w.Predicate = predicate.String
	w.CreatedAt = parseTime(createdAt)
	return &w, nil
}

// ListWatchesForUser returns every watch a user holds, across ALL
// workspaces they're a member of, newest first — enriched with the
// watched item's ref/title/slug/collection ID and its workspace's slug
// so both `pad watch list` and the event-stream handler's per-connection
// watch map can use it directly without a second round trip per row.
//
// Deliberately unscoped by workspace (unlike most list methods in this
// file): a watch is a personal subscription, not a workspace resource,
// and the event-stream handler is inherently cross-workspace — it needs
// every watch a caller holds to filter one global notification stream.
//
// IMPORTANT: this does NOT re-check the caller's CURRENT access to the
// watched item — it only reflects the watches table, which durably
// outlives a workspace membership or grant revocation. Callers MUST run
// the result through server.filterWatchesByCurrentAccess (or an
// equivalent live-access check) before using it to gate delivery or a
// listing response; not doing so leaks item/workspace metadata for
// access the caller no longer has (TASK-2533, codex round 1 finding 1).
func (s *Store) ListWatchesForUser(userID string) ([]models.Watch, error) {
	rows, err := s.db.Query(s.q(`
		SELECT w.id, w.workspace_id, w.user_id, w.item_id, w.predicate, w.created_at,
		       i.title, i.slug, i.collection_id, i.item_number, c.prefix, ws.slug
		FROM watches w
		JOIN items i ON i.id = w.item_id
		JOIN collections c ON c.id = i.collection_id
		JOIN workspaces ws ON ws.id = w.workspace_id
		WHERE w.user_id = ?
		ORDER BY w.created_at DESC
	`), userID)
	if err != nil {
		return nil, fmt.Errorf("list watches: %w", err)
	}
	defer rows.Close()

	var out []models.Watch
	for rows.Next() {
		var w models.Watch
		var predicate sql.NullString
		var createdAt string
		var itemNumber sql.NullInt64
		var prefix string
		if err := rows.Scan(&w.ID, &w.WorkspaceID, &w.UserID, &w.ItemID, &predicate, &createdAt,
			&w.ItemTitle, &w.ItemSlug, &w.ItemCollectionID, &itemNumber, &prefix, &w.WorkspaceSlug); err != nil {
			return nil, fmt.Errorf("scan watch: %w", err)
		}
		w.Predicate = predicate.String
		w.CreatedAt = parseTime(createdAt)
		if prefix != "" && itemNumber.Valid {
			w.ItemRef = fmt.Sprintf("%s-%d", prefix, itemNumber.Int64)
		}
		out = append(out, w)
	}
	if out == nil {
		out = []models.Watch{}
	}
	return out, rows.Err()
}

// DeleteWatch removes a user's watch on an item. Returns sql.ErrNoRows if
// no such watch exists — mirrors UnstarItem's contract.
func (s *Store) DeleteWatch(userID, itemID string) error {
	result, err := s.db.Exec(s.q(`DELETE FROM watches WHERE user_id = ? AND item_id = ?`), userID, itemID)
	if err != nil {
		return fmt.Errorf("delete watch: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
