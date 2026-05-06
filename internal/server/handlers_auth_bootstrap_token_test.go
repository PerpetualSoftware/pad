package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// Tests for the first-run logs-token bootstrap flow (TASK-1167 / PLAN-1166).
// These tests pin the security-critical contracts that codex review
// surfaced before implementation:
//
//   - F2: cloud mode never accepts the token bypass.
//   - F5: concurrent valid-token requests cannot create multiple admins.
//   - F6: token is accepted via X-Bootstrap-Token header only — never via
//     ?token= query.
//   - F9: cloud mode never advertises setup_method=logs_token.
//
// Plus regression-safe coverage of the existing loopback gate and the
// session-check setup_method response.

const testBootstrapToken = "test-bootstrap-token-fixed-value"

// configureBootstrapToken seeds a known token on the server so tests don't
// depend on the random generator. The on-disk file is created in a temp
// dir so the consume-removes-file assertion has a real file to delete.
func configureBootstrapToken(t *testing.T, srv *Server) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".bootstrap-token")
	if err := os.WriteFile(path, []byte(testBootstrapToken+"\n"), 0600); err != nil {
		t.Fatalf("seed token file: %v", err)
	}
	srv.SetBootstrapToken(testBootstrapToken, path)
	return path
}

func bootstrapBody(email string) map[string]string {
	return map[string]string{
		"email":    email,
		"name":     "Admin",
		"password": "correct-horse-battery-staple",
	}
}

// doRequestWithHeadersFromAddr lets us combine arbitrary headers with a
// non-loopback remote-addr. Existing helpers cover one or the other but
// not both.
func doRequestWithHeadersFromAddr(srv *Server, method, path string, body interface{}, headers map[string]string, remoteAddr string) *httptest.ResponseRecorder {
	var bodyReader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.RemoteAddr = remoteAddr
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// TestBootstrapToken_NonLoopback_HeaderAccepted is the happy path: a
// non-loopback peer presents the correct X-Bootstrap-Token header and
// the bootstrap succeeds, the file is deleted, and the in-memory token
// is cleared.
func TestBootstrapToken_NonLoopback_HeaderAccepted(t *testing.T) {
	srv := testServer(t)
	tokenPath := configureBootstrapToken(t, srv)

	rr := doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/bootstrap",
		bootstrapBody("admin@test.com"),
		map[string]string{BootstrapTokenHeader: testBootstrapToken},
		"192.0.2.1:1234")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 with valid header, got %d: %s", rr.Code, rr.Body.String())
	}

	// File deleted.
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("token file still exists after bootstrap: stat err = %v", err)
	}
	// In-memory cleared.
	if srv.hasBootstrapToken() {
		t.Fatal("hasBootstrapToken() = true after consume, want false")
	}
}

