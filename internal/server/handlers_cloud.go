package server

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xarmian/pad/internal/models"
	"github.com/xarmian/pad/internal/store"
)

// validateCloudSecret checks the cloud_secret field in a JSON request body
// against the server's configured cloud secret. Returns true if the secret
// matches; writes a 403 error and returns false otherwise.
//
// This is the authentication mechanism between the pad-cloud sidecar and
// the pad binary. All cloud-gated endpoints (oauth-login, admin/plan) must
// call this before processing the request.
func (s *Server) validateCloudSecret(secret string, w http.ResponseWriter) bool {
	if len(s.cloudSecrets) == 0 {
		writeError(w, http.StatusForbidden, "forbidden", "Cloud mode not configured")
		return false
	}
	for i, key := range s.cloudSecrets {
		if subtle.ConstantTimeCompare([]byte(secret), []byte(key)) == 1 {
			if i > 0 {
				slog.Info("cloud secret validated with rotated key", "key_index", i)
			}
			return true
		}
	}
	writeError(w, http.StatusForbidden, "forbidden", "Invalid cloud secret")
	return false
}

// requireCloudMode is a middleware/guard that rejects requests when the server
// is not running in cloud mode. Used to protect cloud-only endpoints.
func (s *Server) requireCloudMode(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cloudMode {
			writeError(w, http.StatusNotFound, "not_found", "Not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- OAuth Login (TASK-430) ---

// handleOAuthLogin handles POST /api/v1/auth/oauth-login.
// Called by the pad-cloud sidecar after completing an OAuth flow.
// Creates or finds a user by email and creates a session.
func (s *Server) handleOAuthLogin(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Provider      string `json:"provider"`
		Email         string `json:"email"`
		Name          string `json:"name"`
		AvatarURL     string `json:"avatar_url"`
		EmailVerified bool   `json:"email_verified"`
		CloudSecret   string `json:"cloud_secret"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	// 1. Validate cloud secret
	if !s.validateCloudSecret(input.CloudSecret, w) {
		slog.Warn("oauth-login: invalid cloud secret", "provider", input.Provider, "email", input.Email)
		return
	}

	// 2. Validate required fields
	if input.Provider == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "provider is required")
		return
	}
	if input.Provider != "github" && input.Provider != "google" {
		writeError(w, http.StatusBadRequest, "bad_request", "provider must be 'github' or 'google'")
		return
	}

	input.Email = strings.ToLower(strings.TrimSpace(input.Email))
	if input.Email == "" || !emailRegexp.MatchString(input.Email) {
		writeError(w, http.StatusBadRequest, "bad_request", "valid email is required")
		return
	}

	// 3. Require verified email from OAuth provider
	if !input.EmailVerified {
		slog.Warn("oauth-login: rejected unverified email", "provider", input.Provider, "email", input.Email)
		s.logAuditEvent(models.ActionOAuthLoginFailed, r, auditMeta(map[string]string{
			"provider": input.Provider,
			"email":    input.Email,
			"reason":   "email_not_verified",
		}))
		writeError(w, http.StatusForbidden, "forbidden", "Only verified email addresses are accepted from OAuth providers")
		return
	}

	// 4. Sanitize inputs
	input.Name = strings.TrimSpace(input.Name)
	if len(input.Name) > 200 {
		input.Name = input.Name[:200]
	}
	if input.AvatarURL != "" {
		if u, err := url.Parse(input.AvatarURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			input.AvatarURL = "" // Invalid URL — drop it
		}
	}

	// 5. Find or create user
	user, err := s.store.GetUserByEmail(input.Email)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	isNewUser := false
	if user == nil {
		// Create new user from OAuth
		if input.Name == "" {
			input.Name = strings.Split(input.Email, "@")[0]
		}
		user, err = s.store.CreateOAuthUser(input.Email, input.Name, input.AvatarURL)
		if err != nil {
			slog.Error("oauth-login: failed to create user", "error", err, "email", input.Email)
			writeInternalError(w, err)
			return
		}
		isNewUser = true
		slog.Info("oauth-login: created new user", "provider", input.Provider, "email", input.Email, "user_id", user.ID)

		// Auto-create default workspace for new OAuth users
		s.autoCreateWorkspace(user)
	} else {
		// Existing user — implicit account link.
		// Block OAuth login if the user has 2FA enabled — OAuth must not bypass 2FA.
		if user.TOTPEnabled {
			slog.Warn("oauth-login: blocked — existing user has 2FA enabled",
				"provider", input.Provider,
				"email", input.Email,
				"user_id", user.ID,
			)
			s.logAuditEventForUser(models.ActionOAuthLoginFailed, r, user.ID, auditMeta(map[string]string{
				"provider": input.Provider,
				"email":    input.Email,
				"reason":   "2fa_enabled",
			}))
			writeError(w, http.StatusForbidden, "forbidden",
				"This account has two-factor authentication enabled. Please sign in with your password and 2FA code, then link your OAuth provider in account settings.")
			return
		}

		// Log the implicit link for audit
		slog.Info("oauth-login: existing user (account link)",
			"provider", input.Provider,
			"email", input.Email,
			"user_id", user.ID,
		)

		// Update avatar if they don't have one
		if user.AvatarURL == "" && input.AvatarURL != "" {
			avatar := input.AvatarURL
			s.store.UpdateUser(user.ID, models.UserUpdate{AvatarURL: &avatar})
		}
	}

	// 6. Create session (30-day TTL for OAuth sessions)
	token, err := s.createAuthSession(w, r, user, 30*24*time.Hour)
	if err != nil {
		return // Error already written by createAuthSession
	}

	// 7. Audit log
	s.logAuditEventForUser(models.ActionOAuthLogin, r, user.ID, auditMeta(map[string]string{
		"provider": input.Provider,
		"email":    input.Email,
		"new_user": boolStr(isNewUser),
	}))

	// 8. Return session info
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user":     sessionUserPayload(user),
		"token":    token,
		"new_user": isNewUser,
	})
}

// --- Admin Plan Endpoint (TASK-431) ---

// handleSetPlan handles POST /api/v1/admin/plan.
// Called by the pad-cloud sidecar to update a user's billing plan
// after Stripe subscription events.
func (s *Server) handleSetPlan(w http.ResponseWriter, r *http.Request) {
	var input struct {
		UserID      string `json:"user_id"`
		Plan        string `json:"plan"`
		ExpiresAt   string `json:"expires_at"`
		CloudSecret string `json:"cloud_secret"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	// 1. Validate cloud secret (or admin auth)
	user := currentUser(r)
	isAdmin := user != nil && user.Role == "admin"
	if !isAdmin {
		if !s.validateCloudSecret(input.CloudSecret, w) {
			return
		}
	}

	// 2. Validate inputs
	if input.UserID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "user_id is required")
		return
	}
	validPlans := map[string]bool{"free": true, "pro": true, "self-hosted": true}
	if !validPlans[input.Plan] {
		writeError(w, http.StatusBadRequest, "bad_request", "plan must be 'free', 'pro', or 'self-hosted'")
		return
	}

	// 3. Verify user exists
	targetUser, err := s.store.GetUser(input.UserID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if targetUser == nil {
		writeError(w, http.StatusNotFound, "not_found", "User not found")
		return
	}

	// 4. Update plan
	oldPlan := targetUser.Plan
	if err := s.store.SetUserPlan(input.UserID, input.Plan, input.ExpiresAt); err != nil {
		writeInternalError(w, err)
		return
	}

	// 5. Audit log
	actorID := ""
	if user != nil {
		actorID = user.ID
	}
	s.logAuditEventForUser(models.ActionPlanChanged, r, actorID, auditMeta(map[string]string{
		"target_user_id": input.UserID,
		"old_plan":       oldPlan,
		"new_plan":       input.Plan,
		"expires_at":     input.ExpiresAt,
	}))

	slog.Info("plan updated", "user_id", input.UserID, "old_plan", oldPlan, "new_plan", input.Plan)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user_id": input.UserID,
		"plan":    input.Plan,
		"ok":      true,
	})
}

