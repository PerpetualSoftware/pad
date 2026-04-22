package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

const (
	csrfHeader   = "X-CSRF-Token"
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
		// (login, register, bootstrap, password reset).
		if strings.HasPrefix(r.URL.Path, "/api/v1/auth/") {
			next.ServeHTTP(w, r)
			return
		}

		// Cloud sidecar calls bypass CSRF because they don't use cookie-
		// based sessions; they authenticate via X-Cloud-Secret (or legacy
		// ?cloud_secret). Path-gate this explicitly so a stray
		// ?cloud_secret= on any other /api/ path (trivial in a cross-site
		// form action) cannot be used to defeat CSRF elsewhere. Admin calls
		// over cookie sessions to the same three endpoints fall through
		// and still require a CSRF token — that is the entire point of
		// narrowing from a path carve-out to a credential-plus-path check.
		if isCloudAdminPath(r.URL.Path) && hasCloudSecretMarker(r) {
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
		cookie, err := r.Cookie(csrfCookieName(s.secureCookies))
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusForbidden, "csrf_error", "Missing CSRF token")
			return
		}

		headerToken := r.Header.Get(csrfHeader)
		if headerToken == "" {
			writeError(w, http.StatusForbidden, "csrf_error", "Missing CSRF header")
			return
		}

		// Lengths must match AND the bytes must match, evaluated in
		// constant time. subtle.ConstantTimeCompare returns 0 on length
		// mismatch too — we check separately so the length-mismatch
		// branch doesn't short-circuit before the compare (Go's == would
		// return early on length, leaking timing signal about how many
		// leading bytes are correct).
		cookieBytes, headerBytes := []byte(cookie.Value), []byte(headerToken)
		if len(cookieBytes) != len(headerBytes) ||
			subtle.ConstantTimeCompare(cookieBytes, headerBytes) != 1 {
			writeError(w, http.StatusForbidden, "csrf_error", "CSRF token mismatch")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// setCSRFCookie writes a new CSRF token cookie. The cookie is NOT HttpOnly
// so that JavaScript can read it and send it back as a header.
func setCSRFCookie(w http.ResponseWriter, ttl int, secure bool) {
	token := generateCSRFToken()
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName(secure),
		Value:    token,
		Path:     "/",
		MaxAge:   ttl,
		HttpOnly: false, // Must be readable by JS
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearCSRFCookie removes the CSRF cookie (e.g. on logout).
// Must clear both prefixed and unprefixed names to handle upgrades cleanly.
func clearCSRFCookie(w http.ResponseWriter) {
	for _, name := range []string{"pad_csrf", "__Host-pad_csrf"} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: false,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

// generateCSRFToken returns a cryptographically random hex string.
func generateCSRFToken() string {
	b := make([]byte, csrfTokenLen)
	if _, err := rand.Read(b); err != nil {
		panic("csrf: failed to generate random token: " + err.Error())
	}
	return hex.EncodeToString(b)
}