// TestBootstrapToken_NonLoopback_NoHeader pins the loopback-only default
// for self-host without a token configured (regression coverage —
// existing behavior must not change).
func TestBootstrapToken_NonLoopback_NoHeader(t *testing.T) {
	srv := testServer(t)
	configureBootstrapToken(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/auth/bootstrap", bootstrapBody("admin@test.com"))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without header, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestBootstrapToken_NonLoopback_WrongHeader covers wrong-token rejection.
func TestBootstrapToken_NonLoopback_WrongHeader(t *testing.T) {
	srv := testServer(t)
	configureBootstrapToken(t, srv)

	rr := doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/bootstrap",
		bootstrapBody("admin@test.com"),
		map[string]string{BootstrapTokenHeader: "wrong-token"},
		"192.0.2.1:1234")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 with wrong token, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestBootstrapToken_NonLoopback_QueryRejected pins the F6 contract: token
// must NOT be accepted via ?token= query, only via X-Bootstrap-Token
// header. This prevents the secret from landing in any caller's access
// log, browser history, or proxy log.
func TestBootstrapToken_NonLoopback_QueryRejected(t *testing.T) {
	srv := testServer(t)
	configureBootstrapToken(t, srv)

	// No header — only query param. Must be rejected.
	rr := doRequest(srv, "POST",
		"/api/v1/auth/bootstrap?token="+testBootstrapToken,
		bootstrapBody("admin@test.com"))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 with token in query (header-only contract), got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestBootstrapToken_Replay_AfterConsume verifies that once the first
// admin is created, the same token cannot be reused — even by the same
// client.
func TestBootstrapToken_Replay_AfterConsume(t *testing.T) {
	srv := testServer(t)
	configureBootstrapToken(t, srv)

	// Round 1 — succeeds.
	rr := doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/bootstrap",
		bootstrapBody("admin@test.com"),
		map[string]string{BootstrapTokenHeader: testBootstrapToken},
		"192.0.2.1:1234")
	if rr.Code != http.StatusCreated {
		t.Fatalf("first bootstrap: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Round 2 — same token replayed. After consume it's been cleared in
	// memory + the file is deleted, so the validate path returns false
	// and we fall back to the loopback gate (which fails: non-loopback
	// peer). UserCount > 0 also bails the inner check, but we expect
	// the outer gate to win first with 403.
	rr = doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/bootstrap",
		bootstrapBody("second@test.com"),
		map[string]string{BootstrapTokenHeader: testBootstrapToken},
		"192.0.2.1:1234")
	if rr.Code == http.StatusCreated {
		t.Fatalf("replay succeeded with consumed token: %s", rr.Body.String())
	}
}

// TestBootstrapToken_CloudMode_NoBypass pins the F2 contract: a fresh
// cloud instance with UserCount=0 + a token configured + a non-loopback
// peer presenting the correct header → still 403. Cloud bootstrap
// remains loopback-only regardless of token state.
//
// In production, cmd/pad/main.go never calls EnsureBootstrapToken when
// the instance is in cloud mode (D10), so the token would never even be
// loaded. This test forces the token in anyway to pin the
// defense-in-depth check inside handleBootstrap itself.
func TestBootstrapToken_CloudMode_NoBypass(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode("test-cloud-secret")
	configureBootstrapToken(t, srv)

	rr := doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/bootstrap",
		bootstrapBody("admin@test.com"),
		map[string]string{BootstrapTokenHeader: testBootstrapToken},
		"192.0.2.1:1234")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cloud-mode token bypass leaked! expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	// Token must still be present (consume only fires on success).
	if !srv.hasBootstrapToken() {
		t.Fatal("token was consumed on a rejected cloud-mode bootstrap")
	}
}

// TestBootstrapToken_ConcurrentRace pins the F5 contract: N simultaneous
// requests with the correct token + different emails must produce
// exactly one success. The mutex in handleBootstrap serializes the
// validate → check-UserCount → CreateUser → consume sequence so the
// loser sees an empty in-memory token after the winner consumes.
func TestBootstrapToken_ConcurrentRace(t *testing.T) {
	srv := testServer(t)
	configureBootstrapToken(t, srv)

	const N = 8
	var (
		successes int32
		failures  int32
	)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			email := "admin" + string(rune('0'+idx)) + "@test.com"
			rr := doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/bootstrap",
				bootstrapBody(email),
				map[string]string{BootstrapTokenHeader: testBootstrapToken},
				"192.0.2.1:1234")
			if rr.Code == http.StatusCreated {
				atomic.AddInt32(&successes, 1)
			} else {
				atomic.AddInt32(&failures, 1)
			}
		}(i)
	}
	wg.Wait()

	if successes != 1 {
		t.Fatalf("expected exactly 1 successful bootstrap, got %d (failures: %d)", successes, failures)
	}
	if failures != N-1 {
		t.Fatalf("expected %d failures, got %d", N-1, failures)
	}
	// And exactly 1 admin in the DB.
	count, err := srv.store.UserCount()
	if err != nil {
		t.Fatalf("UserCount: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 admin after concurrent race, got %d (this is the F5 multi-admin bug)", count)
	}
}

