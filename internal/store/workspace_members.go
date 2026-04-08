package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/xarmian/pad/internal/models"
)

// AddWorkspaceMember adds a user to a workspace with the given role.
func (s *Store) AddWorkspaceMember(workspaceID, userID, role string) error {
	ts := now()
	_, err := s.db.Exec(s.q(`
		INSERT INTO workspace_members (workspace_id, user_id, role, created_at)
		VALUES (?, ?, ?, ?)
	`), workspaceID, userID, role, ts)
	if err != nil {
		return fmt.Errorf("add workspace member: %w", err)
	}
	return nil
}

// RemoveWorkspaceMember removes a user from a workspace.
func (s *Store) RemoveWorkspaceMember(workspaceID, userID string) error {
	result, err := s.db.Exec(
		s.q("DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?"),
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

	err := s.db.QueryRow(s.q(`
		SELECT workspace_id, user_id, role, created_at
		FROM workspace_members
		WHERE workspace_id = ? AND user_id = ?
	`), workspaceID, userID).Scan(
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
	rows, err := s.db.Query(s.q(`
		SELECT wm.workspace_id, wm.user_id, wm.role, wm.created_at,
		       u.name, u.email
		FROM workspace_members wm
		JOIN users u ON u.id = wm.user_id
		WHERE wm.workspace_id = ?
		ORDER BY wm.created_at ASC
	`), workspaceID)
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
	rows, err := s.db.Query(s.q(`
		SELECT w.id, w.name, w.slug, w.description, w.settings, w.created_at, w.updated_at, w.deleted_at
		FROM workspaces w
		JOIN workspace_members wm ON wm.workspace_id = w.id
		WHERE wm.user_id = ? AND w.deleted_at IS NULL
		ORDER BY w.name ASC
	`), userID)
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
		s.q("SELECT COUNT(*) FROM workspace_members WHERE workspace_id = ? AND user_id = ?"),
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
		s.q("UPDATE workspace_members SET role = ? WHERE workspace_id = ? AND user_id = ?"),
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

// --- Invitations ---

// CreateInvitation creates a pending workspace invitation.
// Generates a 128-bit (16-byte) random code and stores only its SHA-256 hash.
// The plaintext code is returned once to be shared with the invitee.
func (s *Store) CreateInvitation(workspaceID, email, role, invitedBy string) (*models.WorkspaceInvitation, error) {
	// Generate a random 128-bit join code (32 hex chars)
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate invitation code: %w", err)
	}
	code := hex.EncodeToString(raw)

	// Store SHA-256 hash of the code (plaintext never stored)
	hash := sha256.Sum256([]byte(code))
	codeHash := hex.EncodeToString(hash[:])

	id := newID()
	ts := now()

	_, err := s.db.Exec(s.q(`
		INSERT INTO workspace_invitations (id, workspace_id, email, role, invited_by, code, code_hash, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`), id, workspaceID, strings.ToLower(strings.TrimSpace(email)), role, invitedBy, id, codeHash, ts)
	if err != nil {
		return nil, fmt.Errorf("insert invitation: %w", err)
	}

	inv, err := s.GetInvitation(id)
	if err != nil {
		return nil, err
	}
	// Return the plaintext code to the caller (not stored in DB)
	inv.Code = code
	return inv, nil
}

// GetInvitation retrieves an invitation by ID.
func (s *Store) GetInvitation(id string) (*models.WorkspaceInvitation, error) {
	var inv models.WorkspaceInvitation
	var acceptedAt *string
	var createdAt string

	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, email, role, invited_by, code, accepted_at, created_at
		FROM workspace_invitations WHERE id = ?
	`), id).Scan(
		&inv.ID, &inv.WorkspaceID, &inv.Email, &inv.Role, &inv.InvitedBy,
		&inv.Code, &acceptedAt, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get invitation: %w", err)
	}

	inv.CreatedAt = parseTime(createdAt)
	inv.AcceptedAt = parseTimePtr(acceptedAt)
	return &inv, nil
}

// GetInvitationByCode retrieves a pending invitation by its join code.
// Looks up by SHA-256 hash first (new invitations), then falls back to
// plaintext lookup for legacy invitations created before hashing.
func (s *Store) GetInvitationByCode(code string) (*models.WorkspaceInvitation, error) {
	// Hash the provided code for lookup
	hash := sha256.Sum256([]byte(code))
	codeHash := hex.EncodeToString(hash[:])

	var inv models.WorkspaceInvitation
	var acceptedAt *string
	var createdAt string

	// Try hashed lookup first (new invitations)
	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, email, role, invited_by, code, accepted_at, created_at
		FROM workspace_invitations WHERE code_hash = ? AND accepted_at IS NULL
	`), codeHash).Scan(
		&inv.ID, &inv.WorkspaceID, &inv.Email, &inv.Role, &inv.InvitedBy,
		&inv.Code, &acceptedAt, &createdAt,
	)
	if err == nil {
		inv.CreatedAt = parseTime(createdAt)
		inv.AcceptedAt = parseTimePtr(acceptedAt)
		return &inv, nil
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("get invitation by code hash: %w", err)
	}

	// Fall back to plaintext lookup (legacy invitations)
	err = s.db.QueryRow(s.q(`
		SELECT id, workspace_id, email, role, invited_by, code, accepted_at, created_at
		FROM workspace_invitations WHERE code = ? AND accepted_at IS NULL
	`), code).Scan(
		&inv.ID, &inv.WorkspaceID, &inv.Email, &inv.Role, &inv.InvitedBy,
		&inv.Code, &acceptedAt, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get invitation by code: %w", err)
	}

	inv.CreatedAt = parseTime(createdAt)
	inv.AcceptedAt = parseTimePtr(acceptedAt)
	return &inv, nil
}

