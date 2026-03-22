package store

import (
	"database/sql"
	"fmt"

	"github.com/xarmian/pad/internal/models"
)

func (s *Store) ListWorkspaces() ([]models.Workspace, error) {
	rows, err := s.db.Query(`
		SELECT id, name, slug, description, settings, created_at, updated_at
		FROM workspaces
		WHERE deleted_at IS NULL
		ORDER BY name ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workspaces []models.Workspace
	for rows.Next() {
		var w models.Workspace
		var createdAt, updatedAt string
		if err := rows.Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.Settings, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		w.CreatedAt = parseTime(createdAt)
		w.UpdatedAt = parseTime(updatedAt)
		workspaces = append(workspaces, w)
	}
	return workspaces, rows.Err()
}

func (s *Store) CreateWorkspace(input models.WorkspaceCreate) (*models.Workspace, error) {
	id := newID()
	ts := now()

	slug := input.Slug
	if slug == "" {
		slug = slugify(input.Name)
	}

	// Ensure unique slug
	finalSlug, err := s.uniqueSlug("workspaces", "id", "", slug)
	if err != nil {
		return nil, err
	}
	// For workspaces, slug is globally unique, so we need a different check
	finalSlug, err = s.uniqueWorkspaceSlug(slug)
	if err != nil {
		return nil, err
	}

	settings := input.Settings
	if settings == "" {
		settings = "{}"
	}

	_, err = s.db.Exec(`
		INSERT INTO workspaces (id, name, slug, description, settings, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, id, input.Name, finalSlug, input.Description, settings, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("insert workspace: %w", err)
	}

	return s.GetWorkspaceBySlug(finalSlug)
}

func (s *Store) uniqueWorkspaceSlug(baseSlug string) (string, error) {
	slug := baseSlug
	for i := 2; ; i++ {
		var count int
		err := s.db.QueryRow("SELECT COUNT(*) FROM workspaces WHERE slug = ? AND deleted_at IS NULL", slug).Scan(&count)
		if err != nil {
			return "", err
		}
		if count == 0 {
			return slug, nil
		}
		slug = fmt.Sprintf("%s-%d", baseSlug, i)
	}
}

func (s *Store) GetWorkspaceBySlug(slug string) (*models.Workspace, error) {
	var w models.Workspace
	var createdAt, updatedAt string
	var deletedAt *string

	err := s.db.QueryRow(`
		SELECT id, name, slug, description, settings, created_at, updated_at, deleted_at
		FROM workspaces
		WHERE slug = ? AND deleted_at IS NULL
	`, slug).Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.Settings, &createdAt, &updatedAt, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	w.CreatedAt = parseTime(createdAt)
	w.UpdatedAt = parseTime(updatedAt)
	w.DeletedAt = parseTimePtr(deletedAt)
	return &w, nil
}

func (s *Store) GetWorkspaceByID(id string) (*models.Workspace, error) {
	var w models.Workspace
	var createdAt, updatedAt string
	var deletedAt *string

	err := s.db.QueryRow(`
		SELECT id, name, slug, description, settings, created_at, updated_at, deleted_at
		FROM workspaces
		WHERE id = ? AND deleted_at IS NULL
	`, id).Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.Settings, &createdAt, &updatedAt, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	w.CreatedAt = parseTime(createdAt)
	w.UpdatedAt = parseTime(updatedAt)
	w.DeletedAt = parseTimePtr(deletedAt)
	return &w, nil
}

func (s *Store) UpdateWorkspace(slug string, input models.WorkspaceUpdate) (*models.Workspace, error) {
	w, err := s.GetWorkspaceBySlug(slug)
	if err != nil {
		return nil, err
	}
	if w == nil {
		return nil, nil
	}

	ts := now()

	if input.Name != nil {
		w.Name = *input.Name
	}
	if input.Description != nil {
		w.Description = *input.Description
	}
	if input.Settings != nil {
		w.Settings = *input.Settings
	}

	_, err = s.db.Exec(`
		UPDATE workspaces SET name = ?, description = ?, settings = ?, updated_at = ?
		WHERE id = ?
	`, w.Name, w.Description, w.Settings, ts, w.ID)
	if err != nil {
		return nil, err
	}

	return s.GetWorkspaceBySlug(slug)
}

func (s *Store) DeleteWorkspace(slug string) error {
	ts := now()
	result, err := s.db.Exec(`
		UPDATE workspaces SET deleted_at = ?, updated_at = ?
		WHERE slug = ? AND deleted_at IS NULL
	`, ts, ts, slug)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}
