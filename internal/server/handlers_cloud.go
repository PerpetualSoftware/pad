package server

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
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

// cloudAdminPaths are the exact paths where a cloud-secret auth attempt is
// allowed to bypass the normal user-auth / CSRF gates. Defined as an
// explicit whitelist so no future /api/v1/... route accidentally inherits
// the bypass — Codex caught a P0 in the first cut of this change where
// setting X-Cloud-Secret on any path (e.g. GET /api/v1/workspaces)
// bypassed auth globally.
var cloudAdminPaths = map[string]struct{}{
	"/api/v1/admin/plan":                   {},
	"/api/v1/admin/stripe-customer-id":     {},
	"/api/v1/admin/user-by-customer":       {},
	"/api/v1/admin/stripe-event-processed": {},
	"/api/v1/admin/stripe-event-unmark":    {},
	"/api/v1/admin/payment-failed":         {},
}

// isCloudAdminPath returns true if the request targets one of the three
// cloud-sidecar-only admin endpoints. Kept separate from the credential
// check so the auth and CSRF middleware call sites can clearly combine
// "right path" AND "right credential marker" in a single if-statement.
func isCloudAdminPath(path string) bool {
	_, ok := cloudAdminPaths[path]
	return ok
}

// hasCloudSecretMarker returns true if the request carries a sidecar-
// style auth marker: X-Cloud-Secret header or a cloud_secret field in
// the JSON body of a POST/PUT to a cloud admin path. It does NOT check
// the path — callers MUST gate on isCloudAdminPath first. Returning
// true for a request that carries a wrong secret value is safe; the
// handler's validateCloudSecret rejects mismatches.
//
// The body peek is scoped to cloud admin POSTs only so the cost and
// body-replay side-effect are bounded to the few endpoints that need
// it.
//
// The legacy ?cloud_secret= query-param is NOT honored here — query
// values land in access logs, so a log-file compromise became a
// compromise of the cloud trust boundary. Sidecars must send the
// secret in the X-Cloud-Secret header (TASK-656).
func hasCloudSecretMarker(r *http.Request) bool {
	if r.Header.Get("X-Cloud-Secret") != "" {
		return true
	}
	if (r.Method == http.MethodPost || r.Method == http.MethodPut) && r.Body != nil {
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "application/json") && bodyHasCloudSecret(r) {
			return true
		}
	}
	return false
}

