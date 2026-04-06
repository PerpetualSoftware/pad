package store

import (
	"database/sql"
	"fmt"

	"github.com/xarmian/pad/internal/models"
)

// CreateView adds a new saved view.
func (s *Store) CreateView(workspaceID string, input models.ViewCreate) (*models.View, error) {
	id := newID()
	ts := now()

	slug := input.Slug
	if slug == "" {
		slug = slugify(input.Name)
	}
	slug, err := s.uniqueSlug("views", "workspace_id", workspaceID, slug)
	if err != nil {
		return nil, fmt.Errorf("generate slug: %w", err)
	}

	config := input.Config
	if config == "" {
		config = "{}"
	}

	viewType := input.ViewType
	if viewType == "" {
		viewType = "list"
	}

	_, err = s.db.Exec(s.q(`
		INSERT INTO views (id, workspace_id, collection_id, name, slug, view_type, config, sort_order, is_default, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)`),
		id, workspaceID, input.CollectionID, input.Name, slug, viewType, config, s.dialect.BoolToInt(false), ts, ts,
	)
	if err != nil {
		return nil, fmt.Errorf("insert view: %w", err)
	}

	return s.GetView(id)
}

// GetView returns a single view by ID.
func (s *Store) GetView(id string) (*models.View, error) {
	var v models.View
	var collectionID *string
	var isDefault bool
	var createdAt, updatedAt string

	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, collection_id, name, slug, view_type, config, sort_order, is_default, created_at, updated_at
		FROM views
		WHERE id = ?`), id).Scan(
		&v.ID, &v.WorkspaceID, &collectionID, &v.Name, &v.Slug, &v.ViewType,
		&v.Config, &v.SortOrder, &isDefault, &createdAt, &updatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get view: %w", err)
	}

	v.CollectionID = collectionID
	v.IsDefault = isDefault
	v.CreatedAt = parseTime(createdAt)
	v.UpdatedAt = parseTime(updatedAt)
	return &v, nil
}

// GetViewBySlug returns a view by its workspace-scoped slug.
func (s *Store) GetViewBySlug(workspaceID, slug string) (*models.View, error) {
	var v models.View
	var collectionID *string
	var isDefault bool
	var createdAt, updatedAt string

	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, collection_id, name, slug, view_type, config, sort_order, is_default, created_at, updated_at
		FROM views
		WHERE workspace_id = ? AND slug = ?`), workspaceID, slug).Scan(
		&v.ID, &v.WorkspaceID, &collectionID, &v.Name, &v.Slug, &v.ViewType,
		&v.Config, &v.SortOrder, &isDefault, &createdAt, &updatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get view by slug: %w", err)
	}

	v.CollectionID = collectionID
	v.IsDefault = isDefault
	v.CreatedAt = parseTime(createdAt)
	v.UpdatedAt = parseTime(updatedAt)
	return &v, nil
}

// ListViews returns all views for a collection within a workspace.
func (s *Store) ListViews(workspaceID, collectionID string) ([]models.View, error) {
	rows, err := s.db.Query(s.q(`
		SELECT id, workspace_id, collection_id, name, slug, view_type, config, sort_order, is_default, created_at, updated_at
		FROM views
		WHERE workspace_id = ? AND collection_id = ?
		ORDER BY sort_order ASC, created_at ASC`), workspaceID, collectionID)
	if err != nil {
		return nil, fmt.Errorf("list views: %w", err)
	}
	defer rows.Close()

	var views []models.View
	for rows.Next() {
		var v models.View
		var collID *string
		var isDefault bool
		var createdAt, updatedAt string

		if err := rows.Scan(
			&v.ID, &v.WorkspaceID, &collID, &v.Name, &v.Slug, &v.ViewType,
			&v.Config, &v.SortOrder, &isDefault, &createdAt, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan view: %w", err)
		}
		v.CollectionID = collID
		v.IsDefault = isDefault
		v.CreatedAt = parseTime(createdAt)
		v.UpdatedAt = parseTime(updatedAt)
		views = append(views, v)
	}
	return views, rows.Err()
}

// UpdateView modifies an existing view.
func (s *Store) UpdateView(id string, input models.ViewUpdate) (*models.View, error) {
	ts := now()

	// Build dynamic SET clause
	sets := []string{"updated_at = ?"}
	args := []interface{}{ts}

	if input.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *input.Name)
	}
	if input.ViewType != nil {
		sets = append(sets, "view_type = ?")
		args = append(args, *input.ViewType)
	}
	if input.Config != nil {
		sets = append(sets, "config = ?")
		args = append(args, *input.Config)
	}
	if input.SortOrder != nil {
		sets = append(sets, "sort_order = ?")
		args = append(args, *input.SortOrder)
	}

	args = append(args, id)

	query := "UPDATE views SET "
	for i, s := range sets {
		if i > 0 {
			query += ", "
		}
		query += s
	}
	query += " WHERE id = ?"

	result, err := s.db.Exec(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("update view: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return nil, sql.ErrNoRows
	}

	return s.GetView(id)
}

// DeleteView removes a view by ID.
func (s *Store) DeleteView(id string) error {
	result, err := s.db.Exec(s.q("DELETE FROM views WHERE id = ?"), id)
	if err != nil {
		return fmt.Errorf("delete view: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
