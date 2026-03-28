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
		INSERT INTO activities (id, workspace_id, document_id, action, actor, source, metadata, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, a.ID, a.WorkspaceID, nilIfEmpty(a.DocumentID), a.Action, a.Actor, a.Source, a.Metadata, ts)
	return err
}

func (s *Store) ListWorkspaceActivity(workspaceID string, params models.ActivityListParams) ([]models.Activity, error) {
	query := `
		SELECT id, workspace_id, COALESCE(document_id, ''), action, actor, source, metadata, created_at
		FROM activities
		WHERE workspace_id = ?
	`
	args := []interface{}{workspaceID}

	if params.Action != "" {
		query += " AND action = ?"
		args = append(args, params.Action)
	}
	if params.Actor != "" {
		query += " AND actor = ?"
		args = append(args, params.Actor)
	}
	if params.Source != "" {
		query += " AND source = ?"
		args = append(args, params.Source)
	}

	query += " ORDER BY created_at DESC"

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

	return scanActivities(rows)
}

func (s *Store) ListDocumentActivity(documentID string, params models.ActivityListParams) ([]models.Activity, error) {
	query := `
		SELECT id, workspace_id, COALESCE(document_id, ''), action, actor, source, metadata, created_at
		FROM activities
		WHERE document_id = ?
	`
	args := []interface{}{documentID}

	if params.Action != "" {
		query += " AND action = ?"
		args = append(args, params.Action)
	}
	if params.Actor != "" {
		query += " AND actor = ?"
		args = append(args, params.Actor)
	}

	query += " ORDER BY created_at DESC"

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

	return scanActivities(rows)
}

func scanActivities(rows interface {
	Next() bool
	Scan(dest ...interface{}) error
	Err() error
}) ([]models.Activity, error) {
	var activities []models.Activity
	for rows.Next() {
		var a models.Activity
		var createdAt string
		if err := rows.Scan(&a.ID, &a.WorkspaceID, &a.DocumentID, &a.Action, &a.Actor, &a.Source, &a.Metadata, &createdAt); err != nil {
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
