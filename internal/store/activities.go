package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xarmian/pad/internal/models"
)

// ActivityDebounceCooldown is the window during which repeated "updated" actions
// on the same item by the same user are coalesced into a single activity entry.
const ActivityDebounceCooldown = 5 * time.Minute

func (s *Store) CreateActivity(a models.Activity) (string, error) {
	a.ID = newID()
	if a.Metadata == "" {
		a.Metadata = "{}"
	}
	ts := now()

	_, err := s.db.Exec(`
		INSERT INTO activities (id, workspace_id, document_id, action, actor, source, metadata, user_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, a.ID, a.WorkspaceID, nilIfEmpty(a.DocumentID), a.Action, a.Actor, a.Source, a.Metadata, nilIfEmpty(a.UserID), ts)
	return a.ID, err
}

// CreateActivityDebounced creates a new activity or updates an existing one if a
// matching activity (same document, same user, same action) was recorded within
// the cooldown window. Only "updated" actions are debounced; all other actions
// always create a new entry.
//
// When merging, the existing activity's timestamp is bumped to now and its
// metadata changes are accumulated (so a rapid sequence of field edits produces
// a single activity whose metadata lists all changed fields).
func (s *Store) CreateActivityDebounced(a models.Activity) (string, error) {
	if a.Metadata == "" {
		a.Metadata = "{}"
	}

	// Only debounce "updated" actions — everything else always creates a new row.
	if a.Action != "updated" {
		return s.CreateActivity(a)
	}

	ts := now()
	cutoff := time.Now().UTC().Add(-ActivityDebounceCooldown).Format(time.RFC3339)

	// Look for a recent activity to coalesce with.
	var existingID, existingMeta string
	err := s.db.QueryRow(`
		SELECT id, metadata FROM activities
		WHERE document_id = ? AND action = ? AND created_at >= ?
			AND ((user_id IS NOT NULL AND user_id = ?) OR (user_id IS NULL AND ? = ''))
		ORDER BY created_at DESC LIMIT 1
	`, a.DocumentID, a.Action, cutoff, a.UserID, a.UserID).Scan(&existingID, &existingMeta)

	if err == sql.ErrNoRows {
		// No recent match — create a new activity.
		return s.CreateActivity(a)
	}
	if err != nil {
		// Query error — fall back to creating a new activity.
		return s.CreateActivity(a)
	}

	// Merge metadata: accumulate "changes" strings from both old and new.
	merged := mergeActivityMeta(existingMeta, a.Metadata)

	_, err = s.db.Exec(`
		UPDATE activities SET metadata = ?, created_at = ? WHERE id = ?
	`, merged, ts, existingID)
	return existingID, err
}

// mergeActivityMeta combines two activity metadata JSON strings, accumulating
// the "changes" field. If both contain changes, they are joined with "; ".
// Other metadata fields (like "agent") are preserved from both sides.
func mergeActivityMeta(existing, incoming string) string {
	var existMap, newMap map[string]interface{}
	if err := json.Unmarshal([]byte(existing), &existMap); err != nil {
		existMap = make(map[string]interface{})
	}
	if err := json.Unmarshal([]byte(incoming), &newMap); err != nil {
		newMap = make(map[string]interface{})
	}

	// Accumulate changes
	existChanges, _ := existMap["changes"].(string)
	newChanges, _ := newMap["changes"].(string)

	// Start with existing map as base, overlay new fields
	for k, v := range newMap {
		if k == "changes" {
			continue // handled separately
		}
		existMap[k] = v
	}

	if newChanges != "" {
		if existChanges != "" {
			existMap["changes"] = existChanges + "; " + newChanges
		} else {
			existMap["changes"] = newChanges
		}
	}
	// If neither had changes, don't add an empty one.
	// If only existing had changes, it's already in existMap.

	result, err := json.Marshal(existMap)
	if err != nil {
		return existing
	}
	return string(result)
}

func (s *Store) ListWorkspaceActivity(workspaceID string, params models.ActivityListParams) ([]models.Activity, error) {
	query := `
		SELECT a.id, a.workspace_id, COALESCE(a.document_id, ''), a.action, a.actor, a.source, a.metadata, COALESCE(a.user_id, ''), a.created_at, COALESCE(u.name, '')
		FROM activities a
		LEFT JOIN users u ON a.user_id = u.id
		WHERE a.workspace_id = ?
	`
	args := []interface{}{workspaceID}

	if params.Action != "" {
		query += " AND a.action = ?"
		args = append(args, params.Action)
	}
	if params.Actor != "" {
		query += " AND a.actor = ?"
		args = append(args, params.Actor)
	}
	if params.Source != "" {
		query += " AND a.source = ?"
		args = append(args, params.Source)
	}

	query += " ORDER BY a.created_at DESC"

	limit := params.Limit
	if limit <= 0 {
		limit = 20
	}
	query += fmt.Sprintf(" LIMIT %d", limit)
	if params.Offset > 0 {
		query += fmt.Sprintf(" OFFSET %d", params.Offset)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanActivitiesWithUser(rows)
}

func (s *Store) ListDocumentActivity(documentID string, params models.ActivityListParams) ([]models.Activity, error) {
	query := `
		SELECT a.id, a.workspace_id, COALESCE(a.document_id, ''), a.action, a.actor, a.source, a.metadata, COALESCE(a.user_id, ''), a.created_at, COALESCE(u.name, '')
		FROM activities a
		LEFT JOIN users u ON a.user_id = u.id
		WHERE a.document_id = ?
	`
	args := []interface{}{documentID}

	if params.Action != "" {
		query += " AND a.action = ?"
		args = append(args, params.Action)
	}
	if params.Actor != "" {
		query += " AND a.actor = ?"
		args = append(args, params.Actor)
	}

	query += " ORDER BY a.created_at DESC"

	limit := params.Limit
	if limit <= 0 {
		limit = 20
	}
	query += fmt.Sprintf(" LIMIT %d", limit)
	if params.Offset > 0 {
		query += fmt.Sprintf(" OFFSET %d", params.Offset)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanActivitiesWithUser(rows)
}

// ListDocumentActivityBeforeTime returns activities for a document created before the given time,
// ordered newest-first, limited to `limit` results. Used for cursor-based timeline pagination.
func (s *Store) ListDocumentActivityBeforeTime(documentID string, before time.Time, limit int) ([]models.Activity, error) {
	rows, err := s.db.Query(`
		SELECT a.id, a.workspace_id, COALESCE(a.document_id, ''), a.action, a.actor, a.source, a.metadata, COALESCE(a.user_id, ''), a.created_at, COALESCE(u.name, '')
		FROM activities a
		LEFT JOIN users u ON a.user_id = u.id
		WHERE a.document_id = ? AND a.created_at < ?
		ORDER BY a.created_at DESC
		LIMIT ?
	`, documentID, before.Format(time.RFC3339), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanActivitiesWithUser(rows)
}

func scanActivitiesWithUser(rows interface {
	Next() bool
	Scan(dest ...interface{}) error
	Err() error
}) ([]models.Activity, error) {
	var activities []models.Activity
	for rows.Next() {
		var a models.Activity
		var createdAt string
		if err := rows.Scan(&a.ID, &a.WorkspaceID, &a.DocumentID, &a.Action, &a.Actor, &a.Source, &a.Metadata, &a.UserID, &createdAt, &a.ActorName); err != nil {
			return nil, err
		}
		a.CreatedAt = parseTime(createdAt)
		activities = append(activities, a)
	}
	return activities, rows.Err()
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
