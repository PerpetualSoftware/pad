package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// handleCreateCLIAuthSession creates a new pending CLI auth session.
// The CLI calls this, then presents the auth URL to the user.
// POST /api/v1/auth/cli/sessions
func (s *Server) handleCreateCLIAuthSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.store.CreateCLIAuthSession()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to create CLI auth session")
		return
	}

	// Build the auth URL that the user will open in their browser.
	// Use the request's Host header to construct the URL so it works
	// regardless of whether the server is at localhost, a VPS, or cloud.
	scheme := "https"
	if r.TLS == nil {
		// Check X-Forwarded-Proto for reverse proxy setups
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			scheme = proto
		} else {
			scheme = "http"
		}
	}
	authURL := fmt.Sprintf("%s://%s/auth/cli/%s", scheme, r.Host, sess.Code)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"session_code": sess.Code,
		"auth_url":     authURL,
		"expires_at":   sess.ExpiresAt,
	})
}

// handlePollCLIAuthSession checks the status of a CLI auth session.
// The CLI polls this until the session is approved or expired.
// GET /api/v1/auth/cli/sessions/{code}
func (s *Server) handlePollCLIAuthSession(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if code == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Missing session code")
		return
	}

	sess, err := s.store.GetCLIAuthSession(code)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check CLI auth session")
		return
	}
	if sess == nil {
		writeError(w, http.StatusNotFound, "not_found", "CLI auth session not found")
		return
	}

	response := map[string]interface{}{
		"status": sess.Status,
	}

	// Only include the token and user info when approved
	if sess.Status == "approved" && sess.Token != "" {
		response["token"] = sess.Token

		// Look up user info to return alongside the token
		if sess.UserID != "" {
			user, err := s.store.GetUser(sess.UserID)
			if err == nil && user != nil {
				response["user"] = sessionUserPayload(user)
			}
		}

		// Clean up the session after it's been consumed
		_ = s.store.DeleteCLIAuthSession(code)
	}

	writeJSON(w, http.StatusOK, response)
}

// handleApproveCLIAuthSession approves a pending CLI auth session.
// Called from the browser by an authenticated user.
// POST /api/v1/auth/cli/sessions/{code}/approve
func (s *Server) handleApproveCLIAuthSession(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if code == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Missing session code")
		return
	}

	// Resolve the authenticated user — try context first (set by middleware),
	// then fall back to session cookie (since auth routes are exempt from middleware).
	user := currentUser(r)
	if user == nil {
		user = s.validateSessionCookie(r)
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "You must be logged in to approve a CLI session")
		return
	}

	// Verify the session exists and is pending
	sess, err := s.store.GetCLIAuthSession(code)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check CLI auth session")
		return
	}
	if sess == nil {
		writeError(w, http.StatusNotFound, "not_found", "CLI auth session not found")
		return
	}
	if sess.Status == "expired" {
		writeError(w, http.StatusGone, "expired", "This CLI auth session has expired. Run 'pad auth login' again.")
		return
	}
	if sess.Status == "approved" {
		writeError(w, http.StatusConflict, "already_approved", "This CLI session has already been approved")
		return
	}

	// Create a new session token for the CLI (long-lived, 30 days)
	token, err := s.store.CreateSession(user.ID, "cli-browser-auth", clientIP(r), "", 30*24*time.Hour)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to create session")
		return
	}

	// Approve the CLI auth session with the new token
	if err := s.store.ApproveCLIAuthSession(code, token, user.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to approve CLI auth session")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"approved": true,
		"user":     sessionUserPayload(user),
	})
}
