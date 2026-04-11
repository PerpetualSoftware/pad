package server

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/xarmian/pad/internal/models"
)

// handleListMembers returns all members of a workspace.
func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	members, err := s.store.ListWorkspaceMembers(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Include pending invitations, enriched with join URLs
	invitations, err := s.store.ListWorkspaceInvitations(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	type invWithURL struct {
		ID        string `json:"id"`
		Email     string `json:"email"`
		Role      string `json:"role"`
		Code      string `json:"code"`
		JoinURL   string `json:"join_url,omitempty"`
		CreatedAt string `json:"created_at"`
	}
	enrichedInvs := make([]invWithURL, len(invitations))
	for i, inv := range invitations {
		// For hashed invitations (code == id placeholder), the plaintext
		// code is not recoverable — only show code/join_url for legacy invites.
		code := inv.Code
		if code == inv.ID {
			code = ""
		}
		enrichedInvs[i] = invWithURL{
			ID:        inv.ID,
			Email:     inv.Email,
			Role:      inv.Role,
			Code:      code,
			CreatedAt: inv.CreatedAt.Format("2006-01-02T15:04:05Z"),
		}
		if s.baseURL != "" && code != "" {
			enrichedInvs[i].JoinURL = s.baseURL + "/join/" + code
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"members":     members,
		"invitations": enrichedInvs,
	})
}

// handleInviteMember creates an invitation or directly adds a user if they exist.
func (s *Server) handleInviteMember(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	// Only owners can invite
	if !requireRole(r, "owner") {
		writeError(w, http.StatusForbidden, "forbidden", "Only workspace owners can invite members")
		return
	}

	var input struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Email == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "email is required")
		return
	}
	if input.Role == "" {
		input.Role = "editor"
	}

	inviterID := currentUserID(r)

	// Check if user with this email already exists
	existingUser, err := s.store.GetUserByEmail(input.Email)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	if existingUser != nil {
		// User exists — add them directly
		alreadyMember, _ := s.store.IsWorkspaceMember(workspaceID, existingUser.ID)
		if alreadyMember {
			writeError(w, http.StatusConflict, "conflict", "User is already a member of this workspace")
			return
		}
		if err := s.store.AddWorkspaceMember(workspaceID, existingUser.ID, input.Role); err != nil {
			writeInternalError(w, err)
			return
		}
		s.logWorkspaceAuditEvent(workspaceID, models.ActionMemberInvited, r, auditMeta(map[string]string{"email": existingUser.Email, "role": input.Role, "added_directly": "true"}))
		writeJSON(w, http.StatusCreated, map[string]interface{}{
			"added":   true,
			"user_id": existingUser.ID,
			"email":   existingUser.Email,
			"name":    existingUser.Name,
			"role":    input.Role,
		})
		return
	}

	// User doesn't exist — create an invitation
	inv, err := s.store.CreateInvitation(workspaceID, input.Email, input.Role, inviterID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	resp := map[string]interface{}{
		"invited": true,
		"code":    inv.Code,
		"email":   inv.Email,
		"role":    inv.Role,
	}
	joinURL := ""
	if s.baseURL != "" {
		joinURL = s.baseURL + "/join/" + inv.Code
		resp["join_url"] = joinURL
	}

	s.logWorkspaceAuditEvent(workspaceID, models.ActionMemberInvited, r, auditMeta(map[string]string{"email": input.Email, "role": input.Role}))

	writeJSON(w, http.StatusCreated, resp)

	// Send invitation email asynchronously (fire-and-forget)
	if s.email != nil && joinURL != "" {
		go func() {
			inviterName := "A team member"
			wsName := "a workspace"
			if user, err := s.store.GetUser(inviterID); err == nil && user != nil {
				inviterName = user.Name
			}
			if ws, err := s.store.GetWorkspaceByID(workspaceID); err == nil && ws != nil {
				wsName = ws.Name
			}
			if err := s.email.SendInvitation(context.Background(), inv.Email, inviterName, wsName, joinURL); err != nil {
				slog.Error("failed to send invitation email", "error", err)
			}
		}()
	}
}