// --- Plan Limit Enforcement ---

// enforcePlanLimit checks a workspace-scoped plan limit and writes a 403
// error if the limit is exceeded. Returns true if the operation is allowed.
// In non-cloud mode, always returns true (no limits enforced).
func (s *Server) enforcePlanLimit(w http.ResponseWriter, workspaceID, feature string) bool {
	if !s.cloudMode {
		return true // Self-hosted: no limits
	}

	result, err := s.store.CheckLimit(workspaceID, feature)
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	if !result.Allowed {
		writePlanLimitError(w, result)
		return false
	}
	return true
}

// enforceUserPlanLimit checks a user-scoped plan limit and writes a 403
// error if the limit is exceeded. Returns true if the operation is allowed.
// In non-cloud mode, always returns true (no limits enforced).
func (s *Server) enforceUserPlanLimit(w http.ResponseWriter, userID, feature string) bool {
	if !s.cloudMode {
		return true // Self-hosted: no limits
	}

	result, err := s.store.CheckUserLimit(userID, feature)
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	if !result.Allowed {
		writePlanLimitError(w, result)
		return false
	}
	return true
}

// writePlanLimitError writes a structured 403 response for plan limit violations.
func writePlanLimitError(w http.ResponseWriter, result *store.LimitResult) {
	writeJSON(w, http.StatusForbidden, map[string]interface{}{
		"error":       "plan_limit_exceeded",
		"feature":     result.Feature,
		"limit":       result.Limit,
		"current":     result.Current,
		"plan":        result.Plan,
		"upgrade_url": "/console/billing",
	})
}

// --- Auto-create Workspace (TASK-432) ---

// autoCreateWorkspace creates a default workspace for a new user in cloud mode.
// Called after user creation in register, bootstrap, and oauth-login handlers.
// No-op in self-hosted mode. Errors are logged but don't fail the signup.
func (s *Server) autoCreateWorkspace(user *models.User) {
	if !s.cloudMode {
		return
	}

	name := user.Name + "'s Workspace"
	ws, err := s.store.CreateWorkspace(models.WorkspaceCreate{
		Name:    name,
		OwnerID: user.ID,
	})
	if err != nil {
		slog.Error("auto-create workspace failed", "user_id", user.ID, "error", err)
		return
	}

	// Seed default collections
	if err := s.store.SeedCollectionsFromTemplate(ws.ID, ""); err != nil {
		slog.Warn("auto-create workspace: failed to seed collections", "workspace_id", ws.ID, "error", err)
	}

	// Add user as owner
	_ = s.store.AddWorkspaceMember(ws.ID, user.ID, "owner")

	slog.Info("auto-created default workspace", "user_id", user.ID, "workspace", ws.Slug)
}

// --- Helpers ---

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
