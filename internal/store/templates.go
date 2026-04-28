package store

import (
	"database/sql"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func (s *Store) ListCustomTemplates(workspaceID string) ([]models.CustomTemplate, error) {
	rows, err := s.db.Query(s.q(`
		SELECT id, workspace_id, name, description, doc_type, icon, content, created_at, updated_at
		FROM custom_templates
		WHERE workspace_id = ?
		ORDER BY name ASC
	`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list custom templates: %w", err)
	}
	defer rows.Close()

	var templates []models.CustomTemplate
	for rows.Next() {
		var t models.CustomTemplate
		var createdAt, updatedAt string
		if err := rows.Scan(&t.ID, &t.WorkspaceID, &t.Name, &t.Description, &t.DocType, &t.Icon, &t.Content, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		t.CreatedAt = parseTime(createdAt)
		t.UpdatedAt = parseTime(updatedAt)
		templates = append(templates, t)
	}
	return templates, rows.Err()
}

func (s *Store) GetCustomTemplate(id string) (*models.CustomTemplate, error) {
	var t models.CustomTemplate
	var createdAt, updatedAt string
	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, name, description, doc_type, icon, content, created_at, updated_at
		FROM custom_templates
		WHERE id = ?
	`), id).Scan(&t.ID, &t.WorkspaceID, &t.Name, &t.Description, &t.DocType, &t.Icon, &t.Content, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = parseTime(createdAt)
	t.UpdatedAt = parseTime(updatedAt)
	return &t, nil
}

func (s *Store) CreateCustomTemplate(input models.CustomTemplateCreate) (*models.CustomTemplate, error) {
	id := newID()
	ts := now()

	_, err := s.db.Exec(s.q(`
		INSERT INTO custom_templates (id, workspace_id, name, description, doc_type, icon, content, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), id, input.WorkspaceID, input.Name, input.Description, input.DocType, input.Icon, input.Content, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("create custom template: %w", err)
	}

	return s.GetCustomTemplate(id)
}

func (s *Store) DeleteCustomTemplate(id string) error {
	result, err := s.db.Exec(s.q(`DELETE FROM custom_templates WHERE id = ?`), id)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}