// handleRemoveMember removes a user from a workspace.
func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	if !requireRole(r, "owner") {
		writeError(w, http.StatusForbidden, "forbidden", "Only workspace owners can remove members")
		return
	}

	userID := chi.URLParam(r, "userID")

	// Prevent removing yourself
	if userID == currentUserID(r) {
		writeError(w, http.StatusBadRequest, "bad_request", "Cannot remove yourself from the workspace")
		return
	}

	if err := s.store.RemoveWorkspaceMember(workspaceID, userID); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Member not found")
		return
	}

	s.logWorkspaceAuditEvent(workspaceID, models.ActionMemberRemoved, r, auditMeta(map[string]string{"user_id": userID}))

	w.WriteHeader(http.StatusNoContent)
}

// handleUpdateMemberRole changes a member's role in a workspace.
func (s *Server) handleUpdateMemberRole(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	if !requireRole(r, "owner") {
		writeError(w, http.StatusForbidden, "forbidden", "Only workspace owners can change roles")
		return
	}

	userID := chi.URLParam(r, "userID")

	var input struct {
		Role string `json:"role"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Role == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "role is required")
		return
	}

	if err := s.store.UpdateWorkspaceMemberRole(workspaceID, userID, input.Role); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Member not found")
		return
	}

	s.logWorkspaceAuditEvent(workspaceID, models.ActionRoleChanged, r, auditMeta(map[string]string{"user_id": userID, "role": input.Role}))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user_id": userID,
		"role":    input.Role,
	})
}

// handleCancelInvitation deletes a pending invitation.
func (s *Server) handleCancelInvitation(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	if !requireRole(r, "owner") {
		writeError(w, http.StatusForbidden, "forbidden", "Only workspace owners can cancel invitations")
		return
	}

	invID := chi.URLParam(r, "invID")
	if err := s.store.DeleteInvitation(workspaceID, invID); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Invitation not found")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleAcceptInvitation accepts a workspace invitation by code.
func (s *Server) handleAcceptInvitation(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")

	inv, err := s.store.GetInvitationByCode(code)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if inv == nil {
		writeError(w, http.StatusNotFound, "not_found", "Invitation not found or already accepted")
		return
	}

	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "You must be logged in to accept an invitation")
		return
	}

	// Add user to workspace
	if err := s.store.AddWorkspaceMember(inv.WorkspaceID, user.ID, inv.Role); err != nil {
		writeInternalError(w, err)
		return
	}

	// Mark invitation as accepted
	if err := s.store.AcceptInvitation(inv.ID); err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"accepted":     true,
		"workspace_id": inv.WorkspaceID,
		"role":         inv.Role,
	})
}

// handleGetMemberCollectionAccess returns a member's collection access mode and granted IDs.
func (s *Server) handleGetMemberCollectionAccess(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	// Only workspace owners or the user themselves can view collection access
	userID := chi.URLParam(r, "userID")
	if !requireRole(r, "owner") && currentUserID(r) != userID {
		writeError(w, http.StatusForbidden, "forbidden", "Only workspace owners can view other members' collection access")
		return
	}
	member, err := s.store.GetWorkspaceMember(workspaceID, userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if member == nil {
		writeError(w, http.StatusNotFound, "not_found", "Member not found")
		return
	}

	grants, err := s.store.GetMemberCollectionAccess(workspaceID, userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"collection_access": member.CollectionAccess,
		"collection_ids":    grants,
	})
}

// handleSetMemberCollectionAccess updates a member's collection visibility.
// Only workspace owners can change other members' access.
func (s *Server) handleSetMemberCollectionAccess(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	if !requireRole(r, "owner") {
		writeError(w, http.StatusForbidden, "forbidden", "Only workspace owners can manage collection access")
		return
	}

	userID := chi.URLParam(r, "userID")
	member, err := s.store.GetWorkspaceMember(workspaceID, userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if member == nil {
		writeError(w, http.StatusNotFound, "not_found", "Member not found")
		return
	}

	var input struct {
		Mode          string   `json:"mode"`           // "all" or "specific"
		CollectionIDs []string `json:"collection_ids"` // only for "specific"
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Mode != "all" && input.Mode != "specific" {
		writeError(w, http.StatusBadRequest, "validation_error", "Mode must be 'all' or 'specific'")
		return
	}

	if err := s.store.SetMemberCollectionAccess(workspaceID, userID, input.Mode, input.CollectionIDs); err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"collection_access": input.Mode,
		"collection_ids":    input.CollectionIDs,
	})
}
