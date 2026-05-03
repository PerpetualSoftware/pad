package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

const (
	webSessionTTL = 7 * 24 * time.Hour  // 7 days for web sessions
	cliSessionTTL = 30 * 24 * time.Hour // 30 days for CLI tokens

	authMethodPassword    = "password"
	authMethodCloud       = "cloud"
	setupMethodLocalCLI   = "local_cli"
	setupMethodDockerExec = "docker_exec"
	setupMethodCloud      = "cloud"
)

var emailRegexp = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

// sessionCookieName returns the session cookie name. When running over TLS
// (secureCookies=true), the __Host- prefix is used to prevent subdomain
// cookie injection attacks.
func sessionCookieName(secure bool) string {
	if secure {
		return "__Host-pad_session"
	}
	return "pad_session"
}

// csrfCookieName returns the CSRF cookie name. Uses the same __Host- prefix
// strategy as the session cookie.
func csrfCookieName(secure bool) string {
	if secure {
		return "__Host-pad_csrf"
	}
	return "pad_csrf"
}

func sessionUserPayload(user *models.User) map[string]interface{} {
	if user == nil {
		return nil
	}
	return map[string]interface{}{
		"id":           user.ID,
		"email":        user.Email,
		"username":     user.Username,
		"name":         user.Name,
		"role":         user.Role,
		"totp_enabled": user.TOTPEnabled,
		"plan":         user.Plan,
	}
}

// handleCheckUsername checks if a username is available for registration.
// GET /api/v1/auth/check-username?username=foo
func (s *Server) handleCheckUsername(w http.ResponseWriter, r *http.Request) {
	username := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("username")))

	if username == "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"available": false,
			"reason":    "invalid",
			"message":   "Username is required",
		})
		return
	}

	// Format/reserved validation
	if err := ValidateUsername(username); err != nil {
		reason := "invalid"
		if IsReservedUsername(username) {
			reason = "reserved"
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"available": false,
			"reason":    reason,
			"message":   err.Error(),
		})
		return
	}

	// Uniqueness check
	existing, err := s.store.GetUserByUsername(username)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if existing != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"available": false,
			"reason":    "taken",
			"message":   "Username is already taken",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"available": true,
		"reason":    nil,
		"message":   nil,
	})
}

func (s *Server) setupStatePayload(setupMethod string) map[string]interface{} {
	return map[string]interface{}{
		"authenticated":  false,
		"setup_required": true,
		"setup_method":   setupMethod,
		"auth_method":    authMethodPassword,
		"cloud_mode":     s.cloudMode,
		"mcp_public_url": s.mcpPublicURL,
	}
}

func (s *Server) sessionStatePayload(authenticated bool, user *models.User) map[string]interface{} {
	// mcp_public_url is the canonical URL clients paste into their MCP-capable
	// agent (e.g. "https://mcp.getpad.dev"). Empty string when PAD_MCP_PUBLIC_URL
	// is unset — the web UI uses presence/absence as the gate that drives the
	// connect banner mode (Remote MCP vs CLI install). Always emitted, never
	// omitted, so the frontend can rely on a string value.
	payload := map[string]interface{}{
		"authenticated":  authenticated,
		"setup_required": false,
		"auth_method":    authMethodPassword,
		"cloud_mode":     s.cloudMode,
		"mcp_public_url": s.mcpPublicURL,
	}
	if authenticated {
		payload["user"] = sessionUserPayload(user)
	}
	return payload
}

