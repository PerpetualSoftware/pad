package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the PAD_TOKEN override's command-level behaviour (issue
// #879, layer 1): whoami reports the effective identity, and logout
// never invalidates the env token's session.
//
// HOME and USERPROFILE are both set because CredentialsPath resolves
// via os.UserHomeDir (HOME on Unix, USERPROFILE on Windows).

func setTempHomeMain(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PAD_MODE", "")
	// Escape any ancestor .pad.toml: applyPadTomlOverride walks up from
	// CWD and its URL pin would override the PAD_URL these tests set
	// (same pattern as TestMonitorClient_UnconfiguredReturnsError).
	t.Chdir(t.TempDir())
	return home
}

func writeCredStore(t *testing.T, home, serverURL, token string) {
	t.Helper()
	dir := filepath.Join(home, ".pad")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `{"version": 2, "credentials": {"` + serverURL + `": {
		"server_url": "` + serverURL + `", "token": "` + token + `",
		"user_id": "u-1", "email": "stored@example.com", "name": "Stored User"}}}`
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(body), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// stubAuthServer serves /api/v1/auth/me for exactly one bearer token and
// records the Authorization header of any /api/v1/auth/logout call.
type stubAuthServer struct {
	*httptest.Server
	meHits       int
	logoutTokens []string
}

func newStubAuthServer(t *testing.T, wantToken string) *stubAuthServer {
	t.Helper()
	s := &stubAuthServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/me", func(w http.ResponseWriter, r *http.Request) {
		s.meHits++
		if r.Header.Get("Authorization") != "Bearer "+wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "unauthorized"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "u-agent", "email": "agent@example.com",
			"name": "Agent User", "role": "member",
		})
	})
	mux.HandleFunc("/api/v1/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		s.logoutTokens = append(s.logoutTokens, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// With PAD_TOKEN set and NO stored credentials, whoami must authenticate
// with the env token instead of printing "Not logged in." — the store
// short-circuit would make whoami lie about the identity every other
// command uses.
func TestWhoami_UsesEnvToken(t *testing.T) {
	setTempHomeMain(t) // empty credential store
	srv := newStubAuthServer(t, "pad_envtoken")
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_envtoken")

	cmd := whoamiCmd()
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("whoami with PAD_TOKEN: %v", err)
	}
	if srv.meHits == 0 {
		t.Error("stub /auth/me never contacted — whoami short-circuited on the store lookup")
	}
}

// Without PAD_TOKEN and without credentials, whoami keeps its existing
// "Not logged in" behaviour and never contacts the server.
func TestWhoami_NoTokenNoStoreUnchanged(t *testing.T) {
	setTempHomeMain(t)
	srv := newStubAuthServer(t, "unused")
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "")

	cmd := whoamiCmd()
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if srv.meHits != 0 {
		t.Errorf("expected no server contact when unauthenticated, got %d /auth/me hits", srv.meHits)
	}
}

// logout manages the STORED session. Under PAD_TOKEN, the client
// constructor resolves the env token, so an unpinned client.Logout()
// would invalidate the env token's server-side session — the one thing
// `pad auth logout` must never do (#879 grounding note 2: identity
// switching is where the concurrency pain lives).
func TestLogout_UnderEnvTokenInvalidatesStoredSessionOnly(t *testing.T) {
	home := setTempHomeMain(t)
	srv := newStubAuthServer(t, "unused")
	writeCredStore(t, home, srv.URL, "padsess_stored")
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_envtoken")

	cmd := logoutCmd()
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("logout: %v", err)
	}
	for _, tok := range srv.logoutTokens {
		if tok == "Bearer pad_envtoken" {
			t.Fatal("logout invalidated the PAD_TOKEN session — must pin to the stored session")
		}
	}
	if len(srv.logoutTokens) != 1 || srv.logoutTokens[0] != "Bearer padsess_stored" {
		t.Errorf("logout tokens = %v, want exactly the stored session", srv.logoutTokens)
	}
	// The stored entry itself is still deleted — that part is unchanged.
	data, err := os.ReadFile(filepath.Join(home, ".pad", "credentials.json"))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if got := string(data); got != "" && containsToken(got, "padsess_stored") {
		t.Errorf("stored credential survived logout: %s", got)
	}
}

// Under PAD_TOKEN with NO stored session there is nothing server-side
// for logout to invalidate — it must not fire /auth/logout with the env
// token.
func TestLogout_UnderEnvTokenNoStoreSkipsServerCall(t *testing.T) {
	setTempHomeMain(t)
	srv := newStubAuthServer(t, "unused")
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_envtoken")

	cmd := logoutCmd()
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if len(srv.logoutTokens) != 0 {
		t.Errorf("expected no /auth/logout call, got tokens %v", srv.logoutTokens)
	}
}

