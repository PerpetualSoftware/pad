package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/go-chi/chi/v5"
)

type contextKey string

const (
	// ctxTokenWorkspaceID is set when a valid API token is present.
	ctxTokenWorkspaceID contextKey = "token_workspace_id"
	// ctxCurrentUser is set when an authenticated user is resolved.
	ctxCurrentUser contextKey = "current_user"
	// ctxWorkspaceRole is set by RequireWorkspaceAccess with the user's role.
	ctxWorkspaceRole contextKey = "workspace_role"
	// ctxIsAPIToken is set to true when the request is authenticated via an API token
	// (as opposed to a session cookie or CLI session token).
	ctxIsAPIToken contextKey = "is_api_token"
	// ctxTokenScopes carries the JSON-encoded scopes string from the
	// validated API token (e.g. `["read"]`, `["*"]`). Stashed by
	// MCPBearerAuth so the in-process MCP dispatcher can re-check
	// scopes per synthesized tool call (the dispatcher bypasses
	// TokenAuth's chain-level scope check by setting WithCurrentUser
	// directly). See WithTokenScopes / TokenScopesFromContext in
	// context.go and the per-tool gate in internal/mcp/dispatch_http.go.
	ctxTokenScopes contextKey = "token_scopes"
	// ctxResolvedWorkspaceID is set by RequireWorkspaceAccess after resolving
	// the workspace slug/ID. Avoids redundant lookups in handlers.
	ctxResolvedWorkspaceID contextKey = "resolved_workspace_id"
	// ctxTokenAllowedWorkspaces carries the OAuth token's workspace
	// allow-list set at consent time (TASK-952). Either a list of
	// slugs or `["*"]` (wildcard). Read by RequireWorkspaceAccess to
	// reject requests against workspaces the user didn't include in
	// the consent (TASK-953). nil → no token-level workspace
	// constraint (PAT auth, or pre-TASK-952 OAuth tokens).
	ctxTokenAllowedWorkspaces contextKey = "token_allowed_workspaces"
	// ctxMCPTokenKind / ctxMCPTokenRef carry the bearer's identity for
	// MCP audit logging (TASK-960). Stashed by MCPBearerAuth so the
	// audit middleware (middleware_mcp_audit.go) can record which
	// connection (OAuth request_id chain) or which PAT (api_tokens.id)
	// drove the call. Empty when no MCP-auth happened (i.e. the request
	// didn't go through MCPBearerAuth). The ref values aren't sensitive
	// — they're internal IDs / chain identifiers, never the raw bearer
	// — so leaking via context is fine.
	ctxMCPTokenKind contextKey = "mcp_token_kind"
	ctxMCPTokenRef  contextKey = "mcp_token_ref"
)