// requestIsLoopback reports whether the request came from a local CLI
// running on the same machine as the Pad server. The check is intentionally
// strict: it must be satisfiable ONLY by a direct loopback-TCP connection,
// never by a request relayed through a proxy (local or remote).
//
// Two conditions must hold:
//
//  1. The untampered TCP peer (captured by CapturePeerAddr before any
//     RealIP rewrite) is a loopback address. This defeats X-Forwarded-For
//     spoofing from a non-loopback attacker — TrustedProxyRealIP already
//     ignores XFF from untrusted peers, but we re-check the raw peer so
//     a proxy misconfigured to trust 127.0.0.0/8 still can't be fooled
//     into rewriting the peer itself.
//
//  2. Neither X-Forwarded-For nor X-Real-IP is set. A legitimate local CLI
//     talking directly to the Pad port never sets these headers. A reverse
//     proxy forwarding public traffic always does — so this rejects the
//     regression Codex flagged on PR #175: a local Caddy/nginx proxying
//     public traffic to Pad on 127.0.0.1 would otherwise make every
//     request look loopback and reopen the bootstrap gate.
//
// The rule denies some unusual legitimate setups (e.g. a local proxy that
// deliberately strips forwarding headers) in exchange for a simple,
// sound invariant. Operators in that narrow case can call
// `pad auth setup` from the host CLI instead of through their proxy.
// isPlausibleEmail is a cheap pre-filter used to decide whether an email
// is worth creating a per-email rate-limiter bucket for. NOT a full RFC
// 5322 validator — it only rejects the two easy ways an attacker could
// flood the limiter's bucket map: (1) excessively long strings, (2)
// strings with no '@' at all. Anything shape-like-an-email passes and
// the real validation happens in the store's password check.
func isPlausibleEmail(s string) bool {
	// RFC 5321 §4.5.3.1.3 caps the full address at 254 octets.
	if s == "" || len(s) > 254 {
		return false
	}
	at := strings.IndexByte(s, '@')
	// Require an '@' that isn't at position 0 or the last char, so
	// neither side of the address is empty.
	return at > 0 && at < len(s)-1
}

