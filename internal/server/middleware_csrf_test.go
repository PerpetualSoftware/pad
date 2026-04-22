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

	// Auth endpoints should work without CSRF token
	endpoints := []string{
		"/api/v1/auth/login",
		"/api/v1/auth/register",
		"/api/v1/auth/logout",
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

	// Logout (auth endpoints are CSRF-exempt)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
	req.RemoteAddr = "192.0.2.1:1234"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	// Check CSRF cookie is cleared
	for _, c := range w.Result().Cookies() {
		if c.Name == "pad_csrf" {
			if c.MaxAge >= 0 {
				t.Errorf("expected CSRF cookie to be cleared (MaxAge < 0), got MaxAge=%d", c.MaxAge)
			}
		}
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
