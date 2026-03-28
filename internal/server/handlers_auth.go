package server

import (
	"crypto/subtle"
	"net/http"
	"time"
)

// handleLogin validates the password and creates a session.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.password == "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":            true,
			"auth_required": false,
		})
		return
	}

	var input struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	// Constant-time comparison to prevent timing attacks
	if subtle.ConstantTimeCompare([]byte(input.Password), []byte(s.password)) != 1 {
		// Slow down brute force attempts
		time.Sleep(500 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid password")
		return
	}

	// Create session
	cookieValue := s.sessions.Create()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    cookieValue,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Secure: false — not using HTTPS for localhost
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true,
	})
}

// handleSessionCheck returns current auth status. This endpoint is exempt from auth middleware.
func (s *Server) handleSessionCheck(w http.ResponseWriter, r *http.Request) {
	if s.password == "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"authenticated": true,
			"auth_required": false,
		})
		return
	}

	authenticated := false
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		authenticated = s.sessions.Validate(cookie.Value)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"authenticated": authenticated,
		"auth_required": true,
	})
}

// handleLogout destroys the session and clears the cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Destroy(cookie.Value)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true,
	})
}
