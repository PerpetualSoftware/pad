package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xarmian/pad/internal/models"
)

// doRequestWithCookieFrom is like doRequestWithCookie but lets the caller
// set the request RemoteAddr so tests can simulate a session jumping to a
// different client IP mid-lifetime.
func doRequestWithCookieFrom(srv *Server, method, path string, body interface{}, token, remoteAddr string) *httptest.ResponseRecorder {
	var bodyReader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = remoteAddr
	req.AddCookie(&http.Cookie{
		Name:  "pad_session",
		Value: token,
	})
	const testCSRF = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req.AddCookie(&http.Cookie{
		Name:  "pad_csrf",
		Value: testCSRF,
	})
	req.Header.Set("X-CSRF-Token", testCSRF)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// TestSessionIPChange_LogOnlyDefault verifies that without
// PAD_IP_CHANGE_ENFORCE=strict, a session that presents a different client
// IP is allowed through but an ActionSessionIPChanged audit row is written.
func TestSessionIPChange_LogOnlyDefault(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "admin@example.com", "Admin") // bootstrap creates from 127.0.0.1

	// First request from the original IP — no change, no audit row.
	rr := doRequestWithCookieFrom(srv, "GET", "/api/v1/auth/me", nil, token, "127.0.0.1:2222")
	if rr.Code != http.StatusOK {
		t.Fatalf("same-IP request: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := countIPChangeEvents(t, srv); got != 0 {
		t.Fatalf("no audit row expected before IP change, got %d", got)
	}

	// Request from a brand-new IP — request still succeeds (log-only).
	rr = doRequestWithCookieFrom(srv, "GET", "/api/v1/auth/me", nil, token, "198.51.100.7:5555")
	if rr.Code != http.StatusOK {
		t.Fatalf("IP-changed request in log-only mode: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := countIPChangeEvents(t, srv); got != 1 {
		t.Fatalf("expected 1 ActionSessionIPChanged audit row after IP change, got %d", got)
	}

	// Subsequent request from the same new IP must NOT re-log (we updated
	// the stored IP in-place on the first hit).
	rr = doRequestWithCookieFrom(srv, "GET", "/api/v1/auth/me", nil, token, "198.51.100.7:5555")
	if rr.Code != http.StatusOK {
		t.Fatalf("same-new-IP repeat: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := countIPChangeEvents(t, srv); got != 1 {
		t.Fatalf("no additional audit row expected after IP settles, got %d", got)
	}
}

// TestSessionIPChange_StrictRejects verifies that with strict enforcement
// enabled, a session presenting a different client IP is rejected AND the
// session is destroyed (so a stolen token can't be retried from the same
// new IP).
func TestSessionIPChange_StrictRejects(t *testing.T) {
	srv := testServer(t)
	srv.SetIPChangeEnforce("strict")
	token := bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	// Sanity: original IP still works.
	rr := doRequestWithCookieFrom(srv, "GET", "/api/v1/auth/me", nil, token, "127.0.0.1:2222")
	if rr.Code != http.StatusOK {
		t.Fatalf("same-IP request: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Jump to a new IP — must be rejected with 401 in strict mode.
	rr = doRequestWithCookieFrom(srv, "GET", "/api/v1/auth/me", nil, token, "198.51.100.7:5555")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("IP-changed request in strict mode: expected 401, got %d: %s", rr.Code, rr.Body.String())
	}
	// Audit row still gets written.
	if got := countIPChangeEvents(t, srv); got != 1 {
		t.Fatalf("expected 1 ActionSessionIPChanged audit row in strict mode, got %d", got)
	}

	// Retrying with the same token — even from the new IP — must fail
	// (session was destroyed).
	rr = doRequestWithCookieFrom(srv, "GET", "/api/v1/auth/me", nil, token, "198.51.100.7:5555")
	if rr.Code == http.StatusOK {
		t.Fatal("session should have been destroyed after strict rejection")
	}
}

// TestSessionIPChange_StrictDestroysSessionAtomically verifies that in
// strict mode the session is destroyed as part of the IP-mismatch check
// and does NOT survive with a rebound IP. A regression would mean that
// if DeleteSession were ever separated from the rotation, a follow-up
// request from the new IP could pass authentication.
func TestSessionIPChange_StrictDestroysSessionAtomically(t *testing.T) {
	srv := testServer(t)
	srv.SetIPChangeEnforce("strict")
	token := bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	// First IP-mismatch request → 401.
	rr := doRequestWithCookieFrom(srv, "GET", "/api/v1/auth/me", nil, token, "198.51.100.7:5555")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on IP mismatch, got %d: %s", rr.Code, rr.Body.String())
	}

	// Second request from the NEW IP must also fail — the session must
	// be gone, not rebound to 198.51.100.7. A regression that rotated
	// the stored IP before destroying would leak through here.
	rr = doRequestWithCookieFrom(srv, "GET", "/api/v1/auth/me", nil, token, "198.51.100.7:5555")
	if rr.Code == http.StatusOK {
		t.Fatal("session must be destroyed in strict mode, not rebound to new IP")
	}

	// Single audit row for the whole transition (the second request hits
	// the ValidateSession-returns-nil branch and never re-logs).
	if got := countIPChangeEvents(t, srv); got != 1 {
		t.Fatalf("expected exactly 1 audit row for the strict-mode transition, got %d", got)
	}
}

// TestSessionIPChange_StrictClearsCookies verifies that in strict mode
// the session cookie is explicitly cleared on the response (MaxAge=-1) so
// the browser doesn't keep re-sending a now-revoked token on the next
// navigation. Only the API 401 path reaches SessionAuth in the current
// routing (the SPA catch-all is mounted on the root router outside the
// auth group), but cookie hygiene still applies for API clients that
// present cookies (e.g. the web UI's fetch calls).
func TestSessionIPChange_StrictClearsCookies(t *testing.T) {
	srv := testServer(t)
	srv.SetIPChangeEnforce("strict")
	token := bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	rr := doRequestWithCookieFrom(srv, "GET", "/api/v1/auth/me", nil, token, "198.51.100.7:5555")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rr.Code, rr.Body.String())
	}

	// Session cookie must have been cleared by Set-Cookie: MaxAge=-1.
	foundClear := false
	for _, c := range rr.Result().Cookies() {
		if strings.HasPrefix(c.Name, "pad_session") && c.MaxAge < 0 {
			foundClear = true
			break
		}
	}
	if !foundClear {
		t.Fatalf("expected session cookie to be cleared in strict mode; cookies=%v", rr.Result().Cookies())
	}
}

// TestSessionIPChange_StrictAllowsPublicAPIPaths verifies that a stale
// session cookie on a PUBLIC API path (login, password reset, health,
// share links, plan-limits) does NOT produce a 401 in strict mode. The
// session still gets destroyed and the cookie cleared so the stale token
// can't be reused, but the request falls through so the user can log in
// again or the health probe succeeds.
func TestSessionIPChange_StrictAllowsPublicAPIPaths(t *testing.T) {
	srv := testServer(t)
	srv.SetIPChangeEnforce("strict")
	token := bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	// POST /api/v1/auth/login with the stale session cookie AND a
	// different client IP. The handler expects email+password in the body
	// and returns 400 "bad_request" on invalid JSON, so we send an empty
	// body to keep the assertion simple. The important thing is: we
	// don't get 401 session_ip_changed — the stale cookie didn't block
	// the user from re-authenticating.
	rr := doRequestWithCookieFrom(srv, "POST", "/api/v1/auth/login", map[string]string{
		"email":    "admin@example.com",
		"password": "wrong-password-on-purpose",
	}, token, "198.51.100.7:5555")
	if rr.Code == http.StatusUnauthorized {
		// Specifically, must NOT be the IP-change error code.
		if strings.Contains(rr.Body.String(), "session_ip_changed") {
			t.Fatalf("stale cookie on login endpoint blocked user with session_ip_changed: %s", rr.Body.String())
		}
	}
	// Session must still be destroyed + audit row written.
	if got := countIPChangeEvents(t, srv); got != 1 {
		t.Fatalf("expected 1 ActionSessionIPChanged audit row, got %d", got)
	}

	// A subsequent authenticated API call with the same stale token must
	// now fail — session is gone.
	rr = doRequestWithCookieFrom(srv, "GET", "/api/v1/auth/me", nil, token, "198.51.100.7:5555")
	if rr.Code == http.StatusOK {
		t.Fatal("revoked session must not authenticate after strict IP-change destroy")
	}

	// Plan-limits is another public path — same treatment.
	srv2 := testServer(t)
	srv2.SetIPChangeEnforce("strict")
	token2 := bootstrapFirstUser(t, srv2, "admin2@example.com", "Admin2")
	rr = doRequestWithCookieFrom(srv2, "GET", "/api/v1/plan-limits", nil, token2, "198.51.100.7:5555")
	if rr.Code == http.StatusUnauthorized && strings.Contains(rr.Body.String(), "session_ip_changed") {
		t.Fatalf("stale cookie on plan-limits blocked with session_ip_changed: %s", rr.Body.String())
	}
}

// TestSessionIPChange_CASDedupesRace verifies that the compare-and-set
// update scheme produces at most one audit row per IP-change transition,
// even when many concurrent requests arrive from the new IP before any
// of them has observed the rotated value.
func TestSessionIPChange_CASDedupesRace(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	// Warm the stored IP with an initial request from the bootstrap source.
	rr := doRequestWithCookieFrom(srv, "GET", "/api/v1/auth/me", nil, token, "127.0.0.1:2222")
	if rr.Code != http.StatusOK {
		t.Fatalf("warmup: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Fire N concurrent requests from the new IP. Only the request whose
	// CAS actually rotated the stored IP should have logged an audit row.
	const n = 20
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			doRequestWithCookieFrom(srv, "GET", "/api/v1/auth/me", nil, token, "198.51.100.7:5555")
			done <- struct{}{}
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}

	if got := countIPChangeEvents(t, srv); got != 1 {
		t.Fatalf("expected exactly 1 audit row from %d concurrent requests, got %d", n, got)
	}
}

// TestSetIPChangeEnforce_CaseInsensitive verifies the setter accepts
// common variants without surprises.
func TestSetIPChangeEnforce_CaseInsensitive(t *testing.T) {
	srv := testServer(t)
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"strict", true},
		{"STRICT", true},
		{"  Strict  ", true},
		{"log", false},
		{"yes", false},
	}
	for _, tc := range cases {
		srv.SetIPChangeEnforce(tc.in)
		if srv.ipChangeEnforceStrict != tc.want {
			t.Errorf("SetIPChangeEnforce(%q) → ipChangeEnforceStrict=%v, want %v",
				tc.in, srv.ipChangeEnforceStrict, tc.want)
		}
	}
}

// countIPChangeEvents returns the number of session_ip_changed rows in the
// audit log.
func countIPChangeEvents(t *testing.T, srv *Server) int {
	t.Helper()
	acts, err := srv.store.ListAuditLog(models.AuditLogParams{
		Action: models.ActionSessionIPChanged,
		Limit:  100,
	})
	if err != nil {
		t.Fatalf("list audit log: %v", err)
	}
	// Guard against accidental matches on unrelated actions.
	n := 0
	for _, a := range acts {
		if a.Action == models.ActionSessionIPChanged {
			n++
		}
	}
	return n
}

// keep strings reference in case future cases need it; harmless no-op.
var _ = strings.EqualFold