func requestIsLoopback(r *http.Request) bool {
	// (2) Reject any proxied request.
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" {
		return false
	}
	// (1) The TCP peer must be a loopback address.
	peer := rawPeerAddr(r)
	host := peer
	if parsedHost, _, err := net.SplitHostPort(peer); err == nil {
		host = parsedHost
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// validateSessionCookie validates a session cookie including session binding
// (User-Agent check). Returns the user if valid, nil otherwise. This must be
// used instead of calling ValidateSession directly to ensure binding is enforced.
func (s *Server) validateSessionCookie(r *http.Request) *models.User {
	cookie, err := r.Cookie(sessionCookieName(s.secureCookies))
	if err != nil {
		// Fallback: check the unprefixed name for sessions created before the upgrade
		cookie, err = r.Cookie("pad_session")
		if err != nil {
			return nil
		}
	}
	session, _ := s.store.ValidateSession(cookie.Value)
	if session == nil || session.User == nil {
		return nil
	}
	// Session binding: reject if User-Agent has changed
	if session.UAHash != "" && sha256hex(r.UserAgent()) != session.UAHash {
		return nil
	}
	return session.User
}

// rotateSessionsAfterCredentialChange invalidates every existing session
// for the user (forcing sign-out on all other devices) and then re-issues
// a fresh session for the current request so the caller stays logged in.
// Call this after any action that changes the credentials or auth surface
// tied to the account: password change, TOTP disable, OAuth provider
// unlink, etc. Without it a stolen cookie stays valid forever — defeating
// the point of letting a user "kick everyone else out" by rotating their
// password.
//
// Sets a fresh session cookie on the response (for browser callers) AND
// returns the new token string (for CLI / API callers using
// Authorization: Bearer padsess_… who never read cookies). Handlers
// should embed the returned token in their response body so both
// transport styles stay authenticated.
//
// On DeleteUserSessions error we log and continue; on CreateSession
// error we write a 500 response and return ok=false — the caller should
// return immediately.
func (s *Server) rotateSessionsAfterCredentialChange(w http.ResponseWriter, r *http.Request, user *models.User) (string, bool) {
	if err := s.store.DeleteUserSessions(user.ID); err != nil {
		// Best-effort: even if deletion fails we must still mint a new
		// session for the caller, but log loudly so the operator knows
		// stale cookies may persist until expiry.
		slog.Error("failed to invalidate sessions after credential change",
			"user_id", user.ID, "error", err)
	}

	token, err := s.store.CreateSession(user.ID, "web", clientIP(r), r.UserAgent(), webSessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error",
			"Credentials updated but failed to refresh session. Please sign in again.")
		return "", false
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName(s.secureCookies),
		Value:    token,
		Path:     "/",
		MaxAge:   int(webSessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	setCSRFCookie(w, int(webSessionTTL.Seconds()), s.secureCookies)
	return token, true
}

func (s *Server) createAuthSession(w http.ResponseWriter, r *http.Request, user *models.User, ttl time.Duration) (string, error) {
	token, err := s.store.CreateSession(user.ID, "web", clientIP(r), r.UserAgent(), ttl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to create session")
		return "", err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName(s.secureCookies),
		Value:    token,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})

	// Set CSRF cookie alongside the session cookie
	setCSRFCookie(w, int(ttl.Seconds()), s.secureCookies)

	return token, nil
}

// handleBootstrap creates the first admin account for a fresh instance.
// It is only allowed from loopback-local requests so setup must happen
// on the server host or from inside the container.
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if s.cloudMode {
		// Allow bootstrap in cloud mode ONLY when no users exist yet.
		// A fresh cloud instance needs at least one admin before OAuth can work.
		count, err := s.store.UserCount()
		if err != nil || count > 0 {
			writeError(w, http.StatusForbidden, "forbidden", "Bootstrap is disabled in cloud mode — users register via OAuth or invitation")
			return
		}
	}
	if !requestIsLoopback(r) {
		writeError(w, http.StatusForbidden, "forbidden", "Bootstrap is only allowed from localhost on the server host")
		return
	}

	var input struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	input.Email = strings.TrimSpace(input.Email)
	input.Name = strings.TrimSpace(input.Name)

	if input.Email == "" || !emailRegexp.MatchString(input.Email) {
		writeError(w, http.StatusBadRequest, "validation_error", "Valid email is required")
		return
	}
	if input.Name == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "Name is required")
		return
	}
	if err := validatePasswordStrength(input.Password, input.Email, input.Name); err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}

	count, err := s.store.UserCount()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check user count")
		return
	}
	if count > 0 {
		writeError(w, http.StatusConflict, "conflict", "This Pad instance has already been initialized")
		return
	}

	existing, err := s.store.GetUserByEmail(input.Email)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check email")
		return
	}
	if existing != nil {
		writeError(w, http.StatusConflict, "conflict", "A user with this email already exists")
		return
	}

	// Auto-generate username from name (D1: no prompt for bootstrap)
	username, err := s.store.EnsureUniqueUsername(store.GenerateUsername(input.Name, input.Email))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to generate username")
		return
	}

	user, err := s.store.CreateUser(models.UserCreate{
		Email:    input.Email,
		Username: username,
		Name:     input.Name,
		Password: input.Password,
		Role:     "admin",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to create user")
		return
	}

	token, err := s.createAuthSession(w, r, user, cliSessionTTL)
	if err != nil {
		return
	}

	s.logAuditEventForUser(models.ActionBootstrap, r, user.ID, auditMeta(map[string]string{"email": user.Email}))

	// Auto-create default workspace in cloud mode
	s.autoCreateWorkspace(user)

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"user":  sessionUserPayload(user),
		"token": token,
	})
}

