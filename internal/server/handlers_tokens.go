package server

import (
	"database/sql"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/xarmian/pad/internal/models"
)

// handleCreateToken creates a new API token scoped to a workspace.
// The token is owned by the authenticated user (if any).
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	var input models.APITokenCreate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}

	input.WorkspaceID = workspaceID

	userID := currentUserID(r)
	token, err := s.store.CreateAPIToken(userID, input)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	s.logAuditEvent(models.ActionTokenCreated, r, fmt.Sprintf(`{"name":"%s","workspace_id":"%s"}`, input.Name, input.WorkspaceID))

	writeJSON(w, http.StatusCreated, token)
}

// handleListTokens returns all API tokens for a workspace (without secrets).
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	tokens, err := s.store.ListAPITokens(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if tokens == nil {
		tokens = []models.APIToken{}
	}

	writeJSON(w, http.StatusOK, tokens)
}

// handleDeleteToken revokes an API token by ID.
func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	_, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	tokenID := chi.URLParam(r, "tokenID")
	if err := s.store.DeleteAPIToken(tokenID); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "Token not found")
			return
		}
		writeInternalError(w, err)
		return
	}

	s.logAuditEvent(models.ActionTokenRevoked, r, fmt.Sprintf(`{"token_id":"%s"}`, tokenID))

	w.WriteHeader(http.StatusNoContent)
}

// --- User-scoped token endpoints ---

// handleListUserTokens returns all API tokens owned by the authenticated user.
func (s *Server) handleListUserTokens(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	tokens, err := s.store.ListUserAPITokens(userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if tokens == nil {
		tokens = []models.APIToken{}
	}

	writeJSON(w, http.StatusOK, tokens)
}

// handleCreateUserToken creates a new API token owned by the authenticated user.
func (s *Server) handleCreateUserToken(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	var input models.APITokenCreate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}

	token, err := s.store.CreateAPIToken(userID, input)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	s.logAuditEvent(models.ActionTokenCreated, r, fmt.Sprintf(`{"name":"%s"}`, input.Name))

	writeJSON(w, http.StatusCreated, token)
}

// handleDeleteUserToken revokes an API token, verifying it belongs to the user.
func (s *Server) handleDeleteUserToken(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	tokenID := chi.URLParam(r, "tokenID")
	if err := s.store.DeleteUserAPIToken(tokenID, userID); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "Token not found")
			return
		}
		writeInternalError(w, err)
		return
	}

	s.logAuditEvent(models.ActionTokenRevoked, r, fmt.Sprintf(`{"token_id":"%s"}`, tokenID))

	w.WriteHeader(http.StatusNoContent)
}
