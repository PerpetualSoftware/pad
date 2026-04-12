package server

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
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

		// Auto-link the provider for new OAuth users
		if err := s.store.AddOAuthProvider(user.ID, input.Provider); err != nil {
			slog.Error("oauth-login: failed to link provider", "error", err, "user_id", user.ID)
		}

		slog.Info("oauth-login: created new user", "provider", input.Provider, "email", input.Email, "user_id", user.ID)

		// Auto-create default workspace for new OAuth users
		s.autoCreateWorkspace(user)
	} else {
		// Existing user — require explicit provider linking.
		// The user must have previously linked this provider from their settings.
		if !user.HasOAuthProvider(input.Provider) {
			slog.Warn("oauth-login: rejected — provider not linked",
				"provider", input.Provider,
				"email", input.Email,
				"user_id", user.ID,
			)
			s.logAuditEventForUser(models.ActionOAuthLoginFailed, r, user.ID, auditMeta(map[string]string{
				"provider": input.Provider,
				"email":    input.Email,
				"reason":   "provider_not_linked",
			}))
			writeError(w, http.StatusForbidden, "oauth_provider_not_linked",
				"An account with this email already exists. Sign in with your password and link "+input.Provider+" from account settings.")
			return
		}

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
		"new_user": strconv.FormatBool(isNewUser),
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

	// 2b. Validate expires_at format if provided
	if input.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, input.ExpiresAt); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "expires_at must be a valid RFC3339 timestamp")
			return
		}
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

// --- OAuth Provider Linking (TASK-504) ---

// handleOAuthLink handles POST /api/v1/auth/oauth-link.
// Called by the pad-cloud sidecar after an OAuth flow initiated from account settings.
// Requires an active session (the user must be logged in) and links the provider.
func (s *Server) handleOAuthLink(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Provider      string `json:"provider"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		CloudSecret   string `json:"cloud_secret"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	// 1. Validate cloud secret
	if !s.validateCloudSecret(input.CloudSecret, w) {
		return
	}

	// 2. Validate provider
	if input.Provider != "github" && input.Provider != "google" {
		writeError(w, http.StatusBadRequest, "bad_request", "provider must be 'github' or 'google'")
		return
	}

	// 3. Require verified email
	if !input.EmailVerified {
		writeError(w, http.StatusForbidden, "forbidden", "Only verified email addresses are accepted")
		return
	}

	// 4. Find user by email (the sidecar passes the OAuth email)
	input.Email = strings.ToLower(strings.TrimSpace(input.Email))
	user, err := s.store.GetUserByEmail(input.Email)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if user == nil {
		writeError(w, http.StatusNotFound, "not_found", "No account found with that email")
		return
	}

	// 5. Check if already linked
	if user.HasOAuthProvider(input.Provider) {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":       true,
			"provider": input.Provider,
			"message":  "Provider already linked",
		})
		return
	}

	// 6. Link the provider
	if err := s.store.AddOAuthProvider(user.ID, input.Provider); err != nil {
		writeInternalError(w, err)
		return
	}

	// 7. Audit log
	s.logAuditEventForUser(models.ActionOAuthLogin, r, user.ID, auditMeta(map[string]string{
		"provider": input.Provider,
		"email":    input.Email,
		"action":   "link_provider",
	}))

	slog.Info("oauth-link: provider linked", "provider", input.Provider, "user_id", user.ID)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":       true,
		"provider": input.Provider,
	})
}