// TokenAuth middleware checks for an Authorization: Bearer pad_xxx header.
// If a valid token is found, the associated workspace ID is stored in the
// request context. If no token header is present the request passes through
// unchanged (existing localhost behaviour). Invalid or expired tokens
// receive a 401 response.
func (s *Server) TokenAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			// No token provided — allow through (localhost access).
			next.ServeHTTP(w, r)
			return
		}

		// Expect "Bearer pad_<64 hex chars>" or "Bearer padsess_<64 hex chars>"
		if !strings.HasPrefix(auth, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid authorization header format")
			return
		}

		token := strings.TrimPrefix(auth, "Bearer ")
		token = strings.TrimSpace(token)

		// Session token (from CLI login)
		if strings.HasPrefix(token, "padsess_") {
			session, err := s.store.ValidateSession(token)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal_error", "Session validation failed")
				return
			}
			if session == nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid or expired session")
				return
			}
			// Session binding: validate User-Agent hasn't changed
			if session.UAHash != "" && sha256hex(r.UserAgent()) != session.UAHash {
				slog.Warn("session binding mismatch: User-Agent changed",
					"session_ip", session.IPAddress,
					"client_ip", clientIP(r))
				writeError(w, http.StatusUnauthorized, "unauthorized", "Session expired")
				return
			}
			// Session binding: detect IP changes. Log to audit log in all modes;
			// reject only when PAD_IP_CHANGE_ENFORCE=strict.
			switch s.handleSessionIPChange(w, r, session, token) {
			case sessionIPChangeTerminated:
				return
			case sessionIPChangeRevoked:
				// Session was destroyed. For public API paths (login,
				// password reset, health, share links) pass through
				// unauthenticated so the caller can recover. For
				// authenticated-only paths, the Bearer flow has no SPA
				// fallback — treat revocation as a hard 401.
				if isPublicAPIPath(r.URL.Path) {
					next.ServeHTTP(w, r)
					return
				}
				writeError(w, http.StatusUnauthorized, "session_ip_changed",
					"Session client IP changed — please log in again.")
				return
			}
			ctx := context.WithValue(r.Context(), ctxCurrentUser, session.User)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// API token
		if !strings.HasPrefix(token, "pad_") || len(token) != 68 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid token format")
			return
		}

		apiToken, err := s.store.ValidateToken(token)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Token validation failed")
			return
		}
		if apiToken == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid or expired token")
			return
		}

		// Enforce token scopes
		if !tokenScopeAllows(apiToken.Scopes, r.Method, r.URL.Path) {
			writeError(w, http.StatusForbidden, "forbidden", "Token scope does not permit this action")
			return
		}

		// Add near-expiry warning headers
		setTokenExpiryWarning(w, apiToken)

		ctx := context.WithValue(r.Context(), ctxIsAPIToken, true)

		// Resolve user from token's user_id (new user-owned tokens)
		if apiToken.UserID != "" {
			user, err := s.store.GetUser(apiToken.UserID)
			if err == nil && user != nil {
				ctx = context.WithValue(ctx, ctxCurrentUser, user)
			}
		}

		// Store workspace ID from the token if workspace-scoped
		if apiToken.WorkspaceID != "" {
			ctx = context.WithValue(ctx, ctxTokenWorkspaceID, apiToken.WorkspaceID)
		}

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// SessionAuth middleware resolves session cookies into authenticated users.
// If a user was already resolved by TokenAuth, this is a no-op.
func (s *Server) SessionAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Already authenticated by TokenAuth — user-bound API tokens set
		// currentUser, legacy workspace-scoped tokens set tokenWorkspaceID
		// with no user. In either case we must short-circuit: otherwise a
		// stale session cookie on the same request could trigger the
		// IP-change strict-mode path and 401 the request even though the
		// API token itself is valid.
		if currentUser(r) != nil || tokenWorkspaceID(r) != "" {
			next.ServeHTTP(w, r)
			return
		}

		// Try session cookie (with fallback to unprefixed name for upgrade path)
		cookie, err := r.Cookie(sessionCookieName(s.secureCookies))
		if err != nil {
			cookie, err = r.Cookie("pad_session")
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
		}

		session, err := s.store.ValidateSession(cookie.Value)
		if err != nil || session == nil {
			next.ServeHTTP(w, r)
			return
		}

		// Session binding: validate User-Agent hasn't changed
		if session.UAHash != "" && sha256hex(r.UserAgent()) != session.UAHash {
			slog.Warn("session binding mismatch: User-Agent changed",
				"session_ip", session.IPAddress,
				"client_ip", clientIP(r))
			next.ServeHTTP(w, r)
			return
		}

		// Session binding: detect IP changes. Log to audit log in all modes;
		// reject only when PAD_IP_CHANGE_ENFORCE=strict.
		switch s.handleSessionIPChange(w, r, session, cookie.Value) {
		case sessionIPChangeTerminated:
			return
		case sessionIPChangeRevoked:
			// Browser path: let the request through unauthenticated so
			// the SPA renders its login flow instead of a JSON error.
			next.ServeHTTP(w, r)
			return
		}

		// Re-issue CSRF cookie if the session is valid but the cookie is missing.
		// This can happen when cookies expire at different times or are selectively cleared.
		// Skip for auth endpoints — they manage their own CSRF cookies (login sets, logout clears).
		if !strings.HasPrefix(r.URL.Path, "/api/v1/auth/") {
			if _, csrfErr := r.Cookie(csrfCookieName(s.secureCookies)); csrfErr != nil {
				setCSRFCookie(w, 7*24*60*60, s.secureCookies)
			}
		}

		ctx := context.WithValue(r.Context(), ctxCurrentUser, session.User)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isPublicAPIPath reports whether the given request path is an API
