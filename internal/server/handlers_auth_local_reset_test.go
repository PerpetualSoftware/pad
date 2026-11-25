package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The localhost recovery endpoint (POST /api/v1/auth/local-reset) is the
// self-host escape hatch for a locked-out operator. Its entire security
// model is three gates: not-cloud-mode, strict-loopback, and CLI-only
// (JSON body with no browser Origin or Sec-Fetch-Site headers). These tests
// pin all three gates plus the two output modes (reset link / temp password).

func doLoopbackRawRequest(srv *Server, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func assertLocalResetError(t *testing.T, rr *httptest.ResponseRecorder, wantStatus int, wantCode, wantMsg string) {
	t.Helper()
	if rr.Code != wantStatus {
		t.Fatalf("expected status %d, got %d: %s", wantStatus, rr.Code, rr.Body.String())
	}
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	parseJSON(t, rr, &resp)
	if resp.Error.Code != wantCode {
		t.Fatalf("expected error code %q, got %q (body: %s)", wantCode, resp.Error.Code, rr.Body.String())
	}
	if resp.Error.Message != wantMsg {
		t.Fatalf("expected error message %q, got %q (body: %s)", wantMsg, resp.Error.Message, rr.Body.String())
	}
}

func assertPasswordUnchangedAndSessionValid(t *testing.T, srv *Server, email, sessionToken string) {
	t.Helper()
	rr := doLoopbackRequest(srv, "POST", "/api/v1/auth/login", map[string]interface{}{
		"email":    email,
		"password": "correct-horse-battery-staple",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected original password to still log in, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(srv, "GET", "/api/v1/auth/session", nil, sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("session check: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var session map[string]interface{}
	parseJSON(t, rr, &session)
	if session["authenticated"] != true {
		t.Fatalf("expected existing session token to remain authenticated, got %v", session)
	}
}

func TestLocalReset_RejectsRemoteRequest(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	// Non-loopback peer: must be refused even though the body is valid.
	rr := doRequest(srv, "POST", "/api/v1/auth/local-reset", map[string]interface{}{
		"email": "admin@test.com",
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("remote local-reset: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestLocalReset_DisabledInCloudMode(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	srv.cloudMode = true
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	// Even from loopback, cloud mode must refuse: the host process must
	// never be able to reset an arbitrary tenant's password.
	rr := doLoopbackRequest(srv, "POST", "/api/v1/auth/local-reset", map[string]interface{}{
		"email": "admin@test.com",
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cloud-mode local-reset: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestLocalReset_UnknownEmail(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr := doLoopbackRequest(srv, "POST", "/api/v1/auth/local-reset", map[string]interface{}{
		"email": "nobody@test.com",
	})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown-email local-reset: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestLocalReset_ResetLinkCompletesLogin(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	// A configured public base URL must come back as an absolute, shareable
	// reset_url — not just the path.
	srv.baseURL = "https://pad.example.com"
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	// Loopback, default mode → single-use reset link.
	rr := doLoopbackRequest(srv, "POST", "/api/v1/auth/local-reset", map[string]interface{}{
		"email": "admin@test.com",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("local-reset link: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Method    string `json:"method"`
		ResetPath string `json:"reset_path"`
		ResetURL  string `json:"reset_url"`
		Email     string `json:"email"`
	}
	parseJSON(t, rr, &resp)
	if resp.Method != "reset_url" {
		t.Fatalf("expected method=reset_url, got %q", resp.Method)
	}
	token := strings.TrimPrefix(resp.ResetPath, "/reset-password/")
	if token == "" || token == resp.ResetPath {
		t.Fatalf("expected reset_path of form /reset-password/<token>, got %q", resp.ResetPath)
	}
	if want := "https://pad.example.com" + resp.ResetPath; resp.ResetURL != want {
		t.Fatalf("expected reset_url %q, got %q", want, resp.ResetURL)
	}

	// The returned token must drive the existing reset flow end to end.
	const newPassword = "tr0ubadour-fresh-x9-pass"
	rr = doLoopbackRequest(srv, "POST", "/api/v1/auth/reset-password", map[string]interface{}{
		"token":    token,
		"password": newPassword,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("reset-password with local-reset token: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// And the operator can now log in with the new password.
	rr = doLoopbackRequest(srv, "POST", "/api/v1/auth/login", map[string]interface{}{
		"email":    "admin@test.com",
		"password": newPassword,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("login after reset: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestLocalReset_TempPasswordLogsIn(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr := doLoopbackRequest(srv, "POST", "/api/v1/auth/local-reset", map[string]interface{}{
		"email":         "admin@test.com",
		"temp_password": true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("local-reset temp: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Method       string `json:"method"`
		TempPassword string `json:"temp_password"`
	}
	parseJSON(t, rr, &resp)
	if resp.Method != "temp_password" || resp.TempPassword == "" {
		t.Fatalf("expected a temp_password payload, got %+v", resp)
	}

	rr = doLoopbackRequest(srv, "POST", "/api/v1/auth/login", map[string]interface{}{
		"email":    "admin@test.com",
		"password": resp.TempPassword,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("login with temp password: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestLocalReset_RefusesTextPlainForm(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr := doLoopbackRawRequest(srv, "POST", "/api/v1/auth/local-reset",
		`{"email":"admin@test.com","temp_password":true}`,
		map[string]string{
			"Content-Type": "text/plain",
		},
	)
	assertLocalResetError(t, rr,
		http.StatusUnsupportedMediaType,
		"unsupported_media_type",
		"Local password reset requires a JSON request body",
	)
	assertPasswordUnchangedAndSessionValid(t, srv, "admin@test.com", token)
}

func TestLocalReset_RefusesCrossSiteOrigin(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr := doLoopbackRawRequest(srv, "POST", "/api/v1/auth/local-reset",
		`{"email":"admin@test.com","temp_password":true}`,
		map[string]string{
			"Content-Type": "application/json",
			"Origin":       "https://evil.example",
		},
	)
	assertLocalResetError(t, rr,
		http.StatusForbidden,
		"forbidden",
		"Local password reset is only available from the pad CLI on the server host",
	)
	assertPasswordUnchangedAndSessionValid(t, srv, "admin@test.com", token)
}

func TestLocalReset_RefusesSameOriginBrowser(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	const host = "rebind.evil.example:7777"
	rr := doLoopbackRawRequest(srv, "POST", "http://"+host+"/api/v1/auth/local-reset",
		`{"email":"admin@test.com","temp_password":true}`,
		map[string]string{
			"Content-Type": "application/json",
			"Origin":       "http://" + host,
		},
	)
	assertLocalResetError(t, rr,
		http.StatusForbidden,
		"forbidden",
		"Local password reset is only available from the pad CLI on the server host",
	)
	assertPasswordUnchangedAndSessionValid(t, srv, "admin@test.com", token)
}

func TestLocalReset_RefusesSecFetchSite(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr := doLoopbackRawRequest(srv, "POST", "/api/v1/auth/local-reset",
		`{"email":"admin@test.com","temp_password":true}`,
		map[string]string{
			"Content-Type":   "application/json",
			"Sec-Fetch-Site": "same-origin",
		},
	)
	assertLocalResetError(t, rr,
		http.StatusForbidden,
		"forbidden",
		"Local password reset is only available from the pad CLI on the server host",
	)
	assertPasswordUnchangedAndSessionValid(t, srv, "admin@test.com", token)
}
