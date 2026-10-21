package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
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

// requireInteractiveSession refuses a request authenticated by a long-lived
// API token, and is the gate on every door that MINTS or ROTATES one
// (BUG-2890, ruled on that item's trail day 57).
//
// The escalation it closes: a PAT that can reach a mint door issues further
// tokens with independent names and expiries, which outlive the revocation
// of the token that created them. Revoking a leaked credential then does
// not end the access it was used to establish, and nothing in the token
// list says which token minted which.
//
// It gates the CREDENTIAL, not the door. `isAPITokenAuth` is false for a
// session cookie AND for a `padsess_` CLI bearer — that distinction is
// deliberate and predates this fix (see ctxValidatedSessionBearer's note in
// middleware_auth.go): a CLI session IS an interactive session. A gate
// written against "carries an Authorization: Bearer header" would look
// identical on every PAT test and break every logged-in CLI.
//
// LIST and REVOKE deliberately stay reachable by a PAT. Neither extends
// access, and revocation is the compromised-credential response — gating it
// behind a browser would put a session in the way of the incident path.
//
// Same shape as the 2FA handlers' gate; the code differs because this one
// is `session_required` rather than the generic `forbidden`, so a client
// can tell "wrong credential kind" from "insufficient permissions".
func requireInteractiveSession(w http.ResponseWriter, r *http.Request) bool {
	if isAPITokenAuth(r) {
		writeError(w, http.StatusForbidden, "session_required",
			"Creating or rotating API tokens requires an interactive session, not an API token")
		return false
	}
	return true
}

// handleCreateToken creates a new API token scoped to a workspace.
// The token is owned by the authenticated user (if any).
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	// Before requireMinRole, not after: this door mints through the same
	// store call as /auth/tokens and is reachable by an OWNER's PAT
	// (measured on BUG-2890's trail — the filing named only the two
	// /auth/tokens doors). Checking the credential kind first also stops
	// a PAT-borne caller learning its own membership status from the
	// difference between the two 403s.
	if !requireInteractiveSession(w, r) {
		return
	}
	if !requireMinRole(w, r, "owner") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	var req apiTokenCreateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	input := req.APITokenCreate

	if input.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	if !normalizeTokenScopesInput(w, &input, req.RawScopes) {
		return
	}

	input.WorkspaceID = workspaceID
	defaultDays, maxDays := s.getTokenExpirySettings()

	userID := currentUserID(r)
	// The api_tokens cap counts every token the USER owns, so a token minted
	// here counts against it too and is refused at the cap (BUG-2808). The
	// note says so, because this request names a workspace and a caller
	// would otherwise read the refusal as a per-workspace limit.
	token, err := s.store.CreateAPIToken(userID, input, defaultDays, maxDays, s.planLimitMintOpts(userID)...)
	if err != nil {
		if writeStorePlanLimitError(w, r, err, "This limit counts every API token you own, across all workspaces.") {
			return
		}
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
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	// Scope the revoke to the URL's workspace. requireMinRole("owner") only
	// proves the caller owns THIS workspace, not that {tokenID} belongs to it,
	// so an unscoped delete-by-id is a cross-workspace IDOR (TASK-266). Mirrors
	// DeleteUserAPIToken's user-scoped guard.
	tokenID := chi.URLParam(r, "tokenID")
	if err := s.store.DeleteAPITokenScoped(tokenID, workspaceID); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "Token not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	// TASK-3365: a legacy workspace token's streams have no user and are
	// reached through the workspace; they re-check their credential now.
	s.invalidateWorkspaceAccess(workspaceID)

	s.logAuditEvent(models.ActionTokenRevoked, r, auditMeta(map[string]string{"token_id": tokenID}))

	w.WriteHeader(http.StatusNoContent)
}

// --- User-scoped token endpoints ---

// handleListUserTokens returns all API tokens owned by the authenticated user.
// A workspace-pinned token sees only the tokens pinned to its own workspace,
// itself included (BUG-3982): the names and pins of the user's other tokens are
// facts about other workspaces.
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
	if pin := tokenPinFrom(r.Context()); pin != nil {
		visible := make([]models.APIToken, 0, len(tokens))
		for _, t := range tokens {
			if t.WorkspaceID == pin.ID {
				visible = append(visible, t)
			}
		}
		tokens = visible
	}
	if tokens == nil {
		tokens = []models.APIToken{}
	}

	writeJSON(w, http.StatusOK, tokens)
}

