package server

import (
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/email"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/go-chi/chi/v5"
)

// handleAdminListInvitations returns all pending invitations across all workspaces.
// GET /api/v1/admin/invitations?q=search
func (s *Server) handleAdminListInvitations(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	query := r.URL.Query().Get("q")
	invitations, err := s.store.ListPendingInvitationsAdmin(query)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if invitations == nil {
		invitations = []store.AdminInvitation{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"invitations": invitations,
	})
}

// handleAdminResendInvitation revokes the old invitation and creates a fresh one
// with a new code, then sends the invitation email.
// POST /api/v1/admin/invitations/{invID}/resend
func (s *Server) handleAdminResendInvitation(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	invID := chi.URLParam(r, "invID")
	old, err := s.store.GetInvitation(invID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if old == nil || old.AcceptedAt != nil {
		writeError(w, http.StatusNotFound, "not_found", "Pending invitation not found")
		return
	}

	// Delete old invitation — abort if it's already gone (accepted or revoked concurrently)
	if err := s.store.DeleteInvitationAdmin(invID); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Invitation is no longer pending")
		return
	}

	caller := currentUser(r)
	inviterID := old.InvitedBy
	if caller != nil {
		inviterID = caller.ID
	}

	inv, err := s.store.CreateInvitation(old.WorkspaceID, old.Email, old.Role, inviterID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Send email
	joinURL := ""
	if s.baseURL != "" {
		joinURL = s.baseURL + "/join/" + inv.Code
	}

	if s.email != nil && joinURL != "" {
		// Respect email opt-out preferences
		if optedOut, err := s.store.IsEmailOptedOut(inv.Email); err == nil && optedOut {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"ok":      true,
				"method":  "code",
				"message": "Invitation recreated but email not sent (recipient opted out)",
			})
			return
		}

		inviterName := "An administrator"
		wsName := "a workspace"
		if caller != nil {
			inviterName = caller.Name
		}
		if ws, err := s.store.GetWorkspaceByID(old.WorkspaceID); err == nil && ws != nil {
			wsName = ws.Name
		}
		unsubURL := email.UnsubscribeURL(s.baseURL, inv.Email, s.unsubscribeSecret())
		if err := s.email.SendInvitation(r.Context(), inv.Email, inviterName, wsName, joinURL, unsubURL); err != nil {
			slog.Error("failed to resend invitation email", "error", err, "email", inv.Email)
			writeError(w, http.StatusInternalServerError, "email_failed", "Invitation recreated but failed to send email")
			return
		}
	}

	s.logAuditEvent(models.ActionMemberInvited, r, auditMeta(map[string]string{
		"email":     inv.Email,
		"role":      inv.Role,
		"workspace": old.WorkspaceID,
		"resend":    "true",
	}))

	method := "code"
	if s.email != nil && joinURL != "" {
		method = "email"
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"method":  method,
		"message": "Invitation resent to " + inv.Email,
	})
}

// handleAdminDeleteInvitation revokes a pending invitation.
// DELETE /api/v1/admin/invitations/{invID}
func (s *Server) handleAdminDeleteInvitation(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	invID := chi.URLParam(r, "invID")
	if err := s.store.DeleteInvitationAdmin(invID); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "Pending invitation not found")
			return
		}
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"message": "Invitation revoked",
	})
}
