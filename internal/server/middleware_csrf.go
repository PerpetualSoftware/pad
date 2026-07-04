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

// authCSRFExemptPaths lists the exact /api/v1/auth/* paths that are safe to
// exempt from the double-submit CSRF check (B8, TASK-1932). Every other
// mutating /api/v1/auth/* endpoint is cookie-session-gated and requires the
// CSRF token like any other authenticated mutation — the web client's
// shared request() wrapper already attaches X-CSRF-Token on every
// non-GET/HEAD call, so narrowing this list doesn't require a frontend
// change.
//
// This is an EXACT path match (not a prefix), on purpose: a prefix match is
// exactly the bug B8 flags. In particular:
//   - "/api/v1/auth/cli/sessions" (POST, create) is here because it's
//     genuinely pre-session (the CLI calls it before any login exists).
//     "/api/v1/auth/cli/sessions/{code}/approve" is a DIFFERENT path
//     string and is deliberately NOT here — it's a mutating, cookie-
//     session-authenticated action (approving a pending CLI login) that a
//     cross-site POST could otherwise ride the victim's session to
//     trigger. The GET poll endpoint doesn't need an entry — safe methods
//     are exempt above regardless of path.
//   - "/api/v1/auth/oauth-login" and "/api/v1/auth/oauth-link" are here
//     because they authenticate ONLY via a cloud-secret in the JSON body
//     (the pad-cloud sidecar calling in server-to-server) and never trust
//     a cookie at all — CSRF isn't a meaningful threat model for them, and
//     the sidecar has no CSRF cookie to send. "/api/v1/auth/oauth-unlink"
//     is deliberately NOT here — unlike link/login, it authenticates
//     purely via the session cookie (currentUser(r), no secret check) and
//     is exactly the kind of endpoint this hardening targets.
//   - "/api/v1/auth/resend-verification" is here because the handler
//     itself never inspects the session — it takes an explicit email and
//     returns a uniform response regardless of auth state, so an ambient
//     cookie carries no extra authority for it and CSRF is a no-op either
//     way.
//   - "/api/v1/auth/logout" is deliberately NOT here — it's a real,
//     cookie-session-authenticated mutation, and the frontend already
//     sends CSRF on it via the shared request() wrapper.
var authCSRFExemptPaths = map[string]bool{
	"/api/v1/auth/bootstrap":           true,
	"/api/v1/auth/register":            true,
	"/api/v1/auth/login":               true,
	"/api/v1/auth/forgot-password":     true,
	"/api/v1/auth/reset-password":      true,
	"/api/v1/auth/local-reset":         true,
	"/api/v1/auth/verify-email":        true,
	"/api/v1/auth/resend-verification": true,
	"/api/v1/auth/2fa/login-verify":    true,
	"/api/v1/auth/oauth-login":         true,
	"/api/v1/auth/oauth-link":          true,
	"/api/v1/auth/cli/sessions":        true,
}

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
		// (login, register, bootstrap, password reset, ...) or that don't
		// trust the ambient session at all. See authCSRFExemptPaths for
		// the full rationale per entry — this is an EXACT match, not a
		// prefix, so a mutating cookie-authed endpoint elsewhere under
		// /api/v1/auth/* (PATCH /me, tokens, 2FA management, delete-
		// account, oauth-unlink, CLI-session approve, logout, ...) still
		// falls through to the CSRF check below like any other
		// authenticated mutation (B8, TASK-1932).
		if authCSRFExemptPaths[r.URL.Path] {
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

		// Bearer token requests are not vulnerable to CSRF — skip.
		// Two signals, both mean "non-cookie-authenticated":
		//   1. Authorization: Bearer header on the live request (the
		//      normal CLI / connector path through TokenAuth).
		//   2. ctxIsAPIToken set on the request context — TokenAuth
		//      sets this after a successful Bearer validation, AND
		//      in-process callers (the MCP HTTPHandlerDispatcher in
		//      internal/mcp/dispatch_http.go) set it via the
		//      server.WithAPITokenAuth helper to mark a synthesized
		//      request as having been authenticated out-of-band.
		// Either signal lets the request through; cookie-only
		// requests still require the double-submit token below.
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			next.ServeHTTP(w, r)
			return
		}
		if isAPITokenAuth(r) {
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

		// CSRF tokens are fixed-size hex strings (csrfTokenLen bytes →
		// csrfTokenLen*2 hex chars). Reject any token that doesn't
		// match the expected length BEFORE allocating. Without this
		// an attacker could flood with equally-sized cookie + header
		// pairs (within the 64 KiB MaxHeaderBytes cap) and each
		// failing request would allocate the []byte copies below
		// proportional to the header size — cheap per request, but
		// a noticeable GC cost under sustained load. Post-check,
		// both values are bounded to csrfTokenLen*2 bytes so the
		// allocation is a fixed, tiny cost.
		const expectedLen = csrfTokenLen * 2 // hex encoding
		if len(cookie.Value) != expectedLen || len(headerToken) != expectedLen {
			writeError(w, http.StatusForbidden, "csrf_error", "CSRF token mismatch")
			return
		}
		// subtle.ConstantTimeCompare evaluates the byte compare in time
		// independent of where the first differing byte lives, removing
		// the timing side-channel that the previous `!=` had.
		if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(headerToken)) != 1 {
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