func containsToken(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// envTokenNotice is the login/logout disclosure line (gh-style): present
// exactly when PAD_TOKEN is set.
func TestEnvTokenNotice(t *testing.T) {
	t.Setenv("PAD_TOKEN", "pad_x")
	if got := envTokenNotice(); got == "" {
		t.Fatal("expected a notice when PAD_TOKEN is set")
	}
	t.Setenv("PAD_TOKEN", "")
	if got := envTokenNotice(); got != "" {
		t.Errorf("expected empty notice when unset, got %q", got)
	}
}

// stubSessionServer extends the auth stub with /api/v1/auth/session
// control, for driving init/login flows. meTokens records every
// Authorization header /auth/me saw, so a test can prove which
// identity a command actually consulted.
type stubSessionServer struct {
	*httptest.Server
	meTokens      []string
	meAccept      string // bearer token /auth/me accepts
	sessionStatus int    // HTTP status for /auth/session
}

func newStubSessionServer(t *testing.T, meAccept string, sessionStatus int) *stubSessionServer {
	t.Helper()
	s := &stubSessionServer{meAccept: meAccept, sessionStatus: sessionStatus}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/session", func(w http.ResponseWriter, r *http.Request) {
		if s.sessionStatus != http.StatusOK {
			w.WriteHeader(s.sessionStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authenticated": false, "setup_required": false,
		})
	})
	mux.HandleFunc("/api/v1/auth/me", func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("Authorization")
		s.meTokens = append(s.meTokens, tok)
		if tok != "Bearer "+s.meAccept {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "unauthorized"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "u-stored", "email": "stored@example.com",
			"name": "Stored User", "role": "member",
		})
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// TestInit_RejectedEnvTokenFailsInsteadOfStoredFallback covers PR #1160
// round-1 bug 1: with PAD_TOKEN set to a token the server rejects,
// `pad init`'s auth step used to fall back to valid stored credentials
// and silently run as that user — then report the override as active.
// Under the override, an unauthenticated session must fail with the
// distinct rejected-token message and never consult the stored
// identity.
func TestInit_RejectedEnvTokenFailsInsteadOfStoredFallback(t *testing.T) {
	home := setTempHomeMain(t)
	// /auth/me accepts ONLY the stored token — the env token is rejected.
	srv := newStubSessionServer(t, "padsess_stored", http.StatusOK)
	writeCredStore(t, home, srv.URL, "padsess_stored")
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_rejected")

	cmd := padInitCmd()
	var runErr error
	out := captureStdout(t, func() {
		runErr = cmd.RunE(cmd, nil)
	})
	if runErr == nil {
		t.Fatalf("expected init to fail under a rejected PAD_TOKEN; stdout:\n%s", out)
	}
	if !strings.Contains(runErr.Error(), "PAD_TOKEN") {
		t.Errorf("error %q should name PAD_TOKEN as the cause", runErr.Error())
	}
	for _, tok := range srv.meTokens {
		if tok == "Bearer padsess_stored" {
			t.Error("init consulted the stored identity under a rejected PAD_TOKEN — the silent-fallback bug")
		}
	}
	if strings.Contains(out, "PAD_TOKEN environment override") {
		t.Errorf("status output claims the override was active on a failed init:\n%s", out)
	}
}

// TestLogin_UnderEnvTokenSkipsStoredShortcut covers PR #1160 round-1
// bug 2: `login` printed "Already logged in as <stored user>" off the
// credential store even while PAD_TOKEN was overriding every API call —
// directly contradicting the envTokenNotice printed above it. Under the
// override the shortcut must not fire; login proceeds to the real flow
// (which in this test fails fast at CheckSession — the stub 500s — and
// that error is the proof the shortcut was skipped).
func TestLogin_UnderEnvTokenSkipsStoredShortcut(t *testing.T) {
	home := setTempHomeMain(t)
	srv := newStubSessionServer(t, "padsess_stored", http.StatusInternalServerError)
	writeCredStore(t, home, srv.URL, "padsess_stored")
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_envtoken")

	cmd := loginCmd()
	var runErr error
	out := captureStdout(t, func() {
		runErr = cmd.RunE(cmd, nil)
	})
	if strings.Contains(out, "Already logged in") {
		t.Errorf("stored-credential shortcut fired under PAD_TOKEN:\n%s", out)
	}
	if runErr == nil {
		t.Error("expected login to proceed past the shortcut and fail at the stubbed session check")
	}
}

// TestLogin_NoEnvTokenShortcutUnchanged pins the pre-existing behaviour:
// without PAD_TOKEN, valid stored credentials still short-circuit login
// with the "Already logged in" message.
func TestLogin_NoEnvTokenShortcutUnchanged(t *testing.T) {
	home := setTempHomeMain(t)
	srv := newStubSessionServer(t, "padsess_stored", http.StatusInternalServerError)
	writeCredStore(t, home, srv.URL, "padsess_stored")
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "")

	cmd := loginCmd()
	var runErr error
	out := captureStdout(t, func() {
		runErr = cmd.RunE(cmd, nil)
	})
	if runErr != nil {
		t.Fatalf("login with valid stored creds should short-circuit cleanly: %v", runErr)
	}
	if !strings.Contains(out, "Already logged in") {
		t.Errorf("expected the stored-credential shortcut without PAD_TOKEN:\n%s", out)
	}
}
