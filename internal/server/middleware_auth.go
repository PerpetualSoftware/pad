package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/xarmian/pad/internal/models"
)

type contextKey string

const (
	// ctxTokenWorkspaceID is set when a valid API token is present.
	ctxTokenWorkspaceID contextKey = "token_workspace_id"
	// ctxCurrentUser is set when an authenticated user is resolved.
	ctxCurrentUser contextKey = "current_user"
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
			user, err := s.store.ValidateSession(token)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal_error", "Session validation failed")
				return
			}
			if user == nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid or expired session")
				return
			}
			ctx := context.WithValue(r.Context(), ctxCurrentUser, user)
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

		// Store workspace ID from the token in request context.
		ctx := context.WithValue(r.Context(), ctxTokenWorkspaceID, apiToken.WorkspaceID)
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

		// Try session cookie
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}

		user, err := s.store.ValidateSession(cookie.Value)
		if err != nil || user == nil {
			next.ServeHTTP(w, r)
			return
		}

		ctx := context.WithValue(r.Context(), ctxCurrentUser, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireAuth middleware blocks unauthenticated requests when users exist
// in the system. When no users exist (fresh install), all requests pass
// through to allow the setup flow.
func (s *Server) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Auth endpoints are always exempt
		if strings.HasPrefix(path, "/api/v1/auth/") || path == "/api/v1/health" {
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
		if currentUser(r) != nil || tokenWorkspaceID(r) != "" {
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

// currentUserID returns the authenticated user's ID, or empty string.
func currentUserID(r *http.Request) string {
	if u := currentUser(r); u != nil {
		return u.ID
	}
	return ""
}