// handleRegister creates a new user account.
// Registration is restricted to admins or users with a valid invitation code
// so invitees can create an account via the /join/[code] flow.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email          string `json:"email"`
		Username       string `json:"username"`
		Name           string `json:"name"`
		Password       string `json:"password"`
		InvitationCode string `json:"invitation_code"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	// Validate
	input.Email = strings.TrimSpace(input.Email)
	input.Username = strings.TrimSpace(strings.ToLower(input.Username))
	input.Name = strings.TrimSpace(input.Name)
	input.InvitationCode = strings.TrimSpace(input.InvitationCode)

	if input.Email == "" || !emailRegexp.MatchString(input.Email) {
		writeError(w, http.StatusBadRequest, "validation_error", "Valid email is required")
		return
	}
	if input.Name == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "Name is required")
		return
	}
	if err := validatePasswordStrength(input.Password, input.Email, input.Name, input.Username); err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}

	count, err := s.store.UserCount()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check user count")
		return
	}

	// Validate invitation code if provided (look it up before the auth gate
	// so we can give a clear error for invalid codes).
	var invitation *models.WorkspaceInvitation
	if input.InvitationCode != "" {
		inv, err := s.store.GetInvitationByCode(input.InvitationCode)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to validate invitation")
			return
		}
		if inv == nil {
			writeError(w, http.StatusBadRequest, "invalid_invitation", "Invalid or expired invitation code")
			return
		}
		if inv.IsExpired() {
			// Distinct status from the "not found" case so the UI can show
			// a useful message ("ask the inviter to send a new one") rather
			// than a generic retry-the-code prompt.
			writeError(w, http.StatusGone, "expired", "This invitation has expired. Ask the inviter to send a new one.")
			return
		}
		// An invitation is bound to the email it was sent to. If the signup
		// form supplies a different address, the attacker probably intercepted
		// the link — reject before creating the account. Case-insensitive per
		// RFC 5321 §2.4 (local-parts are technically case-sensitive but mail
		// providers universally normalize them; EqualFold matches the store's
		// own ToLower() normalization).
		if !strings.EqualFold(strings.TrimSpace(input.Email), inv.Email) {
			writeError(w, http.StatusForbidden, "invitation_email_mismatch",
				"This invitation was sent to a different email address. Sign in or register with the invited address.")
			return
		}
		invitation = inv
	}

	if count == 0 {
		writeError(w, http.StatusForbidden, "forbidden", "This Pad instance must be initialized with pad auth setup")
		return
	}

	// When users exist, allow registration if:
	// 1. The requester is an admin, OR
	// 2. A valid invitation code was provided
	if invitation == nil {
		reqUser := currentUser(r)
		if reqUser == nil || reqUser.Role != "admin" {
			writeError(w, http.StatusForbidden, "forbidden", "Registration is restricted")
			return
		}
	}

	// Check for duplicate email
	existing, err := s.store.GetUserByEmail(input.Email)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check email")
		return
	}
	if existing != nil {
		writeError(w, http.StatusConflict, "conflict", "A user with this email already exists")
		return
	}

	// Username: validate if provided, auto-generate if not
	if input.Username != "" {
		if err := ValidateUsername(input.Username); err != nil {
			writeError(w, http.StatusBadRequest, "validation_error", err.Error())
			return
		}
		existingUser, err := s.store.GetUserByUsername(input.Username)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check username")
			return
		}
		if existingUser != nil {
			writeError(w, http.StatusConflict, "conflict", "Username is already taken")
			return
		}
	} else {
		candidate := store.GenerateUsername(input.Name, input.Email)
		unique, err := s.store.EnsureUniqueUsername(candidate)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to generate username")
			return
		}
		input.Username = unique
	}

	// Create user
	user, err := s.store.CreateUser(models.UserCreate{
		Email:    input.Email,
		Username: input.Username,
		Name:     input.Name,
		Password: input.Password,
		Role:     "member",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to create user")
		return
	}

	// If registering via invitation, automatically add the user to the
	// workspace and mark the invitation as accepted.
	if invitation != nil {
		_ = s.store.AddWorkspaceMember(invitation.WorkspaceID, user.ID, invitation.Role)
		_ = s.store.AcceptInvitation(invitation.ID)
	}

	token, err := s.createAuthSession(w, r, user, webSessionTTL)
	if err != nil {
		return
	}

	s.logAuditEventForUser(models.ActionRegister, r, user.ID, auditMeta(map[string]string{"email": user.Email}))

	// Auto-create default workspace in cloud mode
	s.autoCreateWorkspace(user)

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"user":  sessionUserPayload(user),
		"token": token,
	})
}

// handleLogin validates email/password and creates a session.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// If no users exist, no login needed
	count, err := s.store.UserCount()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check user count")
		return
	}
	if count == 0 {
		writeError(w, http.StatusConflict, "setup_required", "This Pad instance must be initialized with pad auth setup")
		return
	}

	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	// Per-email rate limit: catches credential spraying from a botnet that
	// evades the per-IP limit by rotating source addresses. 10 attempts/hour
	// per lowercased email address. The limiter is consumed on every attempt
	// (success or failure) so an attacker can't use successful guesses as a
	// "reset" — but a legitimate user who remembers their password on try 1
	// or 2 will never notice the limit.
	//
	// Only create a bucket for syntactically plausible emails. Inserting
	// every attacker-supplied string would let a distributed attacker grow
	// the bucket map without bound (retention = 2h), which is a memory-DoS
	// vector — so we pre-filter by RFC 5321 max length (254) and require
	// at least an '@'. Invalid input still gets the ordinary 401 from the
	// password check below, just without producing a new limiter entry.
	if s.rateLimiters != nil && s.rateLimiters.AuthEmail != nil {
		emailKey := strings.ToLower(strings.TrimSpace(input.Email))
		if isPlausibleEmail(emailKey) {
			limiter := s.rateLimiters.AuthEmail.getLimiter(emailKey)
			if !limiter.Allow() {
				slog.Warn("rate limited", "email", emailKey, "limiter", "auth_email")
				// Audit even the blocked attempt so an admin can see the
				// sprayed account in the log.
				s.logAuditEvent(models.ActionLoginFailed, r, auditMeta(map[string]string{
					"email":  input.Email,
					"reason": "email_rate_limited",
				}))
				writeRateLimitResponse(w, s.rateLimiters.AuthEmail.config)
				return
			}
		}
	}

	user, err := s.store.ValidatePassword(input.Email, input.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Authentication failed")
		return
	}
	if user == nil {
		// Slow down brute force attempts
		time.Sleep(500 * time.Millisecond)
		s.logAuditEvent(models.ActionLoginFailed, r, auditMeta(map[string]string{"email": input.Email}))
		writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid email or password")
		return
	}

	// Reject disabled accounts
	if user.IsDisabled() {
		writeError(w, http.StatusForbidden, "account_disabled", "Your account has been disabled. Contact an administrator.")
		return
	}

	// If 2FA is enabled, return a challenge token instead of a full session.
	// The challenge token is HMAC-signed, IP-bound, and expires in 5 minutes.
	if user.TOTPEnabled {
		challenge := generateTwoFAChallenge(user.ID, clientIP(r), s.twoFAChallengeSecret)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"requires_2fa":    true,
			"challenge_token": challenge,
		})
		return
	}

	token, err := s.createAuthSession(w, r, user, webSessionTTL)
	if err != nil {
		return
	}

	s.logAuditEventForUser(models.ActionLogin, r, user.ID, auditMeta(map[string]string{"email": user.Email}))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user":  sessionUserPayload(user),
		"token": token,
	})
}

// handleSessionCheck returns current auth status.
func (s *Server) handleSessionCheck(w http.ResponseWriter, r *http.Request) {
	// Check if any users exist
	count, err := s.store.UserCount()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check user count")
		return
	}

	// No users → needs setup (first-time experience)
	if count == 0 {
		writeJSON(w, http.StatusOK, s.setupStatePayload(setupMethodLocalCLI))
		return
	}

	// Try to resolve user from context (set by middleware)
	user := currentUser(r)
	if user != nil {
		writeJSON(w, http.StatusOK, s.sessionStatePayload(true, user))
		return
	}

	// Try session cookie directly (since auth endpoints are exempt from middleware)
	if user := s.validateSessionCookie(r); user != nil {
		writeJSON(w, http.StatusOK, s.sessionStatePayload(true, user))
		return
	}

	writeJSON(w, http.StatusOK, s.sessionStatePayload(false, nil))
}

// handleLogout destroys the session and clears the cookie.
// It handles both cookie-based sessions (web) and Bearer token sessions (CLI).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Revoke cookie-based session
	if cookie, err := r.Cookie(sessionCookieName(s.secureCookies)); err == nil {
		_ = s.store.DeleteSession(cookie.Value)
	}

	// Revoke Bearer session token (CLI auth uses Authorization: Bearer padsess_...)
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if strings.HasPrefix(token, "padsess_") {
			_ = s.store.DeleteSession(token)
		}
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName(s.secureCookies),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})

	// Clear CSRF cookie on logout
	clearCSRFCookie(w)

	s.logAuditEvent(models.ActionLogout, r, "")

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true,
	})
}

// handleGetCurrentUser returns the full profile of the authenticated user.
func (s *Server) handleGetCurrentUser(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil {
		// Try cookie directly (auth endpoints are exempt from middleware)
		user = s.validateSessionCookie(r)
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	resp := map[string]interface{}{
		"id":           user.ID,
		"email":        user.Email,
		"username":     user.Username,
		"name":         user.Name,
		"role":         user.Role,
		"avatar_url":   user.AvatarURL,
		"totp_enabled": user.TOTPEnabled,
		"created_at":   user.CreatedAt,
		"updated_at":   user.UpdatedAt,
	}

	// Include Stripe customer ID when present (used by pad-cloud sidecar
	// to create billing portal sessions without accepting customer_id from
	// the client, preventing users from accessing other users' portals).
	if user.StripeCustomerID != "" {
		resp["stripe_customer_id"] = user.StripeCustomerID
	}

	// Include linked OAuth providers (used by settings UI for link/unlink)
	if providers := user.GetOAuthProviders(); len(providers) > 0 {
		resp["oauth_providers"] = providers
	} else {
		resp["oauth_providers"] = []string{}
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleUpdateCurrentUser updates the authenticated user's profile.
// Supports updating name and/or password. Password changes require the
// current password for verification.
func (s *Server) handleUpdateCurrentUser(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil {
		// Try cookie directly (auth endpoints are exempt from middleware)
		user = s.validateSessionCookie(r)
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	var input struct {
		Name            *string `json:"name,omitempty"`
		Username        *string `json:"username,omitempty"`
		CurrentPassword string  `json:"current_password,omitempty"`
		NewPassword     string  `json:"new_password,omitempty"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	// Validate name if provided
	if input.Name != nil {
		trimmed := strings.TrimSpace(*input.Name)
		if trimmed == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "Name cannot be empty")
			return
		}
		input.Name = &trimmed
	}

	// Validate username if provided
	if input.Username != nil {
		trimmed := strings.ToLower(strings.TrimSpace(*input.Username))
		input.Username = &trimmed

		if err := ValidateUsername(trimmed); err != nil {
			writeError(w, http.StatusBadRequest, "validation_error", err.Error())
			return
		}
		// Check uniqueness (skip if unchanged)
		if trimmed != user.Username {
			existing, err := s.store.GetUserByUsername(trimmed)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check username")
				return
			}
			if existing != nil {
				writeError(w, http.StatusConflict, "conflict", "Username is already taken")
				return
			}
		}
	}

	// Validate password change
	if input.NewPassword != "" {
		if input.CurrentPassword == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "Current password is required to set a new password")
			return
		}
		// Validate against the POST-UPDATE identity — if the same PATCH
		// also changes name/username, a password derived from the new
		// values must be penalized too. Otherwise a caller could set
		// name = "Zaphod" + password = "Zaphod2026" in one request and
		// slip past the context-aware check because we'd be comparing to
		// the PREVIOUS name.
		nameCtx := user.Name
		if input.Name != nil {
			nameCtx = *input.Name
		}
		usernameCtx := user.Username
		if input.Username != nil {
			usernameCtx = *input.Username
		}
		if err := validatePasswordStrength(input.NewPassword, user.Email, nameCtx, usernameCtx); err != nil {
			writeError(w, http.StatusBadRequest, "validation_error", err.Error())
			return
		}

		// Verify current password
		valid, err := s.store.ValidatePassword(user.Email, input.CurrentPassword)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to validate password")
			return
		}
		if valid == nil {
			time.Sleep(500 * time.Millisecond) // Slow down brute force
			writeError(w, http.StatusForbidden, "invalid_password", "Current password is incorrect")
			return
		}
	}

	// Build update
	update := models.UserUpdate{
		Name:     input.Name,
		Username: input.Username,
	}
	if input.NewPassword != "" {
		update.Password = &input.NewPassword
	}

	updated, err := s.store.UpdateUser(user.ID, update)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to update profile")
		return
	}

	resp := map[string]interface{}{
		"id":         updated.ID,
		"email":      updated.Email,
		"username":   updated.Username,
		"name":       updated.Name,
		"role":       updated.Role,
		"avatar_url": updated.AvatarURL,
		"created_at": updated.CreatedAt,
		"updated_at": updated.UpdatedAt,
	}
	if input.NewPassword != "" {
		s.logAuditEvent(models.ActionPasswordChanged, r, "")
		// Sign out every OTHER session — an attacker who sniffed a cookie
		// before the password change shouldn't stay logged in afterwards.
		// Re-issue a fresh session for the caller so they don't get
		// kicked out of the tab they just changed the password in.
		token, ok := s.rotateSessionsAfterCredentialChange(w, r, updated)
		if !ok {
			return
		}
		// Expose the fresh token for Bearer-only callers (CLI / API) who
		// won't see the Set-Cookie header.
		resp["token"] = token
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleForgotPassword generates a password reset token and sends it via email.
// Always returns 200 regardless of whether the email exists (prevents enumeration).
func (s *Server) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email string `json:"email"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	input.Email = strings.TrimSpace(input.Email)

	// Always return the same response to prevent email enumeration
	okResponse := map[string]interface{}{
		"ok":      true,
		"message": "If an account with that email exists, a password reset link has been sent.",
	}

	if input.Email == "" || !emailRegexp.MatchString(input.Email) {
		writeJSON(w, http.StatusOK, okResponse)
		return
	}

	user, err := s.store.GetUserByEmail(input.Email)
	if err != nil || user == nil {
		// Don't reveal whether the email exists
		writeJSON(w, http.StatusOK, okResponse)
		return
	}

	// Generate reset token
	token, err := s.store.CreatePasswordReset(user.ID)
	if err != nil {
		slog.Error("failed to create password reset", "error", err)
		writeJSON(w, http.StatusOK, okResponse)
		return
	}

	// Send reset email
	if s.email != nil && s.baseURL != "" {
		resetURL := s.baseURL + "/reset-password/" + token
		s.goAsync(func() {
			if err := s.email.SendPasswordReset(context.Background(), user.Email, user.Name, resetURL); err != nil {
				slog.Error("failed to send password reset email", "error", err)
			}
		})
	} else {
		slog.Info("password reset token generated (email not configured)")
	}

	writeJSON(w, http.StatusOK, okResponse)
}

// handleResetPassword validates a reset token and sets a new password.
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	if input.Token == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "Reset token is required")
		return
	}
	// Two-phase token handling: look up the user non-destructively so we
	// can run the full identity-aware strength check (email + name +
	// username) against the CURRENT password, then consume the token
	// atomically only if validation passes. Failing pre-consume means a
	// user who typed a weak password can just try again with the same
	// reset link instead of having to request a fresh email.
	preUser, err := s.store.LookupPasswordReset(input.Token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to validate reset token")
		return
	}
	if preUser == nil {
		writeError(w, http.StatusBadRequest, "validation_error", "Reset token is invalid or expired")
		return
	}
	if err := validatePasswordStrength(input.Password, preUser.Email, preUser.Name, preUser.Username); err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}

	// Atomically validate and consume the reset token (prevents race conditions)
	user, err := s.store.ConsumePasswordReset(input.Token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to validate reset token")
		return
	}
	if user == nil {
		writeError(w, http.StatusBadRequest, "invalid_token", "Invalid or expired reset link. Please request a new one.")
		return
	}

	// Reject disabled accounts
	if user.IsDisabled() {
		writeError(w, http.StatusForbidden, "account_disabled", "Your account has been disabled. Contact an administrator.")
		return
	}

	// Update password
	password := input.Password
	update := models.UserUpdate{Password: &password}
	if _, err := s.store.UpdateUser(user.ID, update); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to update password")
		return
	}

	// Invalidate all existing sessions (force logout everywhere)
	if err := s.store.DeleteUserSessions(user.ID); err != nil {
		slog.Error("failed to invalidate sessions after password reset", "error", err)
	}

	// Create a fresh session so the user is logged in
	sessionToken, err := s.store.CreateSession(user.ID, "web", clientIP(r), r.UserAgent(), webSessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Password updated but failed to create session")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName(s.secureCookies),
		Value:    sessionToken,
		Path:     "/",
		MaxAge:   int(webSessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})

	// Set CSRF cookie alongside the new session
	setCSRFCookie(w, int(webSessionTTL.Seconds()), s.secureCookies)

	s.logAuditEventForUser(models.ActionPasswordReset, r, user.ID, auditMeta(map[string]string{"email": user.Email}))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true,
		"user": map[string]interface{}{
			"id":       user.ID,
			"email":    user.Email,
			"username": user.Username,
			"name":     user.Name,
			"role":     user.Role,
		},
		"token": sessionToken,
	})
}
