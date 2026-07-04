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

// authCSRFUnconditionalExemptPaths lists the exact /api/v1/auth/* paths
// whose authority comes entirely from the request BODY — credentials, a
// one-time token, or a shared secret — never from the ambient session
// cookie (B8, TASK-1932). A cookie riding alongside one of these requests
// doesn't escalate what the handler does, so these stay exempt from the
// double-submit CSRF check REGARDLESS of whether SessionAuth also
// resolved a user for the request.
//
// This isn't just a convenience: pad mints the pad_csrf cookie AT LOGIN
// (createAuthSession), so a pre-session endpoint like /login or
// /bootstrap categorically CANNOT require a CSRF token that doesn't exist
// yet. And a lingering session cookie from an earlier login (an E2E test
// harness re-bootstrapping the admin then logging in fresh, the pad CLI,
// a browser tab that re-authenticates as a different account, or simply
// a stale cookie sitting alongside a fresh login POST) must not turn that
// legitimate re-login into a CSRF failure. codex round 4 (via CI's E2E
// suite, not static review — the gap the unit suite was missing) caught
// exactly this: gating the WHOLE list on currentUser(r) == nil (round 2's
// fix) broke /login whenever an ambient cookie was present, because
// login's authority is the password in the body, not the cookie.
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
//   - "/api/v1/auth/register" is deliberately NOT here — see
//     authCSRFSessionGatedExemptPaths below.
//
// Requests carrying a Bearer credential that TokenAuth actually VALIDATED
// (a PAT or a CLI session-bearer token) are unaffected by any of this —
// they hit isValidatedBearerAuth's unconditional exemption further down
// this function regardless of path or cookie presence; see that check's
// comment for the codex-round-3 validated-vs-present distinction.
var authCSRFUnconditionalExemptPaths = map[string]bool{
	"/api/v1/auth/bootstrap":           true,
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

// authCSRFSessionGatedExemptPaths lists /api/v1/auth/* paths that are
// exempt from CSRF ONLY when the request carries no session — i.e. only
// when currentUser(r) == nil at the call site below. Unlike the
// unconditional set above, these handlers have an ALTERNATE authorization
// branch that DOES read the ambient session cookie, so exempting them
// unconditionally would let a cross-site request ride a victim's cookie
// into that branch with no CSRF token.
//
// register is the only entry: handleRegister is genuinely anonymous most
// of the time (self-serve cloud signup, invitation acceptance), but it
// also has an admin-session branch (an already-logged-in admin can
// create a verified account with no invitation code) — codex round 2
// found that a plain path exemption let a cross-site POST ride the
// admin's cookie straight into that branch with no CSRF token, the same
// class of hole as oauth-unlink. Gating on currentUser(r) == nil closes
// that branch (a session-authenticated register falls through to the
// real double-submit check) while a genuinely anonymous register keeps
// the exemption. This is deliberately its OWN map (not folded into the
// unconditional set with a blanket currentUser guard, which is what
// round 2 originally did and what round 4's CI-caught regression forced
// apart): login/bootstrap/etc. have no session-privileged branch at all,
// so gating them on currentUser(r) == nil was never a security
// requirement — it just broke legitimate re-login-with-a-lingering-cookie
// flows for no benefit.
var authCSRFSessionGatedExemptPaths = map[string]bool{
	"/api/v1/auth/register": true,
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

		// Auth endpoints whose authority comes entirely from the request
		// body (credentials, a one-time token, a shared secret) — never
		// from the ambient session cookie — stay exempt regardless of
		// whether SessionAuth also resolved a user for this request. See
		// authCSRFUnconditionalExemptPaths for the full per-entry
		// rationale and the round-4 CI regression this split fixes
		// (B8, TASK-1932).
		if authCSRFUnconditionalExemptPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		// register is exempt ONLY when unauthenticated: handleRegister
		// has an admin-session branch that DOES derive authority from the
		// ambient cookie (codex round 2), so a session-authenticated
		// request falls through to the real double-submit check below
		// instead of this exemption, while a genuinely anonymous request
		// keeps it. See authCSRFSessionGatedExemptPaths's doc comment.
		if authCSRFSessionGatedExemptPaths[r.URL.Path] && currentUser(r) == nil {
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
		// whole tree, far wider than the exempt-path maps above), not just
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