// handleCreateUserToken creates a new API token owned by the authenticated user.
func (s *Server) handleCreateUserToken(w http.ResponseWriter, r *http.Request) {
	if !requireInteractiveSession(w, r) {
		return
	}

	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	var req apiTokenCreateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	input := req.APITokenCreate

	if input.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	if !normalizeTokenScopesInput(w, &input, req.RawScopes) {
		return
	}

	// A token may be pinned only to a live workspace the caller belongs to
	// (BUG-3346). Anything else is the workspace 404, identical for a
	// workspace that does not exist and one the caller is not in.
	if input.WorkspaceID != "" {
		ws, err := s.store.GetWorkspaceByID(input.WorkspaceID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		var member *models.WorkspaceMember
		if ws != nil && ws.DeletedAt == nil {
			if member, err = s.store.GetWorkspaceMember(ws.ID, userID); err != nil {
				writeInternalError(w, err)
				return
			}
		}
		if member == nil {
			writeWorkspaceNotFound(w, "Workspace not found")
			return
		}
	}

	// Enforce API token count limit (user-scoped)
	if !s.enforceUserPlanLimit(w, r, userID, "api_tokens") {
		return
	}

	defaultDays, maxDays := s.getTokenExpirySettings()

	token, err := s.store.CreateAPIToken(userID, input, defaultDays, maxDays, s.planLimitMintOpts(userID)...)
	if err != nil {
		if writeStorePlanLimitError(w, r, err, "") {
			return
		}
		writeInternalError(w, err)
		return
	}

	s.logAuditEvent(models.ActionTokenCreated, r, auditMeta(map[string]string{"name": input.Name}))

	writeJSON(w, http.StatusCreated, token)
}

// handleDeleteUserToken revokes an API token, verifying it belongs to the user.
// A workspace-pinned token may revoke only the tokens it can list (BUG-3982):
// any other id is the same 404 as a missing one.
func (s *Server) handleDeleteUserToken(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	tokenID := chi.URLParam(r, "tokenID")
	var err error
	if pin := tokenPinFrom(r.Context()); pin != nil {
		err = s.store.DeleteUserAPITokenInWorkspace(tokenID, userID, pin.ID)
	} else {
		err = s.store.DeleteUserAPIToken(tokenID, userID)
	}
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "Token not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	// TASK-3365: connections opened with this token re-check it now.
	s.invalidateUserAccess(userID)

	s.logAuditEvent(models.ActionTokenRevoked, r, auditMeta(map[string]string{"token_id": tokenID}))

	w.WriteHeader(http.StatusNoContent)
}

// handleRotateUserToken generates a new secret for an existing token,
// invalidating the old one. The token metadata is preserved.
func (s *Server) handleRotateUserToken(w http.ResponseWriter, r *http.Request) {
	if !requireInteractiveSession(w, r) {
		return
	}

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
	// TASK-3365: the old secret is dead; connections opened with it
	// re-check now.
	s.invalidateUserAccess(userID)

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

// apiTokenScopeVocabulary is every scope value tokenScopeAllows recognizes
// (TASK-3866). A key is minted only with these: anything else is stored
// as written and then refused on every request, so a create that names
// one would hand back a key that can do nothing.
var apiTokenScopeVocabulary = []string{"*", "read", "write", "pad:read", "pad:write", "pad:admin"}

// apiTokenCreateRequest is a create body with scopes kept raw, so an
// explicit "scopes": null can be told apart from an omitted field (Codex
// r1 on TASK-3866). Decoded into models.APITokenCreate alone, null and
// absent both arrive as "" and both would mint a full-access key; the
// shallower RawScopes field takes the "scopes" key from the embedded one.
type apiTokenCreateRequest struct {
	models.APITokenCreate
	RawScopes json.RawMessage `json:"scopes,omitempty"`
}

// normalizeTokenScopesInput checks a create request's scopes and rewrites
// them to their compact JSON form, writing a 400 validation_error and
// returning false when they cannot mint a working key. raw is the request's
// "scopes" value as sent: only an absent field keeps the store's default,
// ["*"]. Refused: a JSON null field, a value that is not a string, the
// empty string and the empty array (the legacy unrestricted forms, which
// no current caller sends and no new key should carry), a string that is
// not a JSON array of strings (including "null"), and any value outside
// the vocabulary.
func normalizeTokenScopesInput(w http.ResponseWriter, input *models.APITokenCreate, raw json.RawMessage) bool {
	allowed := strings.Join(apiTokenScopeVocabulary, ", ")
	input.Scopes = ""
	if len(raw) == 0 {
		return true
	}
	// Into a pointer, so null is nil however it is spaced.
	var field *string
	if err := json.Unmarshal(raw, &field); err != nil || field == nil {
		writeError(w, http.StatusBadRequest, "validation_error",
			fmt.Sprintf("scopes must be a string holding a JSON array of: %s (got %s); omit scopes for full access", allowed, raw))
		return false
	}
	// "" is the stored legacy unrestricted form, refused for the reason []
	// is (Codex r2): only an omitted field gets the default.
	if *field == "" {
		writeError(w, http.StatusBadRequest, "validation_error",
			fmt.Sprintf("scopes must name at least one of: %s; omit scopes for full access", allowed))
		return false
	}
	input.Scopes = *field
	var scopes []string
	if err := json.Unmarshal([]byte(input.Scopes), &scopes); err != nil || scopes == nil {
		writeError(w, http.StatusBadRequest, "validation_error",
			fmt.Sprintf("scopes must be a JSON array of: %s (got %q)", allowed, input.Scopes))
		return false
	}
	if len(scopes) == 0 {
		writeError(w, http.StatusBadRequest, "validation_error",
			fmt.Sprintf("scopes must name at least one of: %s; omit scopes for full access", allowed))
		return false
	}
	for _, scope := range scopes {
		if !slices.Contains(apiTokenScopeVocabulary, scope) {
			writeError(w, http.StatusBadRequest, "validation_error",
				fmt.Sprintf("unknown scope %q; allowed values: %s", scope, allowed))
			return false
		}
	}
	encoded, err := json.Marshal(scopes)
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	input.Scopes = string(encoded)
	return true
}
