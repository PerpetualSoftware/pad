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
// exempt from the double-submit CSRF check WHEN THE REQUEST IS
// UNAUTHENTICATED (B8, TASK-1932; tightened for a P1 in codex round 2 — see
// below). Every other mutating /api/v1/auth/* endpoint is cookie-session-
// gated and requires the CSRF token like any other authenticated mutation —
// the web client's shared request() wrapper already attaches X-CSRF-Token
// on every non-GET/HEAD call, so narrowing this list doesn't require a
// frontend change.
//
// The exemption is conditioned on currentUser(r) == nil (checked at the
// call site below), not just the path. codex round 2 caught that
// /auth/register is genuinely anonymous MOST of the time, but
// handleRegister also has an admin-session branch (an already-logged-in
// admin can create a verified account with no invitation code) — a plain
// path exemption let a cross-site POST ride the admin's cookie straight
// into that branch with no CSRF token, the same class of hole as the
// oauth-unlink case B8 already closed. Gating on currentUser(r) == nil
// closes register's admin branch, and any other session-authenticated
// branch a future handler on this list grows, without touching handler
// code: a request that resolved to a real session via SessionAuth (which
// runs before CSRFProtect — server.go's TokenAuth/SessionAuth/RateLimit/
// CSRFProtect/RequireAuth ordering) falls through to the normal
// double-submit check below instead of the early exemption. Requests
// carrying a Bearer credential that TokenAuth actually VALIDATED (a PAT
// or a CLI session-bearer token) are unaffected — they hit
// isValidatedBearerAuth's unconditional exemption further down this
// function regardless of whether a cookie is also present, because
// TokenAuth runs first and short-circuits SessionAuth once currentUser is
// already set. A Bearer header TokenAuth did NOT validate (garbage or
// rejected, reaching here only via rejectInvalidBearer's public-path
// fallthrough) is a DIFFERENT case — see the header-presence check
// further down and its codex-round-3 fix note; it is exempt only when no
// session was also resolved, precisely so it can't ride a victim's cookie
// through this path exemption. The pad-cloud sidecar sends no cookies at
// all, so oauth-login/oauth-link stay exempt (currentUser(r) is always
// nil for them). One accepted interaction with the round-1 legacy-cookie
// decision: a user resolved via SessionAuth's legacy pad_session cookie
// fallback also loses this exemption and needs a real CSRF token to
// re-hit login/register — harmless, since they're already logged in and
// the frontend sends the header anyway; an EXPIRED or absent legacy
// session resolves to currentUser(r) == nil and stays exempt, so there's
// no lockout path for a genuinely anonymous visitor.
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
		//
		// The currentUser(r) == nil guard closes a P1 codex found in
		// round 2: handleRegister has an admin-session branch (an
		// already-logged-in admin can create a verified account with no
		// invitation code), so exempting the bare path let a cross-site
		// POST ride the admin's cookie into that branch with no CSRF
		// token. SessionAuth runs before CSRFProtect, so currentUser(r)
		// is already populated for any request carrying a valid session
		// — gating on it here means a session-authenticated request to
		// one of these paths falls through to the real double-submit
		// check below, while a genuinely anonymous request (no session,
		// including one that failed the round-1 legacy-cookie decision)
		// keeps the exemption. See authCSRFExemptPaths's doc comment for
		// the full reasoning, including why Bearer/cloud-secret callers
		// are unaffected.
		if authCSRFExemptPaths[r.URL.Path] && currentUser(r) == nil {
			next.ServeHTTP(w, r)
			return
		}

		// Cloud sidecar calls bypass CSRF because they don't use cookie-
		// based sessions; they authenticate via X-Cloud-Secret (or legacy
		// ?cloud_secret). Path-gate this explicitly so a stray
		// ?cloud_secret= on any other /api/ path (trivial in a cross-site
		// form action) cannot be used to defeat CSRF elsewhere.
		//
		// hasCloudSecretMarker only checks PRESENCE of the marker, not
		// whether the secret is actually correct — the real check
		// (validateCloudSecret) happens in-handler. codex round 3 caught
		// that several of these handlers (e.g. handleSetPlan) ALSO accept
		// an admin cookie session as an alternative to the secret
		// (`isAdmin := user != nil && ...; if !isAdmin { validateCloudSecret(...) }`)
		// — so a marker-presence-only exemption let a cross-site request
		// with a garbage X-Cloud-Secret ride a logged-in admin's cookie
		// straight past CSRF into that admin branch, the same class of
		// hole as handleRegister's in round 2. The currentUser(r) == nil
		// guard closes it: a request that also resolved a real session
		// falls through to the normal double-submit check below, so the
		// admin branch can only be reached with either the real secret or
		// a valid CSRF token — never a forged marker riding a stolen
		// cookie. The pad-cloud sidecar sends no cookies, so its calls are
		// unaffected (currentUser(r) is always nil for them).
		if isCloudAdminPath(r.URL.Path) && hasCloudSecretMarker(r) && currentUser(r) == nil {
			next.ServeHTTP(w, r)
			return
		}

		// Bearer credentials TokenAuth actually validated (a PAT or a CLI
		// session-bearer token, or a synthesized in-process MCP-dispatcher
		// call marked via server.WithAPITokenAuth) are never vulnerable to
		// CSRF — the browser never attaches Authorization headers
		// automatically, and the attacker can't forge the token value.
		// Always exempt, regardless of whether a cookie is also present:
		// TokenAuth runs before SessionAuth and short-circuits it once
		// currentUser is set, so a validated Bearer credential is the
		// sole source of truth for currentUser on this request either way.
		if isValidatedBearerAuth(r) {
			next.ServeHTTP(w, r)
			return
		}

		// An Authorization: Bearer header TokenAuth did NOT validate
		// (garbage token, wrong format, expired — anything that hit
		// rejectInvalidBearer's public-API-path fallthrough instead of a
		// hard 401) is exempt ONLY when no session was also resolved for
		// this request. codex round 3: the old unconditional version of
		// this check exempted on header PRESENCE alone, so a cross-site
		// request carrying a victim's real session cookie plus an
		// attacker-supplied garbage Bearer header would sail past CSRF —
		// on any /api/v1/auth/* path (isPublicAPIPath prefix-matches the
		// whole tree, far wider than authCSRFExemptPaths above), not just
		// the newly-tightened B8 endpoints. Gating on currentUser(r) ==
		// nil preserves the CLI-recovery contract this fallthrough exists
		// for (a pure Bearer client with an expired/garbage token, no
		// cookie, must still reach RequireAuth for its own 401 rather than
		// a confusing csrf_error) while closing the cookie-riding case.
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") && currentUser(r) == nil {
			next.ServeHTTP(w, r)
			return
		}

		// No users exist (fresh install) — skip CSRF
		count, err := s.store.UserCount()
		if err != nil || count == 0 {
			next.ServeHTTP(w, r)
			return
		}

		// Cookie-based session: require CSRF token.
		//
		// Deliberately NO legacy-name fallback here, unlike SessionAuth's
		// __Host-pad_session -> pad_session fallback (middleware_auth.go)
		// for the session cookie. The two cookies have different trust
		// requirements:
		//   - Session cookie: the token VALUE is an unguessable secret: an
		//     attacker who can only set the unprefixed name still can't
		//     forge a value the store will accept. Accepting the legacy
		//     name is just an upgrade-path convenience.
		//   - CSRF cookie: double-submit's security property is "the
		//     attacker cannot set BOTH the cookie and the header" for the
		//     victim's browser. An unprefixed pad_csrf cookie can be set
		//     from a sibling subdomain (no __Host- restriction), so an
		//     attacker who controls a subdomain could plant a
		//     matching pad_csrf cookie + forge the matching header value
		//     themselves — defeating the whole scheme. __Host- exists
		//     specifically to close that subdomain-cookie-injection hole,
		//     so accepting the unprefixed name in secure mode would
		//     silently reopen it. Do not "restore symmetry" with
		//     SessionAuth here; the asymmetry is the point.
		//
		// Exposure from the lack of fallback is bounded to deployments
		// that recently flipped PAD_SECURE_COOKIES on: an already-logged-in
		// browser holding legacy pad_session + pad_csrf cookies still
		// authenticates (session fallback) but 403s on CSRF-required
		// mutations until the next login mints __Host--prefixed cookies —
		// self-heals within the session TTL. See
		// TestCSRF_LegacyCookieFallback_NotAcceptedWhenSecure.
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
