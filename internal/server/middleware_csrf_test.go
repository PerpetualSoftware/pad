package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCSRF_SafeMethodsAllowed(t *testing.T) {
	srv := testServer(t)

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		req := httptest.NewRequest(method, "/api/v1/workspaces", nil)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)

		if w.Code == http.StatusForbidden {
			body := w.Body.String()
			if strings.Contains(body, "csrf") || strings.Contains(body, "CSRF") {
				t.Errorf("%s should not be blocked by CSRF, got 403 with body: %s", method, body)
			}
		}
	}
}

func TestCSRF_BearerTokenExempt(t *testing.T) {
	srv := testServer(t)

	// Bootstrap admin so auth is required
	token := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	// POST with Bearer token (session token), no CSRF — should NOT get CSRF error
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces",
		strings.NewReader(`{"name":"test"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code == http.StatusForbidden {
		body := w.Body.String()
		if strings.Contains(body, "csrf") || strings.Contains(body, "CSRF") {
			t.Errorf("Bearer token request should be CSRF-exempt, got 403: %s", body)
		}
	}
}

func TestCSRF_AuthEndpointsExempt(t *testing.T) {
	srv := testServer(t)

	// Auth endpoints should work without CSRF token. logout is
	// deliberately NOT in this list — B8 (TASK-1932) moved it into the
	// CSRF-required set since it's a real cookie-session-authenticated
	// mutation; see TestCSRF_AuthMutatingEndpoints_RequireCSRF.
	endpoints := []string{
		"/api/v1/auth/login",
		"/api/v1/auth/register",
		"/api/v1/auth/forgot-password",
		"/api/v1/auth/reset-password",
	}

	for _, ep := range endpoints {
		req := httptest.NewRequest(http.MethodPost, ep, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.1:1234"
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)

		if w.Code == http.StatusForbidden {
			body := w.Body.String()
			if strings.Contains(body, "csrf") || strings.Contains(body, "CSRF") {
				t.Errorf("%s should be CSRF-exempt, got 403: %s", ep, body)
			}
		}
	}
}

func TestCSRF_MissingTokenBlocked(t *testing.T) {
	srv := testServer(t)

	// Bootstrap and login to get session token
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	sessionToken := loginUser(t, srv, "admin@test.com", "correct-horse-battery-staple")

	// POST with session cookie but no CSRF token at all
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces",
		strings.NewReader(`{"name":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for missing CSRF token, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCSRF_MismatchBlocked(t *testing.T) {
	srv := testServer(t)

	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	sessionToken := loginUser(t, srv, "admin@test.com", "correct-horse-battery-staple")

	// POST with session cookie + CSRF cookie but WRONG header value
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces",
		strings.NewReader(`{"name":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde0")
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
	req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for CSRF mismatch, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCSRF_MatchingTokenAllowed(t *testing.T) {
	srv := testServer(t)

	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	sessionToken := loginUser(t, srv, "admin@test.com", "correct-horse-battery-staple")

	// POST with matching CSRF cookie + header. Value must be the
	// expected csrfTokenLen*2 hex chars so the middleware accepts it.
	csrfVal := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces",
		strings.NewReader(`{"name":"csrftest"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrfVal)
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
	req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: csrfVal})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	// Should NOT be blocked by CSRF
	if w.Code == http.StatusForbidden {
		body := w.Body.String()
		if strings.Contains(body, "csrf") || strings.Contains(body, "CSRF") {
			t.Errorf("matching CSRF token should be allowed, got 403: %s", body)
		}
	}
	// Workspace create should succeed (201)
	if w.Code != http.StatusCreated {
		t.Errorf("expected 201 for workspace create with valid CSRF, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCSRF_FreshInstallExempt(t *testing.T) {
	srv := testServer(t)

	// No users → fresh install → CSRF should be skipped
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces",
		strings.NewReader(`{"name":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code == http.StatusForbidden {
		body := w.Body.String()
		if strings.Contains(body, "csrf") || strings.Contains(body, "CSRF") {
			t.Errorf("fresh install should be CSRF-exempt, got 403: %s", body)
		}
	}
}

func TestCSRF_LoginSetsCSRFCookie(t *testing.T) {
	srv := testServer(t)

	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	// Login
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"email":"admin@test.com","password":"correct-horse-battery-staple"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", w.Code, w.Body.String())
	}

	// Check that CSRF cookie is set
	var foundCSRF bool
	for _, c := range w.Result().Cookies() {
		if c.Name == "pad_csrf" {
			foundCSRF = true
			if c.HttpOnly {
				t.Error("CSRF cookie must not be HttpOnly")
			}
			if c.Value == "" {
				t.Error("CSRF cookie value must not be empty")
			}
		}
	}
	if !foundCSRF {
		t.Error("login response should set pad_csrf cookie")
	}
}

func TestCSRF_LogoutClearsCSRFCookie(t *testing.T) {
	srv := testServer(t)

	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	sessionToken := loginUser(t, srv, "admin@test.com", "correct-horse-battery-staple")

	// logout moved into the CSRF-required set in B8 (TASK-1932) — it's a
	// real cookie-session-authenticated mutation, and the web client
	// already sends the token on it. A valid double-submit pair is
	// required for the request to reach the handler at all.
	csrfVal := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.Header.Set("X-CSRF-Token", csrfVal)
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
	req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: csrfVal})
	req.RemoteAddr = "192.0.2.1:1234"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("logout with valid CSRF token: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Check CSRF cookie is cleared
	var foundClearedCSRF bool
	for _, c := range w.Result().Cookies() {
		if c.Name == "pad_csrf" {
			foundClearedCSRF = true
			if c.MaxAge >= 0 {
				t.Errorf("expected CSRF cookie to be cleared (MaxAge < 0), got MaxAge=%d", c.MaxAge)
			}
		}
	}
	if !foundClearedCSRF {
		t.Error("expected logout response to clear the pad_csrf cookie")
	}
}

// TestCSRF_AuthMutatingEndpoints_RequireCSRF covers B8 (TASK-1932): every
// cookie-session-authenticated mutation under /api/v1/auth/* — not just the
// generic /api/v1/* surface — must require the double-submit CSRF token.
// Before B8 these were all bypassed by a blanket /api/v1/auth/ prefix
// exemption.
func TestCSRF_AuthMutatingEndpoints_RequireCSRF(t *testing.T) {
	srv := testServer(t)

	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	sessionToken := loginUser(t, srv, "admin@test.com", "correct-horse-battery-staple")

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"logout", http.MethodPost, "/api/v1/auth/logout", ""},
		{"update profile", http.MethodPatch, "/api/v1/auth/me", `{"name":"New Name"}`},
		{"oauth-unlink", http.MethodPost, "/api/v1/auth/oauth-unlink", `{"provider":"github"}`},
		{"2fa setup", http.MethodPost, "/api/v1/auth/2fa/setup", ""},
		{"2fa disable", http.MethodPost, "/api/v1/auth/2fa/disable", `{"password":"x"}`},
		{"delete account", http.MethodPost, "/api/v1/auth/delete-account", `{"password":"x"}`},
		{"create token", http.MethodPost, "/api/v1/auth/tokens", `{"name":"t"}`},
		{"delete token", http.MethodDelete, "/api/v1/auth/tokens/some-id", ""},
		{"rotate token", http.MethodPost, "/api/v1/auth/tokens/some-id/rotate", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var bodyReader *strings.Reader
			if tc.body != "" {
				bodyReader = strings.NewReader(tc.body)
			} else {
				bodyReader = strings.NewReader("")
			}
			req := httptest.NewRequest(tc.method, tc.path, bodyReader)
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			req.RemoteAddr = "192.0.2.1:1234"
			req.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
			// Deliberately NO CSRF cookie/header.
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("%s %s without CSRF token: expected 403, got %d: %s", tc.method, tc.path, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "csrf") {
				t.Errorf("%s %s: expected a csrf_error response, got: %s", tc.method, tc.path, w.Body.String())
			}
		})
	}
}

// TestCSRF_CLISessionApprove_RequiresCSRF covers the specific finding that
// approving a pending CLI login (a cookie-session-authenticated mutation
// that could otherwise be ridden by a cross-site POST to bind a victim's
// session to an attacker-controlled CLI code) must require CSRF, while the
// bare create/poll endpoints stay exempt/safe-method.
func TestCSRF_CLISessionApprove_RequiresCSRF(t *testing.T) {
	srv := testServer(t)

	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	sessionToken := loginUser(t, srv, "admin@test.com", "correct-horse-battery-staple")

	// Create a pending CLI auth session (exempt, pre-session by design).
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/cli/sessions", nil)
	createReq.RemoteAddr = "192.0.2.1:1234"
	createW := httptest.NewRecorder()
	srv.ServeHTTP(createW, createReq)
	if createW.Code != http.StatusOK {
		t.Fatalf("create CLI session: expected 200, got %d: %s", createW.Code, createW.Body.String())
	}
	var created struct {
		SessionCode string `json:"session_code"`
	}
	parseJSON(t, createW, &created)
	if created.SessionCode == "" {
		t.Fatal("expected a session_code from CLI session create")
	}

	// Approve without a CSRF token must be blocked.
	approveReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/cli/sessions/"+created.SessionCode+"/approve", nil)
	approveReq.RemoteAddr = "192.0.2.1:1234"
	approveReq.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
	approveW := httptest.NewRecorder()
	srv.ServeHTTP(approveW, approveReq)
	if approveW.Code != http.StatusForbidden {
		t.Fatalf("approve without CSRF token: expected 403, got %d: %s", approveW.Code, approveW.Body.String())
	}

	// Approve with a valid double-submit pair must succeed.
	csrfVal := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	approveReq2 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/cli/sessions/"+created.SessionCode+"/approve", nil)
	approveReq2.Header.Set("X-CSRF-Token", csrfVal)
	approveReq2.RemoteAddr = "192.0.2.1:1234"
	approveReq2.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
	approveReq2.AddCookie(&http.Cookie{Name: "pad_csrf", Value: csrfVal})
	approveW2 := httptest.NewRecorder()
	srv.ServeHTTP(approveW2, approveReq2)
	if approveW2.Code != http.StatusOK {
		t.Fatalf("approve with valid CSRF token: expected 200, got %d: %s", approveW2.Code, approveW2.Body.String())
	}
}

// TestCSRF_TrailingSlashVariant_OfExemptPath_NotExempted guards the exact-
// match design of authCSRFUnconditionalExemptPaths: a trailing-slash
// variant of an otherwise-exempt path (e.g. "/api/v1/auth/login/") must
// NOT inherit the bare path's exemption via prefix-matching or path
// normalization. Uses login (unconditionally exempt) rather than register
// (session-gated) so this test isolates the exact-match property from the
// currentUser(r) == nil gate register alone carries.
func TestCSRF_TrailingSlashVariant_OfExemptPath_NotExempted(t *testing.T) {
	srv := testServer(t)

	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login/", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	// No session cookie either — the point is that the trailing slash
	// alone must not carry the exemption, independent of session state.
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "csrf") {
		t.Errorf("trailing-slash variant of an unconditionally exempt path must still require CSRF, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCSRF_CLISessionsSubpath_DoesNotInheritCreateExemption guards the
// other direction of the same exact-match design: the exempt bare
// "/api/v1/auth/cli/sessions" (create) entry must not leak, via prefix
// matching, to the required "/api/v1/auth/cli/sessions/{code}/approve"
// sub-path.
func TestCSRF_CLISessionsSubpath_DoesNotInheritCreateExemption(t *testing.T) {
	srv := testServer(t)

	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	sessionToken := loginUser(t, srv, "admin@test.com", "correct-horse-battery-staple")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/cli/sessions/some-code/approve", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
	// Deliberately no CSRF cookie/header.
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "csrf") {
		t.Errorf("cli/sessions/{code}/approve must not inherit the bare create path's CSRF exemption, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCSRF_AllMutationMethodsBlocked(t *testing.T) {
	srv := testServer(t)

	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	sessionToken := loginUser(t, srv, "admin@test.com", "correct-horse-battery-staple")

	// All state-changing methods should be blocked without CSRF
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/v1/workspaces",
			strings.NewReader(`{"name":"test"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.1:1234"
		req.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("%s without CSRF should be 403, got %d", method, w.Code)
		}
	}
}

// TestCSRF_LegacyCookieFallback_NotAcceptedWhenSecure pins a deliberate
// asymmetry surfaced in codex review of TASK-1932: SessionAuth falls back
// from the __Host--prefixed session cookie to the legacy unprefixed name
// (upgrade-path convenience — the token VALUE is still an unguessable
// secret either way), but the CSRF cookie lookup has NO such fallback. A
// legacy unprefixed pad_csrf cookie is settable from a sibling subdomain,
// which is exactly the hole __Host- exists to close for double-submit's
// security property (attacker must not be able to set both the cookie and
// the header). Accepting it here would silently reopen that hole for any
// deployment that recently flipped PAD_SECURE_COOKIES on.
//
// This test simulates that exact deployment moment: secureCookies=true,
// but the request still carries legacy-named session AND CSRF cookies (as
// a browser that logged in before the flip would). The session must still
// resolve (auth fallback intentionally works), but a CSRF-required
// mutation must still be rejected (no CSRF fallback) until the browser
// re-logs-in and receives __Host--prefixed cookies.
func TestCSRF_LegacyCookieFallback_NotAcceptedWhenSecure(t *testing.T) {
	srv := testServer(t)

	// Bootstrap and log in while secureCookies is still false, so the
	// login response mints legacy-named "pad_session" / "pad_csrf"
	// cookies — standing in for a browser that authenticated before the
	// deployment enabled secure cookies.
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	sessionToken := loginUser(t, srv, "admin@test.com", "correct-horse-battery-staple")

	// Now simulate the deployment flipping PAD_SECURE_COOKIES on. The
	// browser's existing legacy cookies don't change.
	srv.secureCookies = true

	csrfVal := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/auth/me", strings.NewReader(`{"name":"New Name"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrfVal)
	req.RemoteAddr = "192.0.2.1:1234"
	// Legacy unprefixed cookie names — no __Host- prefix, as an
	// already-logged-in browser would still be presenting post-flip.
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
	req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: csrfVal})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("legacy pad_csrf cookie must NOT be accepted once secureCookies is true, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "csrf") {
		t.Errorf("expected a csrf_error response, got: %s", w.Body.String())
	}
}

// TestCSRF_AdminSessionRegister_RequiresCSRF covers the P1 codex found in
// round 2 of TASK-1932: handleRegister has an admin-session branch (an
// already-logged-in admin can create a verified account with no invitation
// code), but /api/v1/auth/register was unconditionally CSRF-exempt by
// path — a cross-site POST could ride the admin's cookie into that branch
// with no CSRF token, same class of hole as the oauth-unlink case B8
// already closed. An admin session hitting /register without a CSRF token
// must now be blocked.
func TestCSRF_AdminSessionRegister_RequiresCSRF(t *testing.T) {
	srv := testServer(t)

	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(
		`{"email":"newadmin@test.com","name":"New Admin","password":"correct-horse-battery-staple"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: adminToken})
	// Deliberately no CSRF cookie/header.
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "csrf") {
		t.Fatalf("admin-session register without CSRF token must be blocked, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCSRF_AdminSessionRegister_ValidCSRFStillSucceeds is the companion to
// the test above: an admin session WITH a valid double-submit pair must
// still be able to create accounts — the P1 fix only closes the missing-
// token gap, it doesn't break the legitimate admin-created-account flow.
func TestCSRF_AdminSessionRegister_ValidCSRFStillSucceeds(t *testing.T) {
	srv := testServer(t)

	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	csrfVal := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(
		`{"email":"newadmin2@test.com","name":"New Admin","password":"correct-horse-battery-staple"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrfVal)
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: adminToken})
	req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: csrfVal})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("admin-session register with valid CSRF token: expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCSRF_AnonymousRegister_StillExempt confirms the P1 fix didn't
// collateral-damage the genuinely anonymous case: a request carrying no
// session at all must still reach handleRegister rather than being
// rejected by CSRF, even once users already exist (so the separate
// "fresh install" bypass doesn't mask the result).
func TestCSRF_AnonymousRegister_StillExempt(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(
		`{"email":"anon@test.com","name":"Anon","password":"correct-horse-battery-staple"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	// No cookies at all — genuinely anonymous.
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	// Self-hosted (non-cloud), non-admin, no invitation: the handler
	// correctly rejects this with "forbidden" business logic — the point
	// is that it's a business-logic rejection, not a CSRF one.
	if strings.Contains(w.Body.String(), "csrf") {
		t.Fatalf("anonymous register must not be blocked by CSRF, got %d: %s", w.Code, w.Body.String())
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("anonymous register on a non-cloud instance: expected 403 forbidden (business logic), got %d: %s", w.Code, w.Body.String())
	}
}

// TestCSRF_SessionCookiePlusGarbageBearer_RequiresCSRF covers the codex
// round-3 finding: the old Bearer exemption fired on header PRESENCE, not
// validation. TokenAuth's rejectInvalidBearer deliberately falls through
// (rather than 401ing) on /api/v1/auth/* paths so a stale CLI token can
// still recover — but that fallthrough let a cross-site request carrying a
// victim's real session cookie plus an attacker-supplied garbage Bearer
// header sail past CSRF on any newly-CSRF-required auth endpoint. A
// session cookie plus an invalid Bearer header, with no CSRF token, must
// now be blocked.
func TestCSRF_SessionCookiePlusGarbageBearer_RequiresCSRF(t *testing.T) {
	srv := testServer(t)

	sessionToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/auth/me", strings.NewReader(`{"name":"New Name"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer this-is-not-a-real-token")
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
	// Deliberately no CSRF cookie/header.
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "csrf") {
		t.Fatalf("session cookie + garbage Bearer header without CSRF token must be blocked as csrf_error, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCSRF_GarbageBearerNoCookie_StaysCLIRecoveryPath is the companion to
// the test above: a PURE Bearer client (no session cookie at all) with an
// invalid/expired token must keep today's behavior exactly — exempt from
// CSRF, falling through to the normal auth check, which rejects it with
// 401 (not a confusing 403 csrf_error). This is the CLI-recovery
// contract rejectInvalidBearer's fallthrough exists for: a stale
// ~/.pad/credentials.json must still reach /api/v1/auth/me (and similar)
// to get a clear "not logged in" rather than being misdiagnosed as a CSRF
// failure.
func TestCSRF_GarbageBearerNoCookie_StaysCLIRecoveryPath(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/auth/me", strings.NewReader(`{"name":"New Name"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer this-is-not-a-real-token")
	req.RemoteAddr = "192.0.2.1:1234"
	// No cookies at all.
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), "csrf") {
		t.Fatalf("a pure garbage-Bearer client with no cookie must not be blocked by CSRF, got %d: %s", w.Code, w.Body.String())
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (not logged in) for a garbage Bearer token with no cookie, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCSRF_ValidBearerPlusSessionCookie_ExemptRegardless proves the third
// required semantic from codex round 3: a request carrying a Bearer
// credential TokenAuth actually validated (here, a CLI session-bearer
// token) must stay CSRF-exempt even when a session cookie is ALSO
// present — the fix must not newly break legitimate token clients that
// happen to carry cookies. TokenAuth resolves the Bearer first and
// short-circuits SessionAuth once currentUser is set, so the cookie's
// value is irrelevant here; a different value would behave identically.
func TestCSRF_ValidBearerPlusSessionCookie_ExemptRegardless(t *testing.T) {
	srv := testServer(t)

	sessionToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/auth/me", strings.NewReader(`{"name":"New Name"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
	// Deliberately no CSRF cookie/header — the point is that a validated
	// Bearer credential doesn't need one even with a cookie present.
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("valid Bearer + session cookie, no CSRF token: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCSRF_LoginAndBootstrap_ExemptEvenWithAmbientSessionCookie pins the
// exact regression codex round 4's CI (E2E) run caught: round 2's fix
// gated the WHOLE authCSRFExemptPaths list (not just register) on
// currentUser(r) == nil, so a /login POST carrying a lingering session
// cookie — e.g. the E2E harness bootstrapping the admin (which mints a
// session cookie) and then immediately POSTing /login to re-authenticate
// — got a spurious 403 csrf_error instead of reaching the handler. Only
// register has a session-privileged branch that justifies the
// currentUser(r) == nil gate; login/bootstrap have none, so an ambient
// cookie must never cost them their exemption. This is unit-level
// coverage for the exact case the E2E suite hit, since static review
// alone missed the blast radius of round 2's fix.
func TestCSRF_LoginAndBootstrap_ExemptEvenWithAmbientSessionCookie(t *testing.T) {
	srv := testServer(t)

	sessionToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	t.Run("login with ambient session cookie, no CSRF header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(
			`{"email":"admin@test.com","password":"correct-horse-battery-staple"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.1:1234"
		req.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
		// Deliberately no CSRF cookie/header — this is exactly what the
		// E2E harness's global-setup.ts sends after bootstrapping.
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)

		if strings.Contains(w.Body.String(), "csrf") {
			t.Fatalf("login with an ambient session cookie must not be blocked by CSRF, got %d: %s", w.Code, w.Body.String())
		}
		if w.Code != http.StatusOK {
			t.Fatalf("login with correct credentials + ambient cookie: expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("bootstrap with ambient session cookie, no CSRF header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/bootstrap", strings.NewReader(
			`{"email":"second@test.com","name":"Second","password":"correct-horse-battery-staple"}`))
		req.Header.Set("Content-Type", "application/json")
		// handleBootstrap is also loopback-gated (separate from CSRF);
		// use a loopback RemoteAddr so the request reaches the CSRF-
		// irrelevant business logic this test actually cares about,
		// same as bootstrapFirstUser's own doLoopbackRequest helper.
		req.RemoteAddr = "127.0.0.1:1234"
		req.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
		// Deliberately no CSRF cookie/header.
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)

		if strings.Contains(w.Body.String(), "csrf") {
			t.Fatalf("bootstrap with an ambient session cookie must not be blocked by CSRF, got %d: %s", w.Code, w.Body.String())
		}
		// A user already exists (bootstrapFirstUser above), so the
		// handler correctly rejects this with a business-logic 409 — the
		// point is that it's a business-logic rejection, not a CSRF one.
		if w.Code != http.StatusConflict {
			t.Fatalf("bootstrap on an already-initialized instance: expected 409 conflict (business logic), got %d: %s", w.Code, w.Body.String())
		}
	})
}
