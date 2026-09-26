package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// BUG-3011: a web sign-in replaces the browser's session cookie, and the row
// that cookie named must go with it. Deletion is by the presented token only,
// so every other row (the same user's other devices, CLI bearers) survives.

const replacedSessionPassword = "pw-test-12345"

type signInCookie struct{ name, value string }

// signInWith posts a password login carrying the given cookies and optional
// bearer, and returns the recorder. A non-loopback RemoteAddr keeps it on
// the same path a browser takes.
func signInWith(t *testing.T, srv *Server, email string, cookies []signInCookie, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	return postWithCookies(t, srv, "/api/v1/auth/login", map[string]string{
		"email": email, "password": replacedSessionPassword,
	}, cookies, bearer)
}

func postWithCookies(t *testing.T, srv *Server, path string, body interface{}, cookies []signInCookie, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	for _, c := range cookies {
		req.AddCookie(&http.Cookie{Name: c.name, Value: c.value})
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// newSessionCookie returns the session token the response set, failing the
// test if it set none.
func newSessionCookie(t *testing.T, srv *Server, rr *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookieName(srv.secureCookies) && c.Value != "" {
			return c.Value
		}
	}
	t.Fatalf("response set no %s cookie", sessionCookieName(srv.secureCookies))
	return ""
}

func sessionAlive(t *testing.T, srv *Server, token string) bool {
	t.Helper()
	info, err := srv.store.ValidateSession(token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	return info != nil
}

func TestSignIn_AsDifferentUser_DestroysReplacedSession(t *testing.T) {
	srv := testServer(t)
	_, aTok := loginTestUserAs(t, srv, "a@test.com", "A", replacedSessionPassword)
	_, _ = loginTestUserAs(t, srv, "b@test.com", "B", replacedSessionPassword)

	rr := signInWith(t, srv, "b@test.com", []signInCookie{{sessionCookieName(srv.secureCookies), aTok}}, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("sign-in as B: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	bTok := newSessionCookie(t, srv, rr)

	if sessionAlive(t, srv, aTok) {
		t.Error("A's replaced session row is still valid after the browser signed in as B")
	}
	if rec := doRequestWithCookie(srv, http.MethodGet, "/api/v1/auth/me", nil, aTok); rec.Code != http.StatusUnauthorized {
		t.Errorf("A's old cookie: expected 401 from /auth/me, got %d", rec.Code)
	}
	if !sessionAlive(t, srv, bTok) {
		t.Error("the new session B just signed in with is not valid")
	}
}

func TestSignIn_SameUser_DestroysReplacedSessionOnly(t *testing.T) {
	srv := testServer(t)
	user, oldTok := loginTestUserAs(t, srv, "a@test.com", "A", replacedSessionPassword)
	// The same user's session on another device: a separate mint, never
	// presented by this browser.
	otherDevice, err := srv.store.CreateSession(user.ID, "web", "198.51.100.7", "other-device/1.0", webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	rr := signInWith(t, srv, "a@test.com", []signInCookie{{sessionCookieName(srv.secureCookies), oldTok}}, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("re-sign-in: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	newTok := newSessionCookie(t, srv, rr)

	if sessionAlive(t, srv, oldTok) {
		t.Error("the replaced row survived a same-user re-sign-in")
	}
	if !sessionAlive(t, srv, otherDevice) {
		t.Error("the same user's other-device session was destroyed; deletion must be by presented token, never by user")
	}
	if !sessionAlive(t, srv, newTok) {
		t.Error("the new session is not valid")
	}
}

func TestSignIn_NoOrUnknownCookie_Succeeds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cookies []signInCookie
	}{
		{"no cookie", nil},
		{"unknown token", []signInCookie{{"pad_session", "padsess_" + "00000000000000000000000000000000"}}},
		{"empty value", []signInCookie{{"pad_session", ""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer(t)
			_, _ = loginTestUserAs(t, srv, "a@test.com", "A", replacedSessionPassword)
			rr := signInWith(t, srv, "a@test.com", tc.cookies, "")
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
			}
			if !sessionAlive(t, srv, newSessionCookie(t, srv, rr)) {
				t.Error("the new session is not valid")
			}
		})
	}
}

// A CLI padsess_ bearer is not a cookie this response replaces: the CLI
// ignores Set-Cookie and keeps using its own row.
func TestSignIn_BearerSessionIsUntouched(t *testing.T) {
	srv := testServer(t)
	_, cliTok := loginTestUserAs(t, srv, "a@test.com", "A", replacedSessionPassword)

	rr := signInWith(t, srv, "a@test.com", nil, cliTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !sessionAlive(t, srv, cliTok) {
		t.Error("a bearer session presented on the sign-in request was destroyed")
	}
}

// In secure mode the unprefixed legacy cookie is shadowed by the new
// __Host- cookie (validateSessionCookie prefers the prefixed name), so its
// row is orphaned the same way and goes too.
func TestSignIn_SecureMode_DestroysPrefixedAndLegacyCookieRows(t *testing.T) {
	srv := testServer(t)
	srv.secureCookies = true
	_, prefixedTok := loginTestUserAs(t, srv, "a@test.com", "A", replacedSessionPassword)
	_, legacyTok := loginTestUserAs(t, srv, "b@test.com", "B", replacedSessionPassword)
	_, _ = loginTestUserAs(t, srv, "c@test.com", "C", replacedSessionPassword)

	rr := signInWith(t, srv, "c@test.com", []signInCookie{
		{sessionCookieName(true), prefixedTok},
		{sessionCookieName(false), legacyTok},
	}, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if sessionAlive(t, srv, prefixedTok) {
		t.Error("the __Host- cookie's row survived")
	}
	if sessionAlive(t, srv, legacyTok) {
		t.Error("the shadowed legacy cookie's row survived")
	}
}

// handleResetPassword mints its session without createAuthSession, and its
// DeleteUserSessions covers only the reset user, so it is a separate site.
func TestResetPassword_InBrowserOfAnotherUser_DestroysReplacedSession(t *testing.T) {
	srv := testServer(t)
	_, aTok := loginTestUserAs(t, srv, "a@test.com", "A", replacedSessionPassword)
	b, _ := loginTestUserAs(t, srv, "b@test.com", "B", replacedSessionPassword)
	resetTok, err := srv.store.CreatePasswordReset(b.ID)
	if err != nil {
		t.Fatalf("CreatePasswordReset: %v", err)
	}

	rr := postWithCookies(t, srv, "/api/v1/auth/reset-password", map[string]string{
		"token": resetTok, "password": "a-Much-Stronger-Passphrase-2026!",
	}, []signInCookie{{sessionCookieName(srv.secureCookies), aTok}}, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("reset-password: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if sessionAlive(t, srv, aTok) {
		t.Error("A's replaced session row survived B's password reset in A's browser")
	}
}
