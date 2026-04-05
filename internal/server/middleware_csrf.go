package server

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

const (
	csrfCookie = "pad_csrf"
	csrfHeader = "X-CSRF-Token"
	csrfTokenLen = 32 // 32 bytes = 64 hex chars
)

// CSRFProtect implements the double-submit cookie pattern for CSRF protection.
// It validates that state-changing requests (POST, PATCH, PUT, DELETE) from
// cookie-authenticated sessions include a matching CSRF token in both the
// cookie and the X-CSRF-Token header.
//
// Requests authenticated via Bearer tokens (API tokens / CLI) are exempt
// because they are not vulnerable to CSRF attacks — the browser never
// attaches Authorization headers automatically.
//
// Safe methods (GET, HEAD, OPTIONS) are always allowed through.
func (s *Server) CSRFProtect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Safe methods are exempt
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		// Non-API paths are exempt (SPA static files, etc.)
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		// Auth endpoints that need to work before a CSRF token exists
		// (login, register, bootstrap, password reset)
		if strings.HasPrefix(r.URL.Path, "/api/v1/auth/") {
			next.ServeHTTP(w, r)
			return
		}

		// Bearer token requests are not vulnerable to CSRF — skip
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			next.ServeHTTP(w, r)
			return
		}

		// No users exist (fresh install) — skip CSRF
		count, err := s.store.UserCount()
		if err != nil || count == 0 {
			next.ServeHTTP(w, r)
			return
		}

		// Cookie-based session: require CSRF token
		cookie, err := r.Cookie(csrfCookie)
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusForbidden, "csrf_error", "Missing CSRF token")
			return
		}

		headerToken := r.Header.Get(csrfHeader)
		if headerToken == "" {
			writeError(w, http.StatusForbidden, "csrf_error", "Missing CSRF header")
			return
		}

		if cookie.Value != headerToken {
			writeError(w, http.StatusForbidden, "csrf_error", "CSRF token mismatch")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// setCSRFCookie writes a new CSRF token cookie. The cookie is NOT HttpOnly
// so that JavaScript can read it and send it back as a header.
func setCSRFCookie(w http.ResponseWriter, ttl int) {
	token := generateCSRFToken()
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   ttl,
		HttpOnly: false, // Must be readable by JS
		SameSite: http.SameSiteLaxMode,
	})
}

// clearCSRFCookie removes the CSRF cookie (e.g. on logout).
func clearCSRFCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	})
}

// generateCSRFToken returns a cryptographically random hex string.
func generateCSRFToken() string {
	b := make([]byte, csrfTokenLen)
	if _, err := rand.Read(b); err != nil {
		panic("csrf: failed to generate random token: " + err.Error())
	}
	return hex.EncodeToString(b)
}