// endpoint that bypasses authentication entirely. These handlers must
// work even when the caller has no valid session (login, registration,
// password reset, health probes, public plan limits, share-link tokens).
// Shared between RequireAuth and the session-IP-change path so both stay
// in sync — in particular, strict IP-change enforcement must NOT 401
// these endpoints, otherwise a user with a stale cookie can't even
// recover by logging in again.
func isPublicAPIPath(path string) bool {
	return strings.HasPrefix(path, "/api/v1/auth/") ||
		path == "/api/v1/health" ||
		strings.HasPrefix(path, "/api/v1/health/") ||
		strings.HasPrefix(path, "/api/v1/s/") ||
		path == "/api/v1/plan-limits" ||
		// Server capability profile (TASK-878). The editor reads this
		// before login on shared-item preview surfaces, and the
		// handler is intentionally read-only (no DB writes, no
		// per-user state). Keeping it public matches the route's
		// register-time intent in server.go.
		path == "/api/v1/server/capabilities"
}

// RequireAuth middleware blocks unauthenticated requests when users exist
// in the system. When no users exist (fresh install), all requests pass
// through to allow the setup flow.
func (s *Server) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Auth endpoints, share link resolution, health, and the public
		// plan-limits endpoint are always exempt from auth.
		if isPublicAPIPath(path) {
			next.ServeHTTP(w, r)
			return
		}

		// Cloud sidecar endpoints authenticate via cloud_secret (X-Cloud-Secret
		// header, or legacy ?cloud_secret query-param). Only bypass the auth
		// gate when BOTH conditions hold:
		//   1. The request path is one of the three cloud admin endpoints.
		//   2. The request carries a cloud-secret marker.
		// The path gate is critical — without it, setting X-Cloud-Secret on
		// any route (e.g. GET /api/v1/workspaces) would globally bypass auth.
		// After the bypass, requireCloudMode (route-level) + validateCloudSecret
		// (handler-level) still confirm cloud mode is actually on and that the
		// secret matches.
		if isCloudAdminPath(path) && hasCloudSecretMarker(r) {
			next.ServeHTTP(w, r)
			return
		}

		// Static assets are exempt
		if strings.HasPrefix(path, "/_app/") ||
			path == "/favicon.ico" ||
			strings.HasSuffix(path, ".png") ||
			strings.HasSuffix(path, ".svg") ||
			strings.HasSuffix(path, ".ico") ||
			strings.HasSuffix(path, ".webmanifest") ||
			strings.HasSuffix(path, ".json") && !strings.HasPrefix(path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		// If no users exist, allow everything (fresh install / setup mode)
		count, err := s.store.UserCount()
		if err != nil || count == 0 {
			next.ServeHTTP(w, r)
			return
		}

		// Already authenticated via token or session
		if user := currentUser(r); user != nil {
			if user.IsDisabled() {
				writeError(w, http.StatusForbidden, "account_disabled", "Your account has been disabled. Contact an administrator.")
				return
			}
			// Use a short-lived context so the write is cancelled if the DB is slow,
			// preventing goroutine/connection buildup under load. Tracked via
			// s.goAsync so Stop() can drain it before the DB is closed (BUG-842).
			s.goAsync(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				s.store.TouchUserActivity(ctx, user.ID)
			})
			next.ServeHTTP(w, r)
			return
		}
		if tokenWorkspaceID(r) != "" {
			next.ServeHTTP(w, r)
			return
		}

		// Unauthenticated — return 401 for API, let SPA handle for browser
		if strings.HasPrefix(path, "/api/") {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
			return
		}

		// For browser page requests, serve the SPA (frontend will check auth and show login)
		next.ServeHTTP(w, r)
	})
}

