package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/xarmian/pad/internal/models"
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
	// ctxResolvedWorkspaceID is set by RequireWorkspaceAccess after resolving
	// the workspace slug/ID. Avoids redundant lookups in handlers.
	ctxResolvedWorkspaceID contextKey = "resolved_workspace_id"
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
		// Already authenticated by TokenAuth
		if currentUser(r) != nil {
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

// RequireAuth middleware blocks unauthenticated requests when users exist
// in the system. When no users exist (fresh install), all requests pass
// through to allow the setup flow.
func (s *Server) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Auth endpoints, share link resolution, health, and the public
		// plan-limits endpoint are always exempt from auth.
		if strings.HasPrefix(path, "/api/v1/auth/") || path == "/api/v1/health" || strings.HasPrefix(path, "/api/v1/health/") || strings.HasPrefix(path, "/api/v1/s/") ||
			path == "/api/v1/plan-limits" {
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
			// preventing goroutine/connection buildup under load.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				s.store.TouchUserActivity(ctx, user.ID)
			}()
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

// tokenScopeAllows checks if the token's scopes permit the given HTTP method
// and path. Scopes are stored as a JSON array of strings.
//
// Supported scopes:
//   - "*"       — full access (default)
//   - "read"    — GET/HEAD/OPTIONS only
//   - "write"   — all methods
//
// An empty or unparseable scope string defaults to full access for
// backward compatibility with tokens created before scope enforcement.
func tokenScopeAllows(scopesJSON, method, path string) bool {
	_ = path // reserved for future per-resource scopes

	if scopesJSON == "" || scopesJSON == `["*"]` {
		return true
	}

	var scopes []string
	if err := json.Unmarshal([]byte(scopesJSON), &scopes); err != nil {
		// Unparseable → allow (backward compat)
		return true
	}

	hasKnownScope := false
	for _, scope := range scopes {
		switch scope {
		case "*", "write":
			return true
		case "read":
			hasKnownScope = true
			if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
				return true
			}
		default:
			// Unrecognized scope — ignore but don't block.
			// Tokens created before scope enforcement may contain
			// custom values that were previously stored but not checked.
		}
	}

	// If no recognized scope was found, allow for backward compatibility
	// (e.g. tokens with only custom/legacy scope strings).
	if !hasKnownScope {
		return true
	}

	return false
}