// TestBootstrapToken_SessionCheck_LogsToken pins the setup_method value
// returned by /api/v1/auth/session when a token is configured and no
// users exist — the frontend's SetupRequiredNotice branches on this to
// render the "paste your bootstrap token from the container logs" UI.
func TestBootstrapToken_SessionCheck_LogsToken(t *testing.T) {
	srv := testServer(t)
	configureBootstrapToken(t, srv)

	rr := doRequest(srv, "GET", "/api/v1/auth/session", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("session check: expected 200, got %d", rr.Code)
	}
	var session map[string]interface{}
	parseJSON(t, rr, &session)
	if session["setup_required"] != true {
		t.Errorf("expected setup_required=true with no users, got %v", session["setup_required"])
	}
	if session["setup_method"] != "logs_token" {
		t.Errorf("expected setup_method=logs_token when token is loaded + no users, got %v", session["setup_method"])
	}
}

// TestBootstrapToken_SessionCheck_LocalCLI_NoToken pins the existing
// fallback: when no token is configured (e.g. read-only data dir per
// D7), the session payload returns the existing local_cli value.
func TestBootstrapToken_SessionCheck_LocalCLI_NoToken(t *testing.T) {
	srv := testServer(t)
	// Note: NOT calling configureBootstrapToken — leaves token empty.

	rr := doRequest(srv, "GET", "/api/v1/auth/session", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("session check: expected 200, got %d", rr.Code)
	}
	var session map[string]interface{}
	parseJSON(t, rr, &session)
	if session["setup_method"] != "local_cli" {
		t.Errorf("expected setup_method=local_cli with no token loaded, got %v", session["setup_method"])
	}
}

// TestBootstrapToken_SessionCheck_CloudMode_NoLogsToken pins the F9
// contract: cloud-mode session must NEVER advertise logs_token, even if
// a token is somehow loaded (shouldn't happen via cmd/pad/main.go but
// the handler must defend itself anyway).
func TestBootstrapToken_SessionCheck_CloudMode_NoLogsToken(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode("test-cloud-secret")
	configureBootstrapToken(t, srv)

	rr := doRequest(srv, "GET", "/api/v1/auth/session", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("session check: expected 200, got %d", rr.Code)
	}
	var session map[string]interface{}
	parseJSON(t, rr, &session)
	if session["setup_method"] == "logs_token" {
		t.Fatalf("cloud-mode advertised setup_method=logs_token (F9 regression)")
	}
}

// TestBootstrapToken_CORSPreflightAllowsHeader pins the CORS contract: a
// browser submitting from a configured cross-origin must succeed at
// preflight for the X-Bootstrap-Token header. The default same-origin
// case doesn't trigger preflight, so this test specifically configures
// CORS to exercise the preflight path.
func TestBootstrapToken_CORSPreflightAllowsHeader(t *testing.T) {
	srv := testServer(t)
	srv.SetCORSOrigins("https://other.example.com")

	req := httptest.NewRequest("OPTIONS", "/api/v1/auth/bootstrap", nil)
	req.Header.Set("Origin", "https://other.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "X-Bootstrap-Token, Content-Type")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
		t.Fatalf("preflight: expected 200/204, got %d: %s", rr.Code, rr.Body.String())
	}
	allowed := rr.Header().Get("Access-Control-Allow-Headers")
	if allowed == "" {
		t.Fatal("preflight response missing Access-Control-Allow-Headers")
	}
	// Allow header is comma-separated; chi/cors echoes the request set if
	// they're all in the configured list. Check the bootstrap header
	// specifically.
	if !containsHeader(allowed, "X-Bootstrap-Token") {
		t.Fatalf("Access-Control-Allow-Headers missing X-Bootstrap-Token: %q", allowed)
	}
}

// containsHeader does a case-insensitive comma-separated lookup.
func containsHeader(allowed, want string) bool {
	// Lowercase compare on the simple case — preflight responses typically
	// lower-case the echo. Use strings.EqualFold-style match per token.
	for _, part := range splitCSV(allowed) {
		if equalFoldASCII(part, want) {
			return true
		}
	}
	return false
}

func splitCSV(s string) []string {
	out := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, trimSpaceASCII(s[start:i]))
			start = i + 1
		}
	}
	out = append(out, trimSpaceASCII(s[start:]))
	return out
}

func trimSpaceASCII(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t') {
		j--
	}
	return s[i:j]
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 32
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}
