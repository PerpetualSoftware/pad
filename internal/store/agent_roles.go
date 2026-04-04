package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/xarmian/pad/internal/models"
)

func (s *Store) CreateAgentRole(workspaceID string, input models.AgentRoleCreate) (*models.AgentRole, error) {
	id := newID()
	ts := now()

	slug := input.Slug
	if slug == "" {
		slug = slugify(input.Name)
	}
	if slug == "" {
		slug = "role"
	}
	slug, err := s.uniqueSlug("agent_roles", "workspace_id", workspaceID, slug)
	if err != nil {
		return nil, fmt.Errorf("unique slug: %w", err)
	}

	_, err = s.db.Exec(`
		INSERT INTO agent_roles (id, workspace_id, slug, name, description, icon, sort_order, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)
	`, id, workspaceID, slug, input.Name, input.Description, input.Icon, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("create agent role: %w", err)
	}

	return s.GetAgentRole(workspaceID, id)
}

func (s *Store) GetAgentRole(workspaceID, idOrSlug string) (*models.AgentRole, error) {
	var role models.AgentRole
	var createdAt, updatedAt string

	err := s.db.QueryRow(`
		SELECT id, workspace_id, slug, name, description, icon, sort_order, created_at, updated_at
		FROM agent_roles
		WHERE workspace_id = ? AND (id = ? OR slug = ?)
	`, workspaceID, idOrSlug, idOrSlug).Scan(
		&role.ID, &role.WorkspaceID, &role.Slug, &role.Name, &role.Description,
		&role.Icon, &role.SortOrder, &createdAt, &updatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get agent role: %w", err)
	}

	role.CreatedAt = parseTime(createdAt)
	role.UpdatedAt = parseTime(updatedAt)
	return &role, nil
}

func (s *Store) ListAgentRoles(workspaceID string) ([]models.AgentRole, error) {
	rows, err := s.db.Query(`
		SELECT r.id, r.workspace_id, r.slug, r.name, r.description, r.icon, r.sort_order,
		       r.created_at, r.updated_at,
		       COUNT(i.id) as item_count
		FROM agent_roles r
		LEFT JOIN items i ON i.agent_role_id = r.id AND i.deleted_at IS NULL
		WHERE r.workspace_id = ?
		GROUP BY r.id
		ORDER BY r.sort_order ASC, r.name ASC
	`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list agent roles: %w", err)
	}
	defer rows.Close()

	var roles []models.AgentRole
	for rows.Next() {
		var role models.AgentRole
		var createdAt, updatedAt string
		if err := rows.Scan(
			&role.ID, &role.WorkspaceID, &role.Slug, &role.Name, &role.Description,
			&role.Icon, &role.SortOrder, &createdAt, &updatedAt, &role.ItemCount,
		); err != nil {
			return nil, err
		}
		role.CreatedAt = parseTime(createdAt)
		role.UpdatedAt = parseTime(updatedAt)
		roles = append(roles, role)
	}
	if roles == nil {
		roles = []models.AgentRole{}
	}
	return roles, rows.Err()
}

func (s *Store) UpdateAgentRole(workspaceID, id string, input models.AgentRoleUpdate) (*models.AgentRole, error) {
	existing, err := s.GetAgentRole(workspaceID, id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}

	ts := now()
	sets := []string{"updated_at = ?"}
	args := []interface{}{ts}

	if input.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *input.Name)
	}
	if input.Slug != nil {
		sets = append(sets, "slug = ?")
		args = append(args, *input.Slug)
	}
	if input.Description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *input.Description)
	}
	if input.Icon != nil {
		sets = append(sets, "icon = ?")
		args = append(args, *input.Icon)
	}
	if input.SortOrder != nil {
		sets = append(sets, "sort_order = ?")
		args = append(args, *input.SortOrder)
	}

	args = append(args, existing.ID)
	query := fmt.Sprintf("UPDATE agent_roles SET %s WHERE id = ?", strings.Join(sets, ", "))
	_, err = s.db.Exec(query, args...)
	if err != nil {
		return nil, fmt.Errorf("update agent role: %w", err)
	}

	return s.GetAgentRole(workspaceID, existing.ID)
}

func (s *Store) DeleteAgentRole(workspaceID, id string) error {
	result, err := s.db.Exec(`
		DELETE FROM agent_roles WHERE workspace_id = ? AND (id = ? OR slug = ?)
	`, workspaceID, id, id)
	if err != nil {
		return fmt.Errorf("delete agent role: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ResolveAgentRoleID resolves a role identifier (ID or slug) to its UUID.
// Returns empty string if the role doesn't exist.
func (s *Store) ResolveAgentRoleID(workspaceID, idOrSlug string) (string, error) {
	role, err := s.GetAgentRole(workspaceID, idOrSlug)
	if err != nil {
		return "", err
	}
	if role == nil {
		return "", nil
	}
	return role.ID, nil
}

