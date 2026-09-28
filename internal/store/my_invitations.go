package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// MyInvitation is a pending invitation addressed to the caller, enriched with
// what the "+" surface renders (PLAN-3002 U4b / TASK-3277). It carries no
// code: only the code's hash is stored, so the invitee accepts it by ID
// through POST /me/invitations/{id}/accept.
type MyInvitation struct {
	ID                     string     `json:"id"`
	Role                   string     `json:"role"`
	WorkspaceID            string     `json:"-"`
	WorkspaceSlug          string     `json:"workspace_slug"`
	WorkspaceName          string     `json:"workspace_name"`
	WorkspaceOwnerUsername string     `json:"workspace_owner_username"`
	InvitedByName          string     `json:"invited_by_name"`
	CreatedAt              time.Time  `json:"created_at"`
	ExpiresAt              *time.Time `json:"expires_at,omitempty"`
}

// ListPendingInvitationsForEmail returns the invitations addressed to email
// that can still be accepted by userID: not accepted, not expired, to a live
// workspace (BUG-3104), and to a workspace userID is not already a member of
// (accepting one of those inserts a duplicate membership).
//
// The caller decides whether email is proven. This method only matches it.
// Emails are stored lowercased and trimmed on both sides (CreateInvitation,
// CreateUser), so the match is exact equality on the normalised form, the same
// set strings.EqualFold admits at the accept door for rows written that way.
func (s *Store) ListPendingInvitationsForEmail(email, userID string) ([]MyInvitation, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil, nil
	}
	rows, err := s.db.Query(s.q(`
		SELECT i.id, i.role, w.id, w.slug, w.name, COALESCE(ou.username, ''),
		       COALESCE(u.name, ''), i.created_at, i.expires_at
		FROM workspace_invitations i
		JOIN workspaces w ON w.id = i.workspace_id AND w.deleted_at IS NULL
		LEFT JOIN users ou ON ou.id = w.owner_id
		LEFT JOIN users u ON u.id = i.invited_by
		WHERE i.email = ? AND i.accepted_at IS NULL
		  AND NOT EXISTS (
		    SELECT 1 FROM workspace_members m
		    WHERE m.workspace_id = i.workspace_id AND m.user_id = ?
		  )
		ORDER BY i.created_at DESC, i.id ASC
	`), email, userID)
	if err != nil {
		return nil, fmt.Errorf("list pending invitations for email: %w", err)
	}
	defer rows.Close()

	result := []MyInvitation{}
	for rows.Next() {
		var inv MyInvitation
		var createdAt string
		var expiresAt *string
		if err := rows.Scan(&inv.ID, &inv.Role, &inv.WorkspaceID, &inv.WorkspaceSlug, &inv.WorkspaceName,
			&inv.WorkspaceOwnerUsername, &inv.InvitedByName, &createdAt, &expiresAt); err != nil {
			return nil, fmt.Errorf("scan pending invitation: %w", err)
		}
		inv.CreatedAt = parseTime(createdAt)
		inv.ExpiresAt = parseTimePtr(expiresAt)
		// Expiry is decided by the same predicate the accept door uses, in Go,
		// rather than by comparing TEXT timestamps in SQL.
		if (&models.WorkspaceInvitation{ExpiresAt: inv.ExpiresAt}).IsExpired() {
			continue
		}
		result = append(result, inv)
	}
	return result, rows.Err()
}

// GetPendingInvitationForEmail returns the pending invitation id if, and only
// if, it is addressed to email and its workspace is live. Anything else
// (unknown id, someone else's invitation, accepted, deleted workspace) is nil
// with no error, so the by-id accept door answers all of them alike.
// Expiry is left to the caller, which reports it distinctly, as the by-code
// door does.
func (s *Store) GetPendingInvitationForEmail(id, email string) (*models.WorkspaceInvitation, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || id == "" {
		return nil, nil
	}
	var inv models.WorkspaceInvitation
	var acceptedAt, expiresAt *string
	var createdAt string
	err := s.db.QueryRow(s.q(`
		SELECT i.id, i.workspace_id, i.email, i.role, i.invited_by, i.code, i.accepted_at, i.expires_at, i.created_at
		FROM workspace_invitations i
		JOIN workspaces w ON w.id = i.workspace_id AND w.deleted_at IS NULL
		WHERE i.id = ? AND i.email = ? AND i.accepted_at IS NULL
	`), id, email).Scan(
		&inv.ID, &inv.WorkspaceID, &inv.Email, &inv.Role, &inv.InvitedBy,
		&inv.Code, &acceptedAt, &expiresAt, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get pending invitation for email: %w", err)
	}
	inv.CreatedAt = parseTime(createdAt)
	inv.AcceptedAt = parseTimePtr(acceptedAt)
	inv.ExpiresAt = parseTimePtr(expiresAt)
	return &inv, nil
}