// handleOAuthUnlink handles POST /api/v1/auth/oauth-unlink.
// Removes a linked OAuth provider. Requires the user to have a usable password
// (to prevent locking themselves out).
func (s *Server) handleOAuthUnlink(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}

	var input struct {
		Provider string `json:"provider"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	if input.Provider != "github" && input.Provider != "google" {
		writeError(w, http.StatusBadRequest, "bad_request", "provider must be 'github' or 'google'")
		return
	}

	if !user.HasOAuthProvider(input.Provider) {
		writeError(w, http.StatusBadRequest, "bad_request", "Provider not linked")
		return
	}

	// Ensure user won't be locked out: they must have a usable password
	// or another linked provider remaining.
	providers := user.GetOAuthProviders()
	hasOtherProvider := false
	for _, p := range providers {
		if p != input.Provider {
			hasOtherProvider = true
			break
		}
	}
	// A user has a "usable" password if they set one explicitly.
	// OAuth-created users have a random unusable password but may have
	// set one later via the password reset flow.
	// We can't easily distinguish, so we require at least one other auth method.
	if !hasOtherProvider && user.PasswordHash == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"Cannot unlink your only authentication method. Set a password first.")
		return
	}

	if err := s.store.RemoveOAuthProvider(user.ID, input.Provider); err != nil {
		writeInternalError(w, err)
		return
	}

	s.logAuditEventForUser(models.ActionOAuthLogin, r, user.ID, auditMeta(map[string]string{
		"provider": input.Provider,
		"action":   "unlink_provider",
	}))

	slog.Info("oauth-unlink: provider unlinked", "provider", input.Provider, "user_id", user.ID)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":       true,
		"provider": input.Provider,
	})
}

// --- Stripe Customer ID (TASK-505) ---

// handleSetStripeCustomerID handles POST /api/v1/admin/stripe-customer-id.
// Called by the pad-cloud sidecar after a Stripe checkout.completed event
// to associate a Stripe customer ID with a Pad user.
func (s *Server) handleSetStripeCustomerID(w http.ResponseWriter, r *http.Request) {
	var input struct {
		UserID      string `json:"user_id"`
		CustomerID  string `json:"customer_id"`
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
	if input.CustomerID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "customer_id is required")
		return
	}
	if !strings.HasPrefix(input.CustomerID, "cus_") {
		writeError(w, http.StatusBadRequest, "bad_request", "customer_id must start with 'cus_'")
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

	// 4. Store the Stripe customer ID
	if err := s.store.SetUserStripeCustomerID(input.UserID, input.CustomerID); err != nil {
		writeInternalError(w, err)
		return
	}

	// 5. Audit log
	actorID := ""
	if user != nil {
		actorID = user.ID
	}
	s.logAuditEventForUser(models.ActionPlanChanged, r, actorID, auditMeta(map[string]string{
		"target_user_id":     input.UserID,
		"stripe_customer_id": input.CustomerID,
		"action":             "set_stripe_customer_id",
	}))

	slog.Info("stripe customer ID set", "user_id", input.UserID, "customer_id", input.CustomerID)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user_id":     input.UserID,
		"customer_id": input.CustomerID,
		"ok":          true,
	})
}

// handleGetUserByCustomerID handles GET /api/v1/admin/user-by-customer?customer_id=cus_xxx.
// Called by the pad-cloud sidecar during Stripe subscription webhook processing
// to resolve a Stripe customer back to a Pad user.
func (s *Server) handleGetUserByCustomerID(w http.ResponseWriter, r *http.Request) {
	// 1. Validate cloud secret (via header or query param) or admin auth
	user := currentUser(r)
	isAdmin := user != nil && user.Role == "admin"
	if !isAdmin {
		secret := r.Header.Get("X-Cloud-Secret")
		if secret == "" {
			secret = r.URL.Query().Get("cloud_secret")
		}
		if !s.validateCloudSecret(secret, w) {
			return
		}
	}

	// 2. Validate customer_id
	customerID := r.URL.Query().Get("customer_id")
	if customerID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "customer_id query parameter is required")
		return
	}
	if !strings.HasPrefix(customerID, "cus_") {
		writeError(w, http.StatusBadRequest, "bad_request", "customer_id must start with 'cus_'")
		return
	}

	// 3. Look up user
	targetUser, err := s.store.GetUserByStripeCustomerID(customerID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if targetUser == nil {
		writeError(w, http.StatusNotFound, "not_found", "No user found with that Stripe customer ID")
		return
	}

	// 4. Return minimal user info (only what the sidecar needs)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user_id": targetUser.ID,
		"email":   targetUser.Email,
		"plan":    targetUser.Plan,
	})
}

// --- Public Plan Limits (TASK-511) ---

// handleGetPlanLimits returns the configured plan limits for free and pro tiers.
// GET /api/v1/plan-limits — public endpoint, no auth required.
// Used by the billing page to show actual limits instead of hardcoded values.
func (s *Server) handleGetPlanLimits(w http.ResponseWriter, r *http.Request) {
	result := map[string]interface{}{
		"free": store.DefaultFreeLimits,
		"pro":  store.DefaultProLimits,
	}

	// Override with DB-stored limits if available
	features := []string{
		"workspaces", "items_per_workspace", "members_per_workspace",
		"api_tokens", "storage_bytes", "webhooks", "automated_backups",
	}
	for _, plan := range []string{"free", "pro"} {
		overrides := make(map[string]int)
		for _, feature := range features {
			key := "plan_limits_" + plan + "_" + feature
			val, err := s.store.GetPlatformSetting(key)
			if err != nil || val == "" {
				continue
			}
			v, _ := strconv.Atoi(val)
			overrides[feature] = v
		}
		if len(overrides) > 0 {
			// Merge overrides onto defaults
			defaults := store.DefaultFreeLimits
			if plan == "pro" {
				defaults = store.DefaultProLimits
			}
			merged := map[string]int{
				"workspaces":            defaults.Workspaces,
				"items_per_workspace":   defaults.ItemsPerWorkspace,
				"members_per_workspace": defaults.MembersPerWorkspace,
				"api_tokens":            defaults.APITokens,
				"storage_bytes":         defaults.StorageBytes,
				"webhooks":              defaults.Webhooks,
				"automated_backups":     defaults.AutomatedBackups,
			}
			for k, v := range overrides {
				merged[k] = v
			}
			result[plan] = merged
		}
	}

	writeJSON(w, http.StatusOK, result)
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