// bodyHasCloudSecret peeks at the request body (capped at 64 KB) to see
// whether it's a JSON object with a "cloud_secret" field. Restores the
// body afterwards so downstream handlers can decode it again. A parse
// error or missing field is treated as "no marker" — the auth gate will
// reject the request via the normal auth path.
func bodyHasCloudSecret(r *http.Request) bool {
	const maxPeek = 64 * 1024
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxPeek))
	if err != nil {
		return false
	}
	// Replace r.Body with a buffered reader so handlers can still decode.
	// If the body was larger than maxPeek the tail is lost — acceptable
	// because cloud admin requests are all tiny JSON payloads and the
	// handler-level validateCloudSecret will still reject any caller
	// whose request doesn't match the expected schema.
	r.Body = io.NopCloser(bytes.NewReader(buf))
	var body struct {
		CloudSecret string `json:"cloud_secret"`
	}
	if err := json.Unmarshal(buf, &body); err != nil {
		return false
	}
	return body.CloudSecret != ""
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

	// 6. Reject disabled accounts
	if user.IsDisabled() {
		writeError(w, http.StatusForbidden, "account_disabled", "Your account has been disabled. Contact an administrator.")
		return
	}

	// 7. Create session (30-day TTL for OAuth sessions)
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

	// Ensure user won't be locked out after unlinking. They must retain
	// at least one usable sign-in method: either another linked OAuth
	// provider, or an explicitly-set password. OAuth-only users have a
	// random placeholder hash in password_hash that can't actually be
	// used to log in, which is why we track password_set separately.
	providers := user.GetOAuthProviders()
	hasOtherProvider := false
	for _, p := range providers {
		if p != input.Provider {
			hasOtherProvider = true
			break
		}
	}
	if !hasOtherProvider && !user.HasPassword() {
		writeError(w, http.StatusBadRequest, "bad_request",
			"Cannot unlink your only sign-in method. Link another provider or set a password first.")
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

	// Removing a sign-in method changes the account's auth surface — rotate
	// all sessions so any cookie issued while the provider was linked
	// (possibly via a compromised OAuth account) becomes invalid. The
	// caller keeps their session via a re-issued cookie.
	token, ok := s.rotateSessionsAfterCredentialChange(w, r, user)
	if !ok {
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":       true,
		"provider": input.Provider,
		"token":    token, // for Bearer-only callers
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
	// 1. Validate cloud secret via X-Cloud-Secret header (or admin auth).
	//
	// NOTE: ?cloud_secret= was previously accepted for GET convenience but
	// dropped in TASK-656 — query-param values land in access logs (our
	// StructuredLogger records the full path + query, and any fronting
	// reverse proxy typically logs the same), so a log file compromise
	// became a compromise of the cloud trust boundary. Sidecars must now
	// send the secret in the X-Cloud-Secret header.
	user := currentUser(r)
	isAdmin := user != nil && user.Role == "admin"
	if !isAdmin {
		secret := r.Header.Get("X-Cloud-Secret")
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

// --- Stripe Webhook Idempotency (TASK-696) ---

// handleStripeEventProcessed handles POST /api/v1/admin/stripe-event-processed.
// Called by the pad-cloud sidecar BEFORE processing a Stripe webhook event.
// Pad is the source of truth for which event IDs have been handled, so the
// check survives sidecar restarts (previously held in-memory, lost on crash).
//
// Request body:
//
//	{"event_id": "evt_xxx", "cloud_secret": "..."}
//
// Response:
//
//	{"event_id": "evt_xxx", "already_processed": true|false, "processed_at": "RFC3339"}
//
// The processed_at field is the row's timestamp: the one we just inserted on
// a fresh mark, or the existing row's timestamp on a duplicate. Sidecars
// MUST persist this value and pass it back to /admin/stripe-event-unmark if
// they later need to roll back a failed handler (TASK-736 race protection:
// the unmark call uses (event_id, processed_at) as a composite key, so a
// stale unmark can't delete a fresh marker left by a successful retry).
//
// Semantics: the call is transactional — it atomically records the event and
// tells the caller whether it was new or a duplicate. The caller should skip
// handler logic if already_processed=true.
//
// Retention: records older than 7 days are opportunistically pruned inside
// this handler (~1% of successful inserts trigger a DELETE). Stripe retries
// events for up to 72h, so 7 days gives a safe margin.
func (s *Server) handleStripeEventProcessed(w http.ResponseWriter, r *http.Request) {
	var input struct {
		EventID     string `json:"event_id"`
		CloudSecret string `json:"cloud_secret"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	// 1. Validate cloud secret (body field for POST; handler matches the
	//    existing /admin/stripe-customer-id pattern).
	user := currentUser(r)
	isAdmin := user != nil && user.Role == "admin"
	if !isAdmin {
		if !s.validateCloudSecret(input.CloudSecret, w) {
			return
		}
	}

	// 2. Validate input
	if input.EventID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "event_id is required")
		return
	}
	if !strings.HasPrefix(input.EventID, "evt_") {
		writeError(w, http.StatusBadRequest, "bad_request", "event_id must start with 'evt_'")
		return
	}

	// 3. Atomically record-or-detect-duplicate
	alreadyProcessed, processedAt, err := s.store.MarkStripeEventProcessed(input.EventID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// 4. Opportunistic pruning (1% of calls; keeps the table bounded without
	//    a dedicated goroutine). Failures here are non-fatal — log and continue.
	if store.ShouldPruneStripeEvents() {
		go func(eventID string) {
			// Run in background so we don't block the response. 7-day retention
			// covers Stripe's 72h retry window with a generous safety margin.
			removed, perr := s.store.PruneStripeProcessedEvents(7 * 24 * time.Hour)
			if perr != nil {
				slog.Warn("prune stripe_processed_events failed", "error", perr, "trigger_event_id", eventID)
				return
			}
			if removed > 0 {
				slog.Info("pruned stripe_processed_events", "removed", removed)
			}
		}(input.EventID)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"event_id":          input.EventID,
		"already_processed": alreadyProcessed,
		"processed_at":      processedAt,
	})
}

// handleStripeEventUnmark handles POST /api/v1/admin/stripe-event-unmark.
// Called by the pad-cloud sidecar as a best-effort rollback when a webhook
// handler fails AFTER /stripe-event-processed marked the event as seen
// (TASK-736). Without this, Stripe's retries of the same event are
// short-circuited as duplicates and the un-applied side effect has no
// automated recovery — operators have to manually DELETE the row and
// replay from the Stripe dashboard.
//
// Request body:
//
//	{"event_id": "evt_xxx", "processed_at": "RFC3339", "cloud_secret": "..."}
//
// Response:
//
//	{"event_id": "evt_xxx", "unmarked": true|false}
//
// Semantics: the delete is scoped to the specific (event_id, processed_at)
// row originally inserted by /stripe-event-processed. processed_at is
// mandatory — without it, a stale unmark from an earlier failed attempt
// could silently delete the fresh marker left by a successful retry and
// reopen the retry window after the event has been handled. With it, a
// stale token simply doesn't match and the unmark becomes a no-op.
//
// Idempotent: unmarked=true means we actually deleted a row; unmarked=false
// means nothing matched (missing row OR timestamp mismatch — both safe).
// Either outcome is 200.
//
// Audit: every call is persisted to the audit log (ActionStripeEventUnmarked)
// with the event_id and whether a row actually went away. The endpoint can
// reopen Stripe retry windows, so a queryable audit trail is required —
// slog alone would let a compromised cloud_secret spam unmarks invisibly.
//
// Auth: same cloud_secret gate as /stripe-event-processed. Admins hitting
// the endpoint with a browser session can also unmark (useful for manual
// reconciliation from the admin UI), mirroring the pattern of the
// other cloud admin endpoints.
func (s *Server) handleStripeEventUnmark(w http.ResponseWriter, r *http.Request) {
	var input struct {
		EventID     string `json:"event_id"`
		ProcessedAt string `json:"processed_at"`
		CloudSecret string `json:"cloud_secret"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	// 1. Validate cloud secret (body field for POST; admins also allowed).
	user := currentUser(r)
	isAdmin := user != nil && user.Role == "admin"
	if !isAdmin {
		if !s.validateCloudSecret(input.CloudSecret, w) {
			return
		}
	}

	// 2. Validate input — must match the shape /stripe-event-processed accepts
	//    so that an operator copying an event ID from the Stripe dashboard
	//    gets the same format validation across both endpoints. processed_at
	//    is mandatory for race protection (see handler godoc).
	if input.EventID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "event_id is required")
		return
	}
	if !strings.HasPrefix(input.EventID, "evt_") {
		writeError(w, http.StatusBadRequest, "bad_request", "event_id must start with 'evt_'")
		return
	}
	if input.ProcessedAt == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "processed_at is required (pass the value returned by /stripe-event-processed)")
		return
	}

	// 3. Delete the row only if (event_id, processed_at) matches.
	unmarked, err := s.store.UnmarkStripeEventProcessed(input.EventID, input.ProcessedAt)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// 4. Audit log — persisted so admins can see who/when reopened retry
	//    windows. actor_is_admin distinguishes "manual operator action" from
	//    "sidecar rollback" so dashboards can filter accordingly.
	actorID := ""
	if user != nil {
		actorID = user.ID
	}
	s.logAuditEventForUser(models.ActionStripeEventUnmarked, r, actorID, auditMeta(map[string]string{
		"event_id":       input.EventID,
		"processed_at":   input.ProcessedAt,
		"unmarked":       boolToString(unmarked),
		"actor_is_admin": boolToString(isAdmin),
	}))

	if unmarked {
		slog.Info("unmarked stripe event for replay", "event_id", input.EventID, "actor_is_admin", isAdmin)
	} else {
		slog.Info("unmark stripe event: no matching row (idempotent)", "event_id", input.EventID, "actor_is_admin", isAdmin)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"event_id": input.EventID,
		"unmarked": unmarked,
	})
}