// tokenWorkspaceID returns the workspace ID set by the TokenAuth middleware,
// or an empty string if no token was used.
func tokenWorkspaceID(r *http.Request) string {
	v, _ := r.Context().Value(ctxTokenWorkspaceID).(string)
	return v
}

// currentUser returns the authenticated user from the request context,
// or nil if no user is authenticated.
func currentUser(r *http.Request) *models.User {
	u, _ := r.Context().Value(ctxCurrentUser).(*models.User)
	return u
}

// isAPITokenAuth returns true if the request was authenticated via an API token
// (not a session cookie or CLI session). Use this to gate sensitive operations
// like 2FA enrollment that should require an interactive session.
func isAPITokenAuth(r *http.Request) bool {
	v, _ := r.Context().Value(ctxIsAPIToken).(bool)
	return v
}

// currentUserID returns the authenticated user's ID, or empty string.
func currentUserID(r *http.Request) string {
	if u := currentUser(r); u != nil {
		return u.ID
	}
	return ""
}

// RequireWorkspaceAccess checks that the current user is a member of the
// workspace identified by the {slug} URL parameter. The user's workspace
// role is stored in the request context for downstream permission checks.
//
// When no users exist (fresh install), access is granted with an implicit
// "owner" role. Legacy API tokens (workspace-scoped, no user) are allowed
// if the token's workspace matches the requested workspace.
func (s *Server) RequireWorkspaceAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slugOrID := chi.URLParam(r, "slug")
		if slugOrID == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Resolve workspace: supports both UUID and slug.
		ws, err := s.resolveWorkspace(slugOrID, currentUser(r))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to resolve workspace")
			return
		}
		if ws == nil {
			writeError(w, http.StatusNotFound, "not_found", "Workspace not found")
			return
		}

		// OAuth token allow-list gate (TASK-953). The consent UI
		// (TASK-952) lets the user pick which workspaces a token
		// can access; MCPBearerAuth stashes that list in context
		// via WithTokenAllowedWorkspaces. Reject any request hitting
		// a workspace outside the list, even if the user is a
		// member of it — the user explicitly chose not to grant the
		// app that access.
		//
		// nil → no token-level constraint (PAT auth, or pre-TASK-952
		// OAuth tokens that predate the consent UI). Wildcard `["*"]`
		// → grant access to any membership the user has. Else: the
		// resolved workspace's slug MUST appear in the list.
		//
		// Compares against the canonical slug (ws.Slug) because the
		// consent UI persists slugs and the URL slugOrID may be a
		// UUID which resolveWorkspace just translated. Slug-vs-slug
		// is the right comparison.
		if !tokenAllowedWorkspaceMatches(r.Context(), ws.Slug) {
			// TASK-961: count workspace-allow-list denials so the
			// MCP dashboard can flag tokens hitting workspaces outside
			// their consent scope. Gated on MCP-origin requests only
			// (presence of MCP token identity in context) so non-MCP
			// /api/v1 traffic — which can't even reach this gate
			// today, but might in a future PAT-with-allow-list world
			// — doesn't pollute the MCP-specific counter.
			s.recordMCPAuthzDenial(r, "workspace_not_in_allowlist")
			writeError(w, http.StatusForbidden, "permission_denied",
				"Token is not authorized for this workspace")
			return
		}

		// Store resolved workspace ID in context for downstream handlers
		ctx := context.WithValue(r.Context(), ctxResolvedWorkspaceID, ws.ID)

		// Fresh install: no users → everyone gets owner access
		count, _ := s.store.UserCount()
		if count == 0 {
			ctx = context.WithValue(ctx, ctxWorkspaceRole, "owner")
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// Legacy API token: workspace-scoped token without user context
		if tokenWsID := tokenWorkspaceID(r); tokenWsID != "" && currentUser(r) == nil {
			if tokenWsID == ws.ID {
				ctx = context.WithValue(ctx, ctxWorkspaceRole, "editor")
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			writeError(w, http.StatusForbidden, "forbidden", "Token not authorized for this workspace")
			return
		}

		// Authenticated user: check workspace membership
		user := currentUser(r)
		if user == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
			return
		}

		// Admin users get owner access to all workspaces
		if user.Role == "admin" {
			ctx = context.WithValue(ctx, ctxWorkspaceRole, "owner")
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		member, err := s.store.GetWorkspaceMember(ws.ID, user.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check workspace access")
			return
		}
		if member == nil {
			// Not a member — check for guest access via grants
			hasGrants, grantErr := s.store.UserHasGrantsInWorkspace(ws.ID, user.ID)
			if grantErr != nil {
				slog.Error("failed to check guest grants", "workspace_id", ws.ID, "user_id", user.ID, "error", grantErr)
				writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check workspace access")
				return
			}
			if !hasGrants {
				// TASK-961: not_a_member denial — counts only when the
				// request originated from the /mcp surface (otherwise
				// the regular /api/v1 403s would inflate the MCP-
				// specific counter).
				s.recordMCPAuthzDenial(r, "not_a_member")
				writeError(w, http.StatusForbidden, "forbidden", "You are not a member of this workspace")
				return
			}
			ctx = context.WithValue(ctx, ctxWorkspaceRole, "guest")
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		ctx = context.WithValue(ctx, ctxWorkspaceRole, member.Role)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// workspaceRole returns the user's role in the current workspace,
// as set by RequireWorkspaceAccess. Returns empty string if not set.
func workspaceRole(r *http.Request) string {
	v, _ := r.Context().Value(ctxWorkspaceRole).(string)
	return v
}

// recordMCPAuthzDenial bumps the pad_mcp_authz_denials_total counter
// when the request originated from the MCP surface. The discriminator
// is the presence of an MCPTokenIdentity in the request context —
// MCPBearerAuth stashes that for every authenticated /mcp request, and
// no other entry point sets it.
//
// Called from middleware that runs DOWNSTREAM of MCPBearerAuth (the
// in-process MCP dispatcher routes through the API handler tree, so
// /api/v1/* middleware sees the same context). Called sites must pass
// a reason string from the documented vocabulary in
// internal/metrics/metrics.go's MCPAuthzDenialsTotal comment.
//
// No-op when metrics aren't wired (selfhost / tests) or when the
// request didn't come through MCP — keeps the metric MCP-specific
// without forcing every caller to repeat the gate.
func (s *Server) recordMCPAuthzDenial(r *http.Request, reason string) {
	if s.metrics == nil {
		return
	}
	if kind, _ := MCPTokenIdentityFromContext(r.Context()); kind == "" {
		return
	}
	s.metrics.MCPAuthzDenialsTotal.WithLabelValues(reason).Inc()
}

// requireRole checks if the user's workspace role meets the minimum
// required level. Role hierarchy: owner > editor > viewer.
func requireRole(r *http.Request, minRole string) bool {
	role := workspaceRole(r)
	if role == "" {
		return false
	}
	return roleLevel(role) >= roleLevel(minRole)
}

// requireMinRole checks role and writes a 403 if insufficient.
// Returns true if the request should continue, false if it was rejected.
func requireMinRole(w http.ResponseWriter, r *http.Request, minRole string) bool {
	if requireRole(r, minRole) {
		return true
	}
	writeError(w, http.StatusForbidden, "forbidden", "Insufficient permissions")
	return false
}

// roleLevel returns a numeric level for role comparison.
// Higher values indicate more permissions.
func roleLevel(role string) int {
	switch role {
	case "owner":
		return 3
	case "editor":
		return 2
	case "viewer":
		return 1
	case "guest":
		return 0 // Guests have grant-based access only, no role-based permissions
	default:
		return 0
	}
}

// permissionLevel returns a numeric level for grant permission comparison.
func permissionLevel(perm string) int {
	switch perm {
	case "owner":
		return 4
	case "admin":
		return 3
	case "editor":
		return 2
	case "edit":
		return 2
	case "viewer":
		return 1
	case "view":
		return 1
	default:
		return 0
	}
}

// sha256hex returns the SHA-256 hex digest of a string.
func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// canonicalIP returns the semantic (canonical text) form of an IP
// address string. For IPv6 this collapses different valid spellings
// (compressed vs expanded, uppercase vs lowercase, leading zeroes) to
// a single representation so comparing two strings by == reflects
// actual IP equality. For IPv4 it accepts both 4-byte and IPv4-in-IPv6
// forms. For inputs that don't parse as an IP the original string is
// returned so behavior stays predictable on malformed/debug values.
func canonicalIP(s string) string {
	if s == "" {
		return s
	}
	if ip := net.ParseIP(s); ip != nil {
		return ip.String()
	}
	return s
}

// sessionIPChangeOutcome captures what the caller (TokenAuth / SessionAuth)
// should do after handleSessionIPChange inspected the session.
type sessionIPChangeOutcome int

const (
	// sessionIPChangeContinue — happy path. IP matches (or no recorded IP).
	// Caller proceeds to install the session user in context and call next.
	sessionIPChangeContinue sessionIPChangeOutcome = iota
	// sessionIPChangeAllowedLogged — IP differs, log-only mode. Session
	// remains valid, caller proceeds as usual.
	sessionIPChangeAllowedLogged
	// sessionIPChangeRevoked — strict mode. The session was destroyed and
	// the caller must NOT install the user in context. For browser (non-API)
	// paths the request should continue unauthenticated so the SPA can
	// render its login screen instead of a raw JSON error.
	sessionIPChangeRevoked
	// sessionIPChangeTerminated — strict mode + API path. A 401 has already
	// been written; caller must return immediately.
	sessionIPChangeTerminated
)

// handleSessionIPChange compares the client IP on this request to the IP
// recorded when the session was created.
//
// Behavior:
//   - Session has no recorded IP (legacy pre-migration row): continue.
//   - IP matches: continue.
//   - Log-only mode (default): atomically rotate the stored IP via
//     compare-and-set. The race winner emits exactly one
//     ActionSessionIPChanged audit row per transition — concurrent
//     requests that lose the CAS skip logging to avoid duplicate rows.
//   - Strict mode (PAD_IP_CHANGE_ENFORCE=strict): do NOT rotate the
//     stored IP. Instead use DeleteSessionIfExists as the CAS primitive
//     — the request that actually deletes the row (a) logs the audit
//     entry once, (b) returns 401 for API / revoked for browser paths.
//     Rotating the IP first would be unsafe: if the subsequent
//     DeleteSession failed (transient DB error) the session would remain
//     valid rebound to the new IP, defeating strict enforcement. By
//     deleting atomically, any failure leaves the session bound to the
//     original IP so a follow-up request from the new IP still mismatches
//     and is still rejected. If the delete errors outright, the request
//     is rejected with 500 so the client can't proceed.
//
// Log-only mode breaks fewer legitimate clients (mobile roaming, VPN
// toggles, carrier NAT) while still giving operators a visible signal.
func (s *Server) handleSessionIPChange(w http.ResponseWriter, r *http.Request, session *store.SessionInfo, plainToken string) sessionIPChangeOutcome {
	// Canonicalize both sides so equivalent IPv6 representations
	// (compressed vs expanded, case, leading zeros) don't register as a
	// spurious change. Raw trusted-proxy X-Forwarded-For values can
	// arrive in any valid form.
	storedIP := canonicalIP(session.IPAddress)
	newIP := canonicalIP(clientIP(r))
	if storedIP == "" || newIP == "" || storedIP == newIP {
		return sessionIPChangeContinue
	}

	userID := ""
	if session.User != nil {
		userID = session.User.ID
	}

	if s.ipChangeEnforceStrict {
		// Destroy the session atomically. Only the caller whose DELETE
		// affected a row logs and issues the "session_ip_changed" response
		// body. A failed delete means the session is still valid AND still
		// bound to the old IP (we skipped the rotation), so a follow-up
		// request from the new IP will hit this path again — safe.
		deleted, err := s.store.DeleteSessionIfExists(plainToken)
		if err != nil {
			// Fail closed: we can't prove the session is gone, so refuse
			// to let the request through. A retry will converge once the
			// DB recovers.
			slog.Error("failed to destroy session on IP-change in strict mode; failing closed",
				"session_ip", storedIP,
				"client_ip", newIP,
				"user_id", userID,
				"error", err)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"Unable to validate session. Please try again.")
			return sessionIPChangeTerminated
		}
		if deleted {
			s.logAuditEventForUser(models.ActionSessionIPChanged, r, userID, auditMeta(map[string]string{
				"old_ip": storedIP,
				"new_ip": newIP,
			}))
			slog.Warn("session destroyed: IP changed (strict enforcement)",
				"session_ip", storedIP,
				"client_ip", newIP,
				"user_id", userID)
		}
		// Clear client-side cookies regardless — even if another request
		// already deleted the row, this client is still holding the now-
		// invalid token.
		clearSessionCookie(w, s.secureCookies)
		clearCSRFCookie(w)
		// Public API paths (login/register/password-reset, health, share
		// links, plan-limits) must STILL work after the session was
		// destroyed — a user with a stale cookie needs a way back in, and
		// Prometheus health probes shouldn't 401 because some other tab
		// left a stale session. Pass the request through unauthenticated.
		if isPublicAPIPath(r.URL.Path) {
			return sessionIPChangeRevoked
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusUnauthorized, "session_ip_changed",
				"Session client IP changed — please log in again.")
			return sessionIPChangeTerminated
		}
		// Non-API (browser) path: caller must NOT install the destroyed
		// session's user into the request context. The SPA will render its
		// unauth state and the user will redirect to login.
		return sessionIPChangeRevoked
	}

	// Log-only mode: CAS-rotate the stored IP so only the winner logs.
	// The CAS compares against session.IPAddress (the raw value we read
	// from the DB) and writes the canonical newIP so future comparisons
	// are consistent. Err on the side of "winner" when the CAS errors
	// — we'd rather log an extra row than miss the signal entirely.
	// A failed rotate here is non-fatal: worst case the next request
	// also gets an audit row.
	won, err := s.store.UpdateSessionIPIfEquals(plainToken, session.IPAddress, newIP)
	if err != nil {
		slog.Warn("failed to rotate session ip after change", "error", err)
		won = true
	}
	if won {
		s.logAuditEventForUser(models.ActionSessionIPChanged, r, userID, auditMeta(map[string]string{
			"old_ip": storedIP,
			"new_ip": newIP,
		}))
		slog.Info("session client IP changed (logged, request allowed)",
			"session_ip", storedIP,
			"client_ip", newIP,
			"user_id", userID)
	}
	return sessionIPChangeAllowedLogged
}

// clearSessionCookie expires the session cookie on the client so a
// subsequent request doesn't keep presenting a now-revoked token.
func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName(secure),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// tokenAllowedWorkspaceMatches reports whether the OAuth token's
// workspace allow-list (set at consent time, TASK-952) permits the
// given workspace slug. The three return-shape cases match
// TokenAllowedWorkspacesFromContext:
//
//   - nil — no allow-list set (PAT auth, or pre-TASK-952 tokens) →
//     allow.
//   - ["*"] — wildcard consent → allow.
//   - [slug-a, slug-b, ...] — explicit allow-list → require slug ∈
//     list.
//
// Empty (non-nil) slice — the SetAllowedWorkspaces guard rejects
// nil → empty translation, and the consent flow rejects
// `allowed_workspaces` with no entries (handlers_oauth.go's
// parseConsentPayload). So an empty list shouldn't appear in
// practice; if it does, fail closed (no slug matches an empty list).
//
// Used by RequireWorkspaceAccess; package-private because the
// allow-list semantics are coupled to that middleware's flow.
func tokenAllowedWorkspaceMatches(ctx context.Context, slug string) bool {
	allowed := TokenAllowedWorkspacesFromContext(ctx)
	if allowed == nil {
		return true // no token-level gate
	}
	for _, entry := range allowed {
		if entry == "*" {
			return true
		}
		if entry == slug {
			return true
		}
	}
	return false
}

// tokenScopeAllows checks if the token's scopes permit the given HTTP method
// and path. Scopes are stored as a JSON array of strings.
//
// Supported scopes (PAT vocabulary):
//   - "*"       — full access
//   - "read"    — GET/HEAD/OPTIONS only
//   - "write"   — all methods
//
// Supported scopes (OAuth 2.1 / RFC 6749 vocabulary, sub-PR E TASK-1027):
//   - "pad:read"  — equivalent to PAT "read"
//   - "pad:write" — equivalent to PAT "write"
//   - "pad:admin" — equivalent to PAT "*"
//
// MCPBearerAuth's OAuth introspection branch translates fosite's
// space-separated scope string into the same JSON-array stash form
// PAT auth uses, so the same policy applies to both transports. This
// keeps MCP tool authorization centralized: a `pad:read` OAuth token
// can drive read tools; a `pad:write` OAuth token can drive any.
//
// Policy (deny-by-default whitelist):
//   - Empty scope string → allow (legacy DB rows where the column was never
//     populated). These tokens pre-date scope enforcement.
//   - Empty JSON array `[]` → allow (explicit "unrestricted" legacy form,
//     equivalent to no scopes recorded).
//   - Parseable array with at least one scope: allow iff a RECOGNIZED scope
//     grants this method. Unrecognized scopes never contribute to allow
//     and are logged once per request so operators can spot typos like
//     "read-only" that would previously have fallen open.
//   - Unparseable JSON → deny (data corruption or tampering). Previously
//     fell open; that was the security concern behind TASK-667.
//
// Switching to deny-by-default closes the hole where a future scope name
// like "read-only" would silently grant full access.
func tokenScopeAllows(scopesJSON, method, path string) bool {
	_ = path // reserved for future per-resource scopes

	// Empty string (legacy DB rows where the column was never populated)
	// → full access. Same fast path for the explicit wildcard.
	if scopesJSON == "" || strings.TrimSpace(scopesJSON) == `["*"]` {
		return true
	}

	var scopes []string
	if err := json.Unmarshal([]byte(scopesJSON), &scopes); err != nil {
		slog.Warn("token has unparseable scopes; denying request",
			"method", method, "path", path, "error", err)
		return false
	}

	// Distinguish JSON null (scopes == nil after unmarshal) from an
	// explicit empty array (non-nil, len 0). null falls into the deny
	// path because it signals a client-side serializer bug, not a
	// documented legacy form. An explicit [] (including whitespace-
	// padded "[ ]" or "[\n]") is the documented legacy-unrestricted
	// form and keeps working.
	if scopes == nil {
		slog.Warn("token has null scopes; denying request",
			"method", method, "path", path, "scopes_json", scopesJSON)
		return false
	}
	if len(scopes) == 0 {
		// Explicit empty array → unrestricted (legacy pre-enforcement).
		return true
	}

	allowed := false
	var unknown []string
	for _, scope := range scopes {
		switch scope {
		case "*", "write", "pad:write", "pad:admin":
			allowed = true
		case "read", "pad:read":
			if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
				allowed = true
			}
		default:
			unknown = append(unknown, scope)
		}
	}

	if len(unknown) > 0 {
		slog.Warn("token has unrecognized scopes; treated as no-grant",
			"method", method, "path", path, "unknown_scopes", unknown)
	}
	return allowed
}
