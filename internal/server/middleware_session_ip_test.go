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
