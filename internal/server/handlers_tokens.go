package server

import (
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/xarmian/pad/internal/models"
)

// handleCreateToken creates a new API token for a workspace.
// The plaintext token is returned only in this response.
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

	token, err := s.store.CreateAPIToken(workspaceID, input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

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
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
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
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