// handlePaymentFailed handles POST /api/v1/admin/payment-failed.
// Called by the pad-cloud sidecar when it receives an invoice.payment_failed
// webhook from Stripe. Pad owns the user→email mapping and the Maileroo
// integration, so the sidecar forwards the invoice metadata here and pad
// does the actual notification.
//
// Request body:
//
//	{
//	  "stripe_customer_id": "cus_...",
//	  "amount_display":     "$10.00",            // optional, pre-formatted
//	  "next_retry_display": "April 30, 2026",    // optional, pre-formatted
//	  "cloud_secret":       "..."
//	}
//
// Response:
//
//	{"stripe_customer_id": "cus_...", "email_sent": true|false, "reason": "..."}
//
// The handler returns 200 even when no email is sent (e.g. unknown
// customer, email provider not configured, user has no stored email) —
// the sidecar should treat those as non-fatal so Stripe does not retry
// the webhook. Reasons:
//   - "sent"              — email dispatched
//   - "no_customer"       — no user matches stripe_customer_id
//   - "no_email_address"  — user exists but has no email on file
//   - "email_not_configured" — no Maileroo key / base URL
//   - "send_failed"       — Maileroo returned an error (details in logs)
//
// The email is transactional (dunning) and has no unsubscribe link by
// design; users who want to stop receiving dunning mail can update
// their card or cancel the subscription.
func (s *Server) handlePaymentFailed(w http.ResponseWriter, r *http.Request) {
	var input struct {
		StripeCustomerID string `json:"stripe_customer_id"`
		AmountDisplay    string `json:"amount_display"`
		NextRetryDisplay string `json:"next_retry_display"`
		CloudSecret      string `json:"cloud_secret"`
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

	// 2. Validate input
	if input.StripeCustomerID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "stripe_customer_id is required")
		return
	}
	if !strings.HasPrefix(input.StripeCustomerID, "cus_") {
		writeError(w, http.StatusBadRequest, "bad_request", "stripe_customer_id must start with 'cus_'")
		return
	}

	// 3. Look up user. Missing customer is a non-error — Stripe could be
	//    firing a webhook for an account that was deleted on our side;
	//    returning 200 tells the sidecar not to rollback/retry.
	targetUser, err := s.store.GetUserByStripeCustomerID(input.StripeCustomerID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// auditAndRespond records a single audit row for every outcome of the
	// payment-failed flow — including the skip paths before we even try to
	// send — and writes the JSON response. Keeping the audit in one place
	// guarantees that no-customer / no-email / email-not-configured /
	// send-failed all leave a durable trail that /audit-log can surface
	// during dunning reconciliation. UserID attaches to the target user
	// when we have one so /audit-log?user=<id> finds the event; when the
	// customer is unknown, the row still exists, just unfiltered by user.
	auditAndRespond := func(targetUserID, reason string, sent bool) {
		meta := map[string]string{
			"stripe_customer_id": input.StripeCustomerID,
			"amount_display":     input.AmountDisplay,
			"next_retry_display": input.NextRetryDisplay,
			"reason":             reason,
			"sent":               boolToString(sent),
		}
		// When an authenticated admin hits this endpoint manually (instead
		// of the sidecar with cloud_secret), record which admin did it.
		// logAuditEventForUser only carries one UserID field, which we use
		// for the TARGET user so /audit-log?user=<id> surfaces the event —
		// the admin's own ID has to live in metadata. Sidecar calls have
		// no authenticated user, so admin_actor_id is absent for those.
		if isAdmin && user != nil {
			meta["admin_actor_id"] = user.ID
		}
		s.logAuditEventForUser(models.ActionPaymentFailedEmailSent, r, targetUserID, auditMeta(meta))
		writeJSON(w, http.StatusOK, map[string]any{
			"stripe_customer_id": input.StripeCustomerID,
			"email_sent":         sent,
			"reason":             reason,
		})
	}

	if targetUser == nil {
		slog.Info("payment-failed: no user for customer",
			"customer_id", input.StripeCustomerID)
		auditAndRespond("", "no_customer", false)
		return
	}
	if targetUser.Email == "" {
		slog.Info("payment-failed: user has no email on file",
			"user_id", targetUser.ID)
		auditAndRespond(targetUser.ID, "no_email_address", false)
		return
	}

	// 4. Verify email provider is wired up. Same pattern as password reset
	//    (handlers_auth.go) — no panic if the operator hasn't configured
	//    Maileroo, just log and skip.
	if s.email == nil || s.baseURL == "" {
		slog.Warn("payment-failed: email provider not configured; skipping send",
			"user_id", targetUser.ID)
		auditAndRespond(targetUser.ID, "email_not_configured", false)
		return
	}

	billingPortalURL := strings.TrimRight(s.baseURL, "/") + "/billing/portal"
	sendErr := s.email.SendPaymentFailed(r.Context(),
		targetUser.Email, targetUser.Name,
		input.AmountDisplay, input.NextRetryDisplay, billingPortalURL)

	if sendErr != nil {
		slog.Error("payment-failed: email send failed",
			"user_id", targetUser.ID, "error", sendErr)
		auditAndRespond(targetUser.ID, "send_failed", false)
		return
	}

	slog.Info("payment-failed email sent",
		"user_id", targetUser.ID, "customer_id", input.StripeCustomerID)
	auditAndRespond(targetUser.ID, "sent", true)
}

// boolToString converts a Go bool to the string representation used in
// audit metadata. Kept local to this file because audit metadata is
// string-typed (JSON object of string→string), so we need the literal
// "true"/"false" spelling rather than fmt.Sprintf's lower-case default
// (which happens to match but is easy to misread).
func boolToString(b bool) string {
	if b {
		return "true"
	}
	return "false"
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

	// Seed default collections with the startup starter pack. Cloud signups
	// are implicit workspace creations, so they should get the same curated
	// starter conventions/playbooks that `pad init` produces.
	if err := s.store.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		slog.Warn("auto-create workspace: failed to seed collections", "workspace_id", ws.ID, "error", err)
	}

	// Add user as owner
	_ = s.store.AddWorkspaceMember(ws.ID, user.ID, "owner")

	slog.Info("auto-created default workspace", "user_id", user.ID, "workspace", ws.Slug)
}

// --- Helpers ---
