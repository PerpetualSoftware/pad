package store

import (
	"database/sql"
	"fmt"

	"github.com/xarmian/pad/internal/models"
)

// AddWorkspaceMember adds a user to a workspace with the given role.
func (s *Store) AddWorkspaceMember(workspaceID, userID, role string) error {
	ts := now()
	_, err := s.db.Exec(`
		INSERT INTO workspace_members (workspace_id, user_id, role, created_at)
		VALUES (?, ?, ?, ?)
	`, workspaceID, userID, role, ts)
	if err != nil {
		return fmt.Errorf("add workspace member: %w", err)
	}
	return nil
}

// RemoveWorkspaceMember removes a user from a workspace.
func (s *Store) RemoveWorkspaceMember(workspaceID, userID string) error {
	result, err := s.db.Exec(
		"DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?",
		workspaceID, userID,
	)
	if err != nil {
		return fmt.Errorf("remove workspace member: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetWorkspaceMember retrieves a single membership record.
func (s *Store) GetWorkspaceMember(workspaceID, userID string) (*models.WorkspaceMember, error) {
	var m models.WorkspaceMember
	var createdAt string

	err := s.db.QueryRow(`
		SELECT workspace_id, user_id, role, created_at
		FROM workspace_members
		WHERE workspace_id = ? AND user_id = ?
	`, workspaceID, userID).Scan(
		&m.WorkspaceID, &m.UserID, &m.Role, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace member: %w", err)
	}

	m.CreatedAt = parseTime(createdAt)
	return &m, nil
}

// ListWorkspaceMembers returns all members of a workspace, enriched with
// user name and email from a join.
func (s *Store) ListWorkspaceMembers(workspaceID string) ([]models.WorkspaceMember, error) {
	rows, err := s.db.Query(`
		SELECT wm.workspace_id, wm.user_id, wm.role, wm.created_at,
		       u.name, u.email
		FROM workspace_members wm
		JOIN users u ON u.id = wm.user_id
		WHERE wm.workspace_id = ?
		ORDER BY wm.created_at ASC
	`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list workspace members: %w", err)
	}
	defer rows.Close()

	var result []models.WorkspaceMember
	for rows.Next() {
		var m models.WorkspaceMember
		var createdAt string
		if err := rows.Scan(
			&m.WorkspaceID, &m.UserID, &m.Role, &createdAt,
			&m.UserName, &m.UserEmail,
		); err != nil {
			return nil, fmt.Errorf("scan workspace member: %w", err)
		}
		m.CreatedAt = parseTime(createdAt)
		result = append(result, m)
	}
	return result, rows.Err()
}

// GetUserWorkspaces returns all workspaces a user has access to.
func (s *Store) GetUserWorkspaces(userID string) ([]models.Workspace, error) {
	rows, err := s.db.Query(`
		SELECT w.id, w.name, w.slug, w.description, w.settings, w.created_at, w.updated_at, w.deleted_at
		FROM workspaces w
		JOIN workspace_members wm ON wm.workspace_id = w.id
		WHERE wm.user_id = ? AND w.deleted_at IS NULL
		ORDER BY w.name ASC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("get user workspaces: %w", err)
	}
	defer rows.Close()

	var result []models.Workspace
	for rows.Next() {
		var ws models.Workspace
		var createdAt, updatedAt string
		var deletedAt *string
		if err := rows.Scan(
			&ws.ID, &ws.Name, &ws.Slug, &ws.Description, &ws.Settings,
			&createdAt, &updatedAt, &deletedAt,
		); err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		ws.CreatedAt = parseTime(createdAt)
		ws.UpdatedAt = parseTime(updatedAt)
		ws.DeletedAt = parseTimePtr(deletedAt)
		result = append(result, ws)
	}
	return result, rows.Err()
}

// IsWorkspaceMember checks if a user is a member of a workspace.
func (s *Store) IsWorkspaceMember(workspaceID, userID string) (bool, error) {
	var count int
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM workspace_members WHERE workspace_id = ? AND user_id = ?",
		workspaceID, userID,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check workspace membership: %w", err)
	}
	return count > 0, nil
}

// UpdateWorkspaceMemberRole changes a member's role in a workspace.
func (s *Store) UpdateWorkspaceMemberRole(workspaceID, userID, role string) error {
	result, err := s.db.Exec(
		"UPDATE workspace_members SET role = ? WHERE workspace_id = ? AND user_id = ?",
		role, workspaceID, userID,
	)
	if err != nil {
		return fmt.Errorf("update workspace member role: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
