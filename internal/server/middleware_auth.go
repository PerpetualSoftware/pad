package server

import (
	"context"
	"net/http"
	"strings"
)

type contextKey string

const (
	// ctxTokenWorkspaceID is set when a valid API token is present.
	ctxTokenWorkspaceID contextKey = "token_workspace_id"
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

		// Expect "Bearer pad_<64 hex chars>"
		if !strings.HasPrefix(auth, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid authorization header format")
			return
		}

		token := strings.TrimPrefix(auth, "Bearer ")
		token = strings.TrimSpace(token)

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

// tokenWorkspaceID returns the workspace ID set by the TokenAuth middleware,
// or an empty string if no token was used.
func tokenWorkspaceID(r *http.Request) string {
	v, _ := r.Context().Value(ctxTokenWorkspaceID).(string)
	return v
}
