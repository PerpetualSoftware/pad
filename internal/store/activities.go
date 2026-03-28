package store

import (
	"fmt"

	"github.com/xarmian/pad/internal/models"
)

func (s *Store) CreateActivity(a models.Activity) error {
	a.ID = newID()
	if a.Metadata == "" {
		a.Metadata = "{}"
	}
	ts := now()

	_, err := s.db.Exec(`
		INSERT INTO activities (id, workspace_id, document_id, action, actor, source, metadata, user_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, a.ID, a.WorkspaceID, nilIfEmpty(a.DocumentID), a.Action, a.Actor, a.Source, a.Metadata, nilIfEmpty(a.UserID), ts)
	return err
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
