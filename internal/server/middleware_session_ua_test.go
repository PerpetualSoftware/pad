package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// uaBoundSession creates a user + a session bound to a fixed client IP and
// User-Agent, returning the raw session token. Tests drive requests through
// doGetWithCookieUA below, varying only the User-Agent so the IP-binding
// check stays quiet and the UA-binding check is what's under test.
func uaBoundSession(t *testing.T, srv *Server, email, ua string) string {
	t.Helper()
	user, err := srv.store.CreateUser(models.UserCreate{
		Email:    email,
		Name:     "UA Tester",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	tok, err := srv.store.CreateSession(user.ID, "test", "192.0.2.1", ua, webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return tok
}

// doGetWithCookieUA issues a GET with the session cookie, a fixed client IP
// matching the session binding, and a caller-chosen User-Agent. GET needs no
// CSRF token, keeping the UA-binding assertion isolated.
func doGetWithCookieUA(srv *Server, path, token, ua string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("User-Agent", ua)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: token})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// countUAChangeEvents returns the number of session_ua_changed rows in the
// audit log.
func countUAChangeEvents(t *testing.T, srv *Server) int {
	t.Helper()
	acts, err := srv.store.ListAuditLog(models.AuditLogParams{
		Action: models.ActionSessionUAChanged,
		Limit:  100,
	})
	if err != nil {
		t.Fatalf("list audit log: %v", err)
	}
	n := 0
	for _, a := range acts {
		if a.Action == models.ActionSessionUAChanged {
			n++
		}
	}
	return n
}

// TestSessionUAChange_LogOnlyDefault verifies that without
// PAD_IP_CHANGE_ENFORCE=strict a session that presents a different
// User-Agent is allowed through, the session survives, and — critically —
// NO audit row is written (log-only mode must preserve the historical
// slog-only behavior exactly, so existing self-host audit feeds are
// untouched).
func TestSessionUAChange_LogOnlyDefault(t *testing.T) {
	srv := testServer(t)
	const boundUA = "pad-cli/1.0 (session-bound)"
	token := uaBoundSession(t, srv, "ua-logonly@example.com", boundUA)

	// Matching UA — happy path.
	rr := doGetWithCookieUA(srv, "/api/v1/auth/me", token, boundUA)
	if rr.Code != http.StatusOK {
		t.Fatalf("matching-UA request: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Mismatched UA — still allowed in log-only mode.
	rr = doGetWithCookieUA(srv, "/api/v1/auth/me", token, "totally-different-agent/9.9")
	if rr.Code != http.StatusOK {
		t.Fatalf("UA-changed request in log-only mode: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// No audit row (behavior preserved exactly).
	if got := countUAChangeEvents(t, srv); got != 0 {
		t.Fatalf("expected 0 session_ua_changed audit rows in log-only mode, got %d", got)
	}

	// Session survives — the original UA still authenticates afterward.
	rr = doGetWithCookieUA(srv, "/api/v1/auth/me", token, boundUA)
	if rr.Code != http.StatusOK {
		t.Fatalf("post-mismatch matching-UA request: expected 200 (session not revoked), got %d", rr.Code)
	}
}

// TestSessionUAChange_StrictRevokes verifies that with strict enforcement a
// session presenting a different User-Agent is rejected (401), the session
// is destroyed, and an ActionSessionUAChanged audit row is written.
func TestSessionUAChange_StrictRevokes(t *testing.T) {
	srv := testServer(t)
	srv.SetIPChangeEnforce("strict")
	const boundUA = "pad-cli/1.0 (session-bound)"
	token := uaBoundSession(t, srv, "ua-strict@example.com", boundUA)

	// Sanity: matching UA still works in strict mode.
	rr := doGetWithCookieUA(srv, "/api/v1/auth/me", token, boundUA)
	if rr.Code != http.StatusOK {
		t.Fatalf("matching-UA request in strict mode: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Mismatched UA — rejected with 401 session_ua_changed.
	rr = doGetWithCookieUA(srv, "/api/v1/auth/me", token, "stolen-token-replayer/1.0")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("UA-changed request in strict mode: expected 401, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := countUAChangeEvents(t, srv); got != 1 {
		t.Fatalf("expected 1 session_ua_changed audit row in strict mode, got %d", got)
	}

	// Session was destroyed — even the ORIGINAL (matching) UA no longer
	// authenticates. This is the whole point: the stolen token is dead.
	rr = doGetWithCookieUA(srv, "/api/v1/auth/me", token, boundUA)
	if rr.Code == http.StatusOK {
		t.Fatal("session should have been destroyed after strict UA rejection")
	}
}

// TestSessionUAChange_StrictBearerRevokes verifies the same enforcement on
// the CLI session-bearer path (Authorization: Bearer padsess_...), which
// runs through TokenAuth rather than SessionAuth.
func TestSessionUAChange_StrictBearerRevokes(t *testing.T) {
	srv := testServer(t)
	srv.SetIPChangeEnforce("strict")
	const boundUA = "pad-cli/1.0 (session-bound)"
	token := uaBoundSession(t, srv, "ua-bearer@example.com", boundUA)

	doBearer := func(ua string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
		req.RemoteAddr = "192.0.2.1:1234"
		req.Header.Set("User-Agent", ua)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}

	// Matching UA works.
	if rr := doBearer(boundUA); rr.Code != http.StatusOK {
		t.Fatalf("matching-UA bearer request: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Mismatched UA — rejected + revoked.
	rr := doBearer("stolen-bearer/1.0")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("UA-changed bearer request in strict mode: expected 401, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := countUAChangeEvents(t, srv); got != 1 {
		t.Fatalf("expected 1 session_ua_changed audit row, got %d", got)
	}

	// Token is dead now, even with the original UA.
	if rr := doBearer(boundUA); rr.Code == http.StatusOK {
		t.Fatal("bearer session should be destroyed after strict UA rejection")
	}
}
