package server

// BUG-1227 — TokenAuth must not 401 public API paths on a stale/invalid
// Bearer token. Until this fix, any developer with a stale credential in
// `~/.pad/credentials.json` (typically after wiping a test DB) saw
// "Invalid or expired session" on every CLI invocation — including the
// very endpoints needed to recover (/auth/session, /auth/login,
// /auth/forgot-password). The matching IP-change-revoked branch in the
// same file already had the right behavior; these tests pin it for the
// invalid/malformed Bearer paths as well.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// doRequestWithBearer dispatches a request with an Authorization: Bearer
// header set. The existing test helpers in server_test.go don't take a
// token parameter, and re-dispatching there would double-execute the
// request, so we mint a fresh request here.
func doRequestWithBearer(srv *Server, method, path, bearer string, body interface{}) *httptest.ResponseRecorder {
	var bodyReader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestTokenAuth_PublicPath_InvalidSessionFallsThrough — the canonical
// repro for BUG-1227. A `padsess_*` Bearer that doesn't validate must
// NOT cause /api/v1/auth/session to 401; the request must reach
// handleSessionCheck and return the public payload (setup_required: true
// on a fresh server).
func TestTokenAuth_PublicPath_InvalidSessionFallsThrough(t *testing.T) {
	srv := testServer(t)

	rec := doRequestWithBearer(srv, "GET", "/api/v1/auth/session",
		"padsess_0000000000000000000000000000000000000000000000000000000000000000", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("/auth/session with invalid session Bearer: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Sanity: handler ran and returned the public setup-state payload.
	var resp map[string]interface{}
	parseJSON(t, rec, &resp)
	if resp["setup_required"] != true {
		t.Errorf("expected setup_required=true on fresh server, got %v", resp["setup_required"])
	}
}

// TestTokenAuth_PublicPath_MalformedAuthHeaderFallsThrough — an
// Authorization header that doesn't even start with "Bearer " (caller
// somehow sent garbage) must also fall through on public paths. Pre-fix
// this 401'd at the format-check before token parsing.
func TestTokenAuth_PublicPath_MalformedAuthHeaderFallsThrough(t *testing.T) {
	srv := testServer(t)

	req := httptest.NewRequest("GET", "/api/v1/auth/session", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "NotBearer something-else")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/auth/session with malformed Authorization: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestTokenAuth_PublicPath_InvalidAPITokenFormatFallsThrough — a Bearer
// that's neither padsess_* nor a valid pad_* shape (wrong length, wrong
// prefix) must also fall through on public paths. Symmetry with the
// session-token branch.
func TestTokenAuth_PublicPath_InvalidAPITokenFormatFallsThrough(t *testing.T) {
	srv := testServer(t)

	rec := doRequestWithBearer(srv, "GET", "/api/v1/auth/session", "totally-not-a-pad-token", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("/auth/session with invalid token format: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestTokenAuth_PublicPath_InvalidAPITokenFallsThrough — a `pad_*`
// Bearer of the right shape but not matching any live API token must
// fall through on public paths. (Caller saved a token, the server's
// token table got wiped, caller now wants to re-auth.)
func TestTokenAuth_PublicPath_InvalidAPITokenFallsThrough(t *testing.T) {
	srv := testServer(t)

	// pad_<64 hex chars> is the format ValidateToken expects. We use 64
	// zeros so it parses past the format check but fails the lookup.
	bogus := "pad_0000000000000000000000000000000000000000000000000000000000000000"
	rec := doRequestWithBearer(srv, "GET", "/api/v1/auth/session", bogus, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("/auth/session with invalid pad_ Bearer: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestTokenAuth_PublicPath_LoginRecoversWithStaleBearer — the realistic
// recovery flow: caller has a stale Bearer in their credentials, hits
// /auth/login with valid email+password in the body. The login handler
// must run and succeed; it must NOT be blocked by the middleware
// rejecting the stale Bearer. This is the actual user-visible bug in
// BUG-1227 — without this fallthrough the user can't even log in to
// fix their stale credentials.
func TestTokenAuth_PublicPath_LoginRecoversWithStaleBearer(t *testing.T) {
	srv := testServer(t)
	// Bootstrap a real user so we have valid creds to log in with.
	bootstrapFirstUser(t, srv, "user@example.com", "User")

	rec := doRequestWithBearer(srv, "POST", "/api/v1/auth/login",
		"padsess_dead0000000000000000000000000000000000000000000000000000000000",
		map[string]string{
			"email":    "user@example.com",
			"password": "correct-horse-battery-staple",
		})

	if rec.Code != http.StatusOK {
		t.Fatalf("/auth/login with stale Bearer + valid creds: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestTokenAuth_ProtectedPath_StillRejectsInvalidBearer — regression
// guard. The fix only relaxes public paths; protected endpoints must
// still 401 on an invalid Bearer or the security model is broken.
// Hits a workspace endpoint after an admin exists, so RequireAuth is
// engaged.
func TestTokenAuth_ProtectedPath_StillRejectsInvalidBearer(t *testing.T) {
	srv := testServer(t)
	// Bootstrap so RequireAuth gates non-public paths (it short-circuits
	// when UserCount == 0 to allow first-time setup).
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	rec := doRequestWithBearer(srv, "GET", "/api/v1/workspaces",
		"padsess_dead0000000000000000000000000000000000000000000000000000000000", nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("protected /workspaces with invalid Bearer: expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}
