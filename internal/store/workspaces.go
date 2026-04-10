package store

import (
	"database/sql"
	"fmt"

	"github.com/xarmian/pad/internal/models"
)

func (s *Store) ListWorkspaces() ([]models.Workspace, error) {
	return s.ListWorkspacesForUser("")
}

// ListWorkspacesForUser returns all workspaces (admin view), with per-user
// sort_order from workspace_members when userID is provided.
func (s *Store) ListWorkspacesForUser(userID string) ([]models.Workspace, error) {
	var rows *sql.Rows
	var err error

	if userID != "" {
		rows, err = s.db.Query(s.q(`
			SELECT w.id, w.name, w.slug, w.owner_id, w.description, w.settings, w.created_at, w.updated_at,
			       COALESCE(wm.sort_order, 0)
			FROM workspaces w
			LEFT JOIN workspace_members wm ON wm.workspace_id = w.id AND wm.user_id = ?
			WHERE w.deleted_at IS NULL
			ORDER BY COALESCE(wm.sort_order, 0) ASC, w.name ASC
		`), userID)
	} else {
		rows, err = s.db.Query(s.q(`
			SELECT w.id, w.name, w.slug, w.owner_id, w.description, w.settings, w.created_at, w.updated_at,
			       0 as sort_order
			FROM workspaces w
			WHERE w.deleted_at IS NULL
			ORDER BY w.name ASC
		`))
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workspaces []models.Workspace
	for rows.Next() {
		var w models.Workspace
		var createdAt, updatedAt string
		if err := rows.Scan(&w.ID, &w.Name, &w.Slug, &w.OwnerID, &w.Description, &w.Settings, &createdAt, &updatedAt, &w.SortOrder); err != nil {
			return nil, err
		}
		w.CreatedAt = parseTime(createdAt)
		w.UpdatedAt = parseTime(updatedAt)
		w.HydrateDerivedFields()
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
	settings, err = models.NormalizeWorkspaceSettings(settings)
	if err != nil {
		return nil, fmt.Errorf("normalize workspace settings: %w", err)
	}
	if input.Context != nil {
		settings, err = models.ApplyWorkspaceContext(settings, input.Context)
		if err != nil {
			return nil, fmt.Errorf("apply workspace context: %w", err)
		}
	}

	_, err = s.db.Exec(s.q(`
		INSERT INTO workspaces (id, name, slug, owner_id, description, settings, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`), id, input.Name, finalSlug, input.OwnerID, input.Description, settings, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("insert workspace: %w", err)
	}

	return s.GetWorkspaceBySlug(finalSlug)
}

func (s *Store) uniqueWorkspaceSlug(baseSlug string) (string, error) {
	slug := baseSlug
	for i := 2; ; i++ {
		var count int
		err := s.db.QueryRow(s.q("SELECT COUNT(*) FROM workspaces WHERE slug = ? AND deleted_at IS NULL"), slug).Scan(&count)
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

	err := s.db.QueryRow(s.q(`
		SELECT id, name, slug, owner_id, description, settings, created_at, updated_at, deleted_at
		FROM workspaces
		WHERE slug = ? AND deleted_at IS NULL
	`), slug).Scan(&w.ID, &w.Name, &w.Slug, &w.OwnerID, &w.Description, &w.Settings, &createdAt, &updatedAt, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	w.CreatedAt = parseTime(createdAt)
	w.UpdatedAt = parseTime(updatedAt)
	w.DeletedAt = parseTimePtr(deletedAt)
	w.HydrateDerivedFields()
	return &w, nil
}

func (s *Store) GetWorkspaceByID(id string) (*models.Workspace, error) {
	var w models.Workspace
	var createdAt, updatedAt string
	var deletedAt *string

	err := s.db.QueryRow(s.q(`
		SELECT id, name, slug, owner_id, description, settings, created_at, updated_at, deleted_at
		FROM workspaces
		WHERE id = ? AND deleted_at IS NULL
	`), id).Scan(&w.ID, &w.Name, &w.Slug, &w.OwnerID, &w.Description, &w.Settings, &createdAt, &updatedAt, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	w.CreatedAt = parseTime(createdAt)
	w.UpdatedAt = parseTime(updatedAt)
	w.DeletedAt = parseTimePtr(deletedAt)
	w.HydrateDerivedFields()
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
	if input.Context != nil {
		settings, err := models.ApplyWorkspaceContext(w.Settings, input.Context)
		if err != nil {
			return nil, fmt.Errorf("apply workspace context: %w", err)
		}
		w.Settings = settings
	}
	if w.Settings != "" {
		settings, err := models.NormalizeWorkspaceSettings(w.Settings)
		if err != nil {
			return nil, fmt.Errorf("normalize workspace settings: %w", err)
		}
		w.Settings = settings
	}

	_, err = s.db.Exec(s.q(`
		UPDATE workspaces SET name = ?, description = ?, settings = ?, updated_at = ?
		WHERE id = ?
	`), w.Name, w.Description, w.Settings, ts, w.ID)
	if err != nil {
		return nil, err
	}

	return s.GetWorkspaceBySlug(slug)
}

func (s *Store) DeleteWorkspace(slug string) error {
	ts := now()
	result, err := s.db.Exec(s.q(`
		UPDATE workspaces SET deleted_at = ?, updated_at = ?
		WHERE slug = ? AND deleted_at IS NULL
	`), ts, ts, slug)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}
