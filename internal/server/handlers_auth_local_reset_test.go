package server

import (
	"net/http"
	"strings"
	"testing"
)

// The localhost recovery endpoint (POST /api/v1/auth/local-reset) is the
// self-host escape hatch for a locked-out operator. Its entire security
// model is two gates: not-cloud-mode and strict-loopback. These tests pin
// both gates plus the two output modes (reset link / temp password).

func TestLocalReset_RejectsRemoteRequest(t *testing.T) {
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
