package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/xarmian/pad/internal/models"
)

// InvitationTTL is how long a newly-created workspace invitation stays
// accepted-by-code. 14 days mirrors the convention of every password-reset
// or magic-link flow: long enough for a distracted invitee to notice the
// email, short enough that a leaked code doesn't stay exploitable forever.
const InvitationTTL = 14 * 24 * time.Hour

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

// RemoveWorkspaceMemberAndRevokeGrants atomically removes a user from a workspace
// and revokes all their grants in a single transaction. This prevents the user
// from retaining guest access if the member removal succeeds but grant revocation fails.
func (s *Store) RemoveWorkspaceMemberAndRevokeGrants(workspaceID, userID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Revoke grants first (before removing membership)
	if _, err := tx.Exec(s.q("DELETE FROM collection_grants WHERE workspace_id = ? AND user_id = ?"), workspaceID, userID); err != nil {
		return fmt.Errorf("revoke collection grants: %w", err)
	}
	if _, err := tx.Exec(s.q("DELETE FROM item_grants WHERE workspace_id = ? AND user_id = ?"), workspaceID, userID); err != nil {
		return fmt.Errorf("revoke item grants: %w", err)
	}

	// Remove membership
	result, err := tx.Exec(s.q("DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?"), workspaceID, userID)
	if err != nil {
		return fmt.Errorf("remove workspace member: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}

	return tx.Commit()
}

// GetWorkspaceMember retrieves a single membership record.
func (s *Store) GetWorkspaceMember(workspaceID, userID string) (*models.WorkspaceMember, error) {
	var m models.WorkspaceMember
	var createdAt string

	err := s.db.QueryRow(s.q(`
		SELECT workspace_id, user_id, role, collection_access, created_at
		FROM workspace_members
		WHERE workspace_id = ? AND user_id = ?
	`), workspaceID, userID).Scan(
		&m.WorkspaceID, &m.UserID, &m.Role, &m.CollectionAccess, &createdAt,
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
		SELECT wm.workspace_id, wm.user_id, wm.role, wm.collection_access, wm.created_at,
		       u.name, u.email, u.username
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
			&m.WorkspaceID, &m.UserID, &m.Role, &m.CollectionAccess, &createdAt,
			&m.UserName, &m.UserEmail, &m.UserUsername,
		); err != nil {
			return nil, fmt.Errorf("scan workspace member: %w", err)
		}
		m.CreatedAt = parseTime(createdAt)
		result = append(result, m)
	}
	return result, rows.Err()
}

// VisibleCollectionIDs returns the set of collection IDs a member can see.
// Returns nil if the member has "all" access (meaning no filtering needed).
// System collections (conventions, playbooks) are always included for members.
func (s *Store) VisibleCollectionIDs(workspaceID, userID string) ([]string, error) {
	member, err := s.GetWorkspaceMember(workspaceID, userID)
	if err != nil {
		return nil, err
	}
	if member == nil {
		// Not a member — check for guest access via grants
		return s.GuestVisibleCollectionIDs(workspaceID, userID)
	}

	// "all" access — return nil to indicate no filtering needed
	if member.CollectionAccess == "all" || member.CollectionAccess == "" {
		return nil, nil
	}

	// "specific" access — get the granted collection IDs + system collections
	rows, err := s.db.Query(s.q(`
		SELECT collection_id FROM member_collection_access
		WHERE workspace_id = ? AND user_id = ?
	`), workspaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("get member collection access: %w", err)
	}
	defer rows.Close()

	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Always include system collections for members
	sysRows, err := s.db.Query(s.q(`
		SELECT id FROM collections
		WHERE workspace_id = ? AND is_system = ? AND deleted_at IS NULL
	`), workspaceID, s.dialect.BoolToInt(true))
	if err != nil {
		return nil, fmt.Errorf("get system collections: %w", err)
	}
	defer sysRows.Close()

	for sysRows.Next() {
		var id string
		if err := sysRows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}

	// Also include collections from direct collection grants. This ensures
	// that members with "specific" access who are granted additional
	// collections can see them even if they aren't in member_collection_access.
	// Note: we only merge full collection grants here, NOT collections derived
	// from item grants. Item grants should not promote to collection-wide
	// visibility for members — the item-level filtering in handlers handles that.
	collGrantRows, err := s.db.Query(s.q(`
		SELECT DISTINCT collection_id FROM collection_grants
		WHERE workspace_id = ? AND user_id = ?
	`), workspaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("get member collection grants: %w", err)
	}
	defer collGrantRows.Close()
	for collGrantRows.Next() {
		var id string
		if err := collGrantRows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	if err := collGrantRows.Err(); err != nil {
		return nil, err
	}

	// Also include collections that contain items with item grants, so the
	// collection appears in navigation. The actual item-level filtering is
	// handled by the request handlers for members who also have item grants.
	itemCollRows, err := s.db.Query(s.q(`
		SELECT DISTINCT i.collection_id
		FROM item_grants ig
		JOIN items i ON i.id = ig.item_id
		WHERE ig.workspace_id = ? AND ig.user_id = ? AND i.deleted_at IS NULL
	`), workspaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("get member item grant collections: %w", err)
	}
	defer itemCollRows.Close()
	for itemCollRows.Next() {
		var id string
		if err := itemCollRows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	if err := itemCollRows.Err(); err != nil {
		return nil, err
	}

	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	return result, nil
}

// SetMemberCollectionAccess updates a member's collection_access mode and
// replaces their specific collection grants atomically.
func (s *Store) SetMemberCollectionAccess(workspaceID, userID, mode string, collectionIDs []string) error {
	ts := now()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Validate that all collection IDs belong to this workspace
	if mode == "specific" && len(collectionIDs) > 0 {
		for _, collID := range collectionIDs {
			var count int
			if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM collections WHERE id = ? AND workspace_id = ?`), collID, workspaceID).Scan(&count); err != nil {
				return fmt.Errorf("validate collection: %w", err)
			}
			if count == 0 {
				return fmt.Errorf("collection %s does not belong to workspace", collID)
			}
		}
	}

	// Update the mode on workspace_members
	_, err = tx.Exec(s.q(`
		UPDATE workspace_members SET collection_access = ?
		WHERE workspace_id = ? AND user_id = ?
	`), mode, workspaceID, userID)
	if err != nil {
		return fmt.Errorf("update collection_access: %w", err)
	}

	// Clear existing grants
	_, err = tx.Exec(s.q(`
		DELETE FROM member_collection_access
		WHERE workspace_id = ? AND user_id = ?
	`), workspaceID, userID)
	if err != nil {
		return fmt.Errorf("clear collection access: %w", err)
	}

	// Insert new grants (only if mode is "specific")
	if mode == "specific" {
		for _, collID := range collectionIDs {
			_, err := tx.Exec(s.q(`
				INSERT INTO member_collection_access (workspace_id, user_id, collection_id, created_at)
				VALUES (?, ?, ?, ?)
			`), workspaceID, userID, collID, ts)
			if err != nil {
				return fmt.Errorf("insert collection access: %w", err)
			}
		}
	}

	return tx.Commit()
}

// GetMemberCollectionAccess returns the collection IDs a member has been
// explicitly granted access to (only meaningful when collection_access = "specific").
func (s *Store) GetMemberCollectionAccess(workspaceID, userID string) ([]string, error) {
	rows, err := s.db.Query(s.q(`
		SELECT collection_id FROM member_collection_access
		WHERE workspace_id = ? AND user_id = ?
	`), workspaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("get member collection access: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListSystemCollectionIDs returns the IDs of system collections in a workspace.
// System collections are always visible to members regardless of collection_access mode.
func (s *Store) ListSystemCollectionIDs(workspaceID string) ([]string, error) {
	rows, err := s.db.Query(s.q(`
		SELECT id FROM collections
		WHERE workspace_id = ? AND is_system = ? AND deleted_at IS NULL
	`), workspaceID, s.dialect.BoolToInt(true))
	if err != nil {
		return nil, fmt.Errorf("list system collections: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetUserWorkspaces returns all workspaces a user has access to,
// sorted by the user's custom sort order (then name as tiebreaker).
func (s *Store) GetUserWorkspaces(userID string) ([]models.Workspace, error) {
	rows, err := s.db.Query(s.q(`
		SELECT w.id, w.name, w.slug, w.owner_id, COALESCE(ou.username, ''), w.description, w.settings, w.created_at, w.updated_at, w.deleted_at,
		       wm.sort_order
		FROM workspaces w
		JOIN workspace_members wm ON wm.workspace_id = w.id
		LEFT JOIN users ou ON ou.id = w.owner_id
		WHERE wm.user_id = ? AND w.deleted_at IS NULL
		ORDER BY wm.sort_order ASC, w.name ASC
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
			&ws.ID, &ws.Name, &ws.Slug, &ws.OwnerID, &ws.OwnerUsername, &ws.Description, &ws.Settings,
			&createdAt, &updatedAt, &deletedAt,
			&ws.SortOrder,
		); err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		ws.CreatedAt = parseTime(createdAt)
		ws.UpdatedAt = parseTime(updatedAt)
		ws.DeletedAt = parseTimePtr(deletedAt)
		result = append(result, ws)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Also include workspaces where the user has grants but is NOT a member (guest access)
	memberIDs := make(map[string]bool)
	for _, ws := range result {
		memberIDs[ws.ID] = true
	}

	guestRows, err := s.db.Query(s.q(`
		SELECT DISTINCT w.id, w.name, w.slug, w.owner_id, COALESCE(ou.username, ''), w.description, w.settings, w.created_at, w.updated_at, w.deleted_at
		FROM workspaces w
		LEFT JOIN users ou ON ou.id = w.owner_id
		WHERE w.deleted_at IS NULL AND (
			EXISTS (
				SELECT 1 FROM collection_grants cg
				JOIN collections c ON c.id = cg.collection_id
				WHERE cg.workspace_id = w.id AND cg.user_id = ? AND c.deleted_at IS NULL
			)
			OR EXISTS (
				SELECT 1 FROM item_grants ig
				JOIN items i ON i.id = ig.item_id
				JOIN collections c ON c.id = i.collection_id
				WHERE ig.workspace_id = w.id AND ig.user_id = ? AND i.deleted_at IS NULL AND c.deleted_at IS NULL
			)
		)
	`), userID, userID)
	if err != nil {
		return nil, fmt.Errorf("get guest workspaces: %w", err)
	}
	defer guestRows.Close()

	for guestRows.Next() {
		var ws models.Workspace
		var createdAt, updatedAt string
		var deletedAt *string
		if err := guestRows.Scan(
			&ws.ID, &ws.Name, &ws.Slug, &ws.OwnerID, &ws.OwnerUsername, &ws.Description, &ws.Settings,
			&createdAt, &updatedAt, &deletedAt,
		); err != nil {
			return nil, fmt.Errorf("scan guest workspace: %w", err)
		}
		// Skip workspaces the user is already a member of
		if memberIDs[ws.ID] {
			continue
		}
		ws.CreatedAt = parseTime(createdAt)
		ws.UpdatedAt = parseTime(updatedAt)
		ws.DeletedAt = parseTimePtr(deletedAt)
		ws.IsGuest = true
		result = append(result, ws)
	}
	if err := guestRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate guest workspaces: %w", err)
	}

	return result, nil
}

// UpdateWorkspaceSortOrder sets the sort_order for a workspace in a user's membership.
func (s *Store) UpdateWorkspaceSortOrder(userID, workspaceID string, sortOrder int) error {
	result, err := s.db.Exec(
		s.q("UPDATE workspace_members SET sort_order = ? WHERE user_id = ? AND workspace_id = ?"),
		sortOrder, userID, workspaceID,
	)
	if err != nil {
		return fmt.Errorf("update workspace sort order: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
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
	expiresAt := time.Now().UTC().Add(InvitationTTL).Format(time.RFC3339)

	_, err := s.db.Exec(s.q(`
		INSERT INTO workspace_invitations (id, workspace_id, email, role, invited_by, code, code_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), id, workspaceID, strings.ToLower(strings.TrimSpace(email)), role, invitedBy, id, codeHash, ts, expiresAt)
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
	var acceptedAt, expiresAt *string
	var createdAt string

	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, email, role, invited_by, code, accepted_at, expires_at, created_at
		FROM workspace_invitations WHERE id = ?
	`), id).Scan(
		&inv.ID, &inv.WorkspaceID, &inv.Email, &inv.Role, &inv.InvitedBy,
		&inv.Code, &acceptedAt, &expiresAt, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get invitation: %w", err)
	}

	inv.CreatedAt = parseTime(createdAt)
	inv.AcceptedAt = parseTimePtr(acceptedAt)
	inv.ExpiresAt = parseTimePtr(expiresAt)
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
	var acceptedAt, expiresAt *string
	var createdAt string

	// Try hashed lookup first (new invitations)
	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, email, role, invited_by, code, accepted_at, expires_at, created_at
		FROM workspace_invitations WHERE code_hash = ? AND accepted_at IS NULL
	`), codeHash).Scan(
		&inv.ID, &inv.WorkspaceID, &inv.Email, &inv.Role, &inv.InvitedBy,
		&inv.Code, &acceptedAt, &expiresAt, &createdAt,
	)
	if err == nil {
		inv.CreatedAt = parseTime(createdAt)
		inv.AcceptedAt = parseTimePtr(acceptedAt)
		inv.ExpiresAt = parseTimePtr(expiresAt)
		return &inv, nil
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("get invitation by code hash: %w", err)
	}

	// Fall back to plaintext lookup (legacy invitations)
	err = s.db.QueryRow(s.q(`
		SELECT id, workspace_id, email, role, invited_by, code, accepted_at, expires_at, created_at
		FROM workspace_invitations WHERE code = ? AND accepted_at IS NULL
	`), code).Scan(
		&inv.ID, &inv.WorkspaceID, &inv.Email, &inv.Role, &inv.InvitedBy,
		&inv.Code, &acceptedAt, &expiresAt, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get invitation by code: %w", err)
	}

	inv.CreatedAt = parseTime(createdAt)
	inv.AcceptedAt = parseTimePtr(acceptedAt)
	inv.ExpiresAt = parseTimePtr(expiresAt)
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
		SELECT id, workspace_id, email, role, invited_by, code, accepted_at, expires_at, created_at
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
		var acceptedAt, expiresAt *string
		var createdAt string
		if err := rows.Scan(
			&inv.ID, &inv.WorkspaceID, &inv.Email, &inv.Role, &inv.InvitedBy,
			&inv.Code, &acceptedAt, &expiresAt, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan invitation: %w", err)
		}
		inv.CreatedAt = parseTime(createdAt)
		inv.AcceptedAt = parseTimePtr(acceptedAt)
		inv.ExpiresAt = parseTimePtr(expiresAt)
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

	// Backfill owner_id column on workspaces that don't have one set.
	// Uses D3 logic: earliest owner member → earliest member → first admin.
	ownerlessRows, err := s.db.Query(s.q(`
		SELECT id FROM workspaces WHERE (owner_id = '' OR owner_id IS NULL) AND deleted_at IS NULL
	`))
	if err != nil {
		return fmt.Errorf("find workspaces without owner_id: %w", err)
	}
	defer ownerlessRows.Close()

	var ownerlessIDs []string
	for ownerlessRows.Next() {
		var id string
		if err := ownerlessRows.Scan(&id); err != nil {
			return err
		}
		ownerlessIDs = append(ownerlessIDs, id)
	}
	if err := ownerlessRows.Err(); err != nil {
		return err
	}

	for _, wsID := range ownerlessIDs {
		var ownerID string

		// Try: earliest member with "owner" role
		err := s.db.QueryRow(s.q(`
			SELECT user_id FROM workspace_members
			WHERE workspace_id = ? AND role = 'owner'
			ORDER BY created_at ASC LIMIT 1
		`), wsID).Scan(&ownerID)

		if err == sql.ErrNoRows {
			// Try: earliest member regardless of role
			err = s.db.QueryRow(s.q(`
				SELECT user_id FROM workspace_members
				WHERE workspace_id = ?
				ORDER BY created_at ASC LIMIT 1
			`), wsID).Scan(&ownerID)
		}

		if err == sql.ErrNoRows {
			// Fall back to first admin
			ownerID = adminID
		} else if err != nil {
			return fmt.Errorf("find owner for workspace %s: %w", wsID, err)
		}

		_, err = s.db.Exec(s.q(`UPDATE workspaces SET owner_id = ? WHERE id = ?`), ownerID, wsID)
		if err != nil {
			return fmt.Errorf("set owner_id for workspace %s: %w", wsID, err)
		}
	}

	return nil
}

// AdminInvitation holds enriched invitation data for the admin panel.
type AdminInvitation struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	Role          string `json:"role"`
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name"`
	WorkspaceSlug string `json:"workspace_slug"`
	InvitedByID   string `json:"invited_by_id"`
	InvitedByName string `json:"invited_by_name"`
	CreatedAt     string `json:"created_at"`
}

// ListPendingInvitationsAdmin returns all pending invitations across all workspaces
// with workspace and inviter details for the admin panel.
func (s *Store) ListPendingInvitationsAdmin(query string) ([]AdminInvitation, error) {
	var where []string
	var args []interface{}

	where = append(where, "i.accepted_at IS NULL")
	where = append(where, "w.deleted_at IS NULL")

	if query != "" {
		q := "%" + strings.ToLower(query) + "%"
		where = append(where, "(LOWER(i.email) LIKE ? OR LOWER(w.name) LIKE ?)")
		args = append(args, q, q)
	}

	whereClause := "WHERE " + strings.Join(where, " AND ")

	rows, err := s.db.Query(s.q(`
		SELECT i.id, i.email, i.role, w.id, w.name, w.slug, i.invited_by, COALESCE(u.name, ''), i.created_at
		FROM workspace_invitations i
		JOIN workspaces w ON w.id = i.workspace_id
		LEFT JOIN users u ON u.id = i.invited_by
		`+whereClause+`
		ORDER BY i.created_at DESC
	`), args...)
	if err != nil {
		return nil, fmt.Errorf("list pending invitations admin: %w", err)
	}
	defer rows.Close()

	var result []AdminInvitation
	for rows.Next() {
		var inv AdminInvitation
		if err := rows.Scan(&inv.ID, &inv.Email, &inv.Role, &inv.WorkspaceID, &inv.WorkspaceName,
			&inv.WorkspaceSlug, &inv.InvitedByID, &inv.InvitedByName, &inv.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan admin invitation: %w", err)
		}
		result = append(result, inv)
	}
	return result, rows.Err()
}

// DeleteInvitationAdmin removes a pending invitation by ID (no workspace scoping).
func (s *Store) DeleteInvitationAdmin(invitationID string) error {
	result, err := s.db.Exec(
		s.q("DELETE FROM workspace_invitations WHERE id = ? AND accepted_at IS NULL"),
		invitationID,
	)
	if err != nil {
		return fmt.Errorf("delete invitation admin: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AdminUserWorkspace is a lightweight workspace membership for admin views.
type AdminUserWorkspace struct {
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name"`
	WorkspaceSlug string `json:"workspace_slug"`
	OwnerUsername string `json:"owner_username"`
	Role          string `json:"role"`
	JoinedAt      string `json:"joined_at"`
}

// GetUserWorkspaceMemberships returns workspace memberships for admin user detail.
func (s *Store) GetUserWorkspaceMemberships(userID string) ([]AdminUserWorkspace, error) {
	rows, err := s.db.Query(s.q(`
		SELECT w.id, w.name, w.slug, COALESCE(ou.username, ''), wm.role, wm.created_at
		FROM workspace_members wm
		JOIN workspaces w ON w.id = wm.workspace_id
		LEFT JOIN users ou ON ou.id = w.owner_id
		WHERE wm.user_id = ? AND w.deleted_at IS NULL
		ORDER BY w.name ASC
	`), userID)
	if err != nil {
		return nil, fmt.Errorf("get user workspace memberships: %w", err)
	}
	defer rows.Close()

	var result []AdminUserWorkspace
	for rows.Next() {
		var m AdminUserWorkspace
		if err := rows.Scan(&m.WorkspaceID, &m.WorkspaceName, &m.WorkspaceSlug, &m.OwnerUsername, &m.Role, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan workspace membership: %w", err)
		}
		result = append(result, m)
	}
	return result, rows.Err()
}
