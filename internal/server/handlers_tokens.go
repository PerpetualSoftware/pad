package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/xarmian/pad/internal/models"
)

// tokenExpiryWarningDays is the threshold for adding near-expiry warning
// headers to API responses authenticated with a token.
const tokenExpiryWarningDays = 7

// getTokenExpirySettings reads platform settings for token expiry policy.
func (s *Server) getTokenExpirySettings() (defaultDays, maxDays int) {
	defaultDays = 90 // fallback default
	if v, err := s.store.GetPlatformSetting(settingTokenDefaultExpiryDays); err == nil && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			defaultDays = n
		}
	}
	if v, err := s.store.GetPlatformSetting(settingTokenMaxLifetimeDays); err == nil && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxDays = n
		}
	}
	return
}

// handleCreateToken creates a new API token scoped to a workspace.
// The token is owned by the authenticated user (if any).
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
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
	defaultDays, maxDays := s.getTokenExpirySettings()

	userID := currentUserID(r)
	token, err := s.store.CreateAPIToken(userID, input, defaultDays, maxDays)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	s.logAuditEvent(models.ActionTokenCreated, r, auditMeta(map[string]string{"name": input.Name, "workspace_id": input.WorkspaceID}))

	writeJSON(w, http.StatusCreated, token)
}

// handleListTokens returns all API tokens for a workspace (without secrets).
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
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
	if !requireMinRole(w, r, "owner") {
		return
	}
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

	s.logAuditEvent(models.ActionTokenRevoked, r, auditMeta(map[string]string{"token_id": tokenID}))

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

	defaultDays, maxDays := s.getTokenExpirySettings()

	token, err := s.store.CreateAPIToken(userID, input, defaultDays, maxDays)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	s.logAuditEvent(models.ActionTokenCreated, r, auditMeta(map[string]string{"name": input.Name}))

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

	s.logAuditEvent(models.ActionTokenRevoked, r, auditMeta(map[string]string{"token_id": tokenID}))

	w.WriteHeader(http.StatusNoContent)
}

// handleRotateUserToken generates a new secret for an existing token,
// invalidating the old one. The token metadata is preserved.
func (s *Server) handleRotateUserToken(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	tokenID := chi.URLParam(r, "tokenID")

	var input struct {
		ExpiresIn int `json:"expires_in,omitempty"` // new expiry in days (0 = keep existing)
	}
	// Body is optional — rotation works without it, but malformed JSON is rejected
	// to prevent silent destructive rotation when the caller intended to set expiry.
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body")
			return
		}
	}

	_, maxDays := s.getTokenExpirySettings()

	rotated, err := s.store.RotateAPIToken(tokenID, userID, input.ExpiresIn, maxDays)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "Token not found")
			return
		}
		writeInternalError(w, err)
		return
	}

	s.logAuditEvent(models.ActionTokenRotated, r, auditMeta(map[string]string{"token_id": tokenID}))

	writeJSON(w, http.StatusOK, rotated)
}

// setTokenExpiryWarning adds an X-Token-Expires-Soon header if the API token
// used for this request is within the warning threshold of its expiry.
// Called from the TokenAuth middleware after successful token validation.
func setTokenExpiryWarning(w http.ResponseWriter, token *models.APIToken) {
	if token == nil || token.ExpiresAt == nil {
		return
	}
	remaining := time.Until(*token.ExpiresAt)
	if remaining > 0 && remaining < time.Duration(tokenExpiryWarningDays)*24*time.Hour {
		days := int(remaining.Hours() / 24)
		w.Header().Set("X-Token-Expires-Soon", fmt.Sprintf("%d days remaining", days))
		w.Header().Set("X-Token-Expires-At", token.ExpiresAt.Format(time.RFC3339))
	}
}