// AcceptInvitation marks an invitation as accepted.
func (s *Store) AcceptInvitation(id string) error {
	_, err := s.db.Exec(
		s.q("UPDATE workspace_invitations SET accepted_at = ? WHERE id = ?"),
		now(), id,
	)
	if err != nil {
		return fmt.Errorf("accept invitation: %w", err)
	}
	return nil
}

// DeleteInvitation removes a pending invitation.
func (s *Store) DeleteInvitation(workspaceID, invitationID string) error {
	result, err := s.db.Exec(
		s.q("DELETE FROM workspace_invitations WHERE id = ? AND workspace_id = ? AND accepted_at IS NULL"),
		invitationID, workspaceID,
	)
	if err != nil {
		return fmt.Errorf("delete invitation: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListWorkspaceInvitations returns all invitations for a workspace.
func (s *Store) ListWorkspaceInvitations(workspaceID string) ([]models.WorkspaceInvitation, error) {
	rows, err := s.db.Query(s.q(`
		SELECT id, workspace_id, email, role, invited_by, code, accepted_at, created_at
		FROM workspace_invitations
		WHERE workspace_id = ? AND accepted_at IS NULL
		ORDER BY created_at ASC
	`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list workspace invitations: %w", err)
	}
	defer rows.Close()

	var result []models.WorkspaceInvitation
	for rows.Next() {
		var inv models.WorkspaceInvitation
		var acceptedAt *string
		var createdAt string
		if err := rows.Scan(
			&inv.ID, &inv.WorkspaceID, &inv.Email, &inv.Role, &inv.InvitedBy,
			&inv.Code, &acceptedAt, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan invitation: %w", err)
		}
		inv.CreatedAt = parseTime(createdAt)
		inv.AcceptedAt = parseTimePtr(acceptedAt)
		result = append(result, inv)
	}
	return result, rows.Err()
}

// backfillWorkspaceOwners ensures every workspace has at least one owner.
// For workspaces with no members, the first admin user is added as owner.
// This handles the migration case where workspaces existed before the user system.
func (s *Store) backfillWorkspaceOwners() error {
	// Find the first admin user (if any)
	var adminID string
	err := s.db.QueryRow(
		s.q("SELECT id FROM users WHERE role = 'admin' ORDER BY created_at ASC LIMIT 1"),
	).Scan(&adminID)
	if err == sql.ErrNoRows {
		return nil // No users yet — nothing to backfill
	}
	if err != nil {
		return fmt.Errorf("find admin user: %w", err)
	}

	// Find workspaces with no members
	rows, err := s.db.Query(s.q(`
		SELECT w.id FROM workspaces w
		WHERE w.deleted_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM workspace_members wm WHERE wm.workspace_id = w.id)
	`))
	if err != nil {
		return fmt.Errorf("find ownerless workspaces: %w", err)
	}
	defer rows.Close()

	var wsIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		wsIDs = append(wsIDs, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, wsID := range wsIDs {
		if err := s.AddWorkspaceMember(wsID, adminID, "owner"); err != nil {
			return fmt.Errorf("add owner to workspace %s: %w", wsID, err)
		}
	}

	return nil
}
