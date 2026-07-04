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
// match design of authCSRFExemptPaths: a trailing-slash variant of an
// otherwise-exempt path (e.g. "/api/v1/auth/register/") must NOT inherit
// the bare path's exemption via prefix-matching or path normalization.
func TestCSRF_TrailingSlashVariant_OfExemptPath_NotExempted(t *testing.T) {
	srv := testServer(t)

	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	sessionToken := loginUser(t, srv, "admin@test.com", "correct-horse-battery-staple")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register/", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
	// Deliberately no CSRF cookie/header.
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "csrf") {
		t.Errorf("trailing-slash variant of an exempt path must still require CSRF, got %d: %s", w.Code, w.Body.String())
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
