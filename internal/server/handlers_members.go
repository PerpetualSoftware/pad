package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// handleListMembers returns all members of a workspace.
func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	members, err := s.store.ListWorkspaceMembers(workspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Include pending invitations, enriched with join URLs
	invitations, err := s.store.ListWorkspaceInvitations(workspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
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
		enrichedInvs[i] = invWithURL{
			ID:        inv.ID,
			Email:     inv.Email,
			Role:      inv.Role,
			Code:      inv.Code,
			CreatedAt: inv.CreatedAt.Format("2006-01-02T15:04:05Z"),
		}
		if s.baseURL != "" {
			enrichedInvs[i].JoinURL = s.baseURL + "/join/" + inv.Code
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
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
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
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
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
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	resp := map[string]interface{}{
		"invited": true,
		"code":    inv.Code,
		"email":   inv.Email,
		"role":    inv.Role,
	}
	if s.baseURL != "" {
		resp["join_url"] = s.baseURL + "/join/" + inv.Code
	}

	writeJSON(w, http.StatusCreated, resp)
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

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user_id": userID,
		"role":    input.Role,
	})
}

// handleAcceptInvitation accepts a workspace invitation by code.
func (s *Server) handleAcceptInvitation(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")

	inv, err := s.store.GetInvitationByCode(code)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
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
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Mark invitation as accepted
	if err := s.store.AcceptInvitation(inv.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"accepted":     true,
		"workspace_id": inv.WorkspaceID,
		"role":         inv.Role,
	})
}
