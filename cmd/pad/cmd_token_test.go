package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tests for the `pad token` command group (#879 follow-up): CLI mint /
// list / revoke against the existing user-scoped /api/v1/auth/tokens
// endpoints, so the PAD_TOKEN story works end-to-end without the web UI.
//
// HOME and USERPROFILE are both set (via setTempHomeMain) because
// CredentialsPath resolves via os.UserHomeDir (HOME on Unix, USERPROFILE
// on Windows).

// stubTokenServer serves the user-scoped token endpoints for exactly one
// bearer token and records what it was asked to do.
type stubTokenServer struct {
	*httptest.Server
	wantToken    string
	createBodies []map[string]any
	deletePaths  []string
	listHits     int
}

func newStubTokenServer(t *testing.T, wantToken string) *stubTokenServer {
	t.Helper()
	s := &stubTokenServer{wantToken: wantToken}
	mux := http.NewServeMux()
	authorized := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+s.wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": "unauthorized", "message": "Not logged in"},
			})
			return false
		}
		return true
	}
	mux.HandleFunc("/api/v1/auth/tokens", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.listHits++
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"id": "tok-1111", "name": "ci-agent", "prefix": "pad_abc1",
					"created_at":   "2026-08-01T10:00:00Z",
					"last_used_at": "2026-09-01T09:00:00Z",
					"expires_at":   "2026-11-01T10:00:00Z",
				},
				{
					"id": "tok-2222", "name": "sweep", "prefix": "pad_def2",
					"created_at": "2026-08-15T10:00:00Z",
				},
			})
		case http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.createBodies = append(s.createBodies, body)
			if name, _ := body["name"].(string); name == "" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"code": "bad_request", "message": "name is required"},
				})
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "tok-new", "name": body["name"], "prefix": "pad_new1",
				"created_at": "2026-09-02T10:00:00Z",
				"expires_at": "2026-12-01T10:00:00Z",
				"token":      "pad_new1secretsecretsecret",
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/v1/auth/tokens/", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.deletePaths = append(s.deletePaths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/tok-missing") {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": "not_found", "message": "Token not found"},
			})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func setupTokenEnv(t *testing.T) *stubTokenServer {
	t.Helper()
	setTempHomeMain(t) // empty credential store; PAD_TOKEN below is the auth
	srv := newStubTokenServer(t, "pad_envtoken")
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_envtoken")
	return srv
}

// create mints via POST /auth/tokens and prints the secret exactly once,
// with a store-it-now notice — the server never returns it again.
func TestTokenCreate_PrintsSecretOnceWithNotice(t *testing.T) {
	srv := setupTokenEnv(t)

	cmd := tokenCreateCmd()
	cmd.SetArgs([]string{"--name", "ci-agent", "--expires-in", "30"})
	var runErr error
	out := captureStdout(t, func() {
		runErr = cmd.Execute()
	})
	if runErr != nil {
		t.Fatalf("token create: %v", runErr)
	}
	if len(srv.createBodies) != 1 {
		t.Fatalf("expected exactly one POST /auth/tokens, got %d", len(srv.createBodies))
	}
	body := srv.createBodies[0]
	if body["name"] != "ci-agent" {
		t.Errorf("posted name = %v, want ci-agent", body["name"])
	}
	if n, _ := body["expires_in"].(float64); int(n) != 30 {
		t.Errorf("posted expires_in = %v, want 30", body["expires_in"])
	}
	if !strings.Contains(out, "pad_new1secretsecretsecret") {
		t.Errorf("output must contain the minted token once:\n%s", out)
	}
	if strings.Count(out, "pad_new1secretsecretsecret") != 1 {
		t.Errorf("the secret must appear exactly once:\n%s", out)
	}
	lower := strings.ToLower(out)
	if !strings.Contains(lower, "store") && !strings.Contains(lower, "shown") {
		t.Errorf("output must warn the token is shown only now:\n%s", out)
	}
}

// create without --name refuses locally — the flag is required, so the
// server is never asked to reject it.
func TestTokenCreate_RequiresName(t *testing.T) {
	srv := setupTokenEnv(t)

	cmd := tokenCreateCmd()
	cmd.SetArgs([]string{})
	cmd.SetOut(nil)
	cmd.SetErr(nil)
	var runErr error
	captureStdout(t, func() {
		runErr = cmd.Execute()
	})
	if runErr == nil {
		t.Fatal("expected an error when --name is missing")
	}
	if len(srv.createBodies) != 0 {
		t.Errorf("no POST should reach the server on a missing name, got %d", len(srv.createBodies))
	}
}

// list renders metadata for every token and never a secret — the server
// doesn't return secrets on list, and the renderer must not invent one.
func TestTokenList_RendersMetadataWithoutSecret(t *testing.T) {
	srv := setupTokenEnv(t)

	cmd := tokenListCmd()
	var runErr error
	out := captureStdout(t, func() {
		runErr = cmd.RunE(cmd, nil)
	})
	if runErr != nil {
		t.Fatalf("token list: %v", runErr)
	}
	if srv.listHits != 1 {
		t.Fatalf("expected one GET /auth/tokens, got %d", srv.listHits)
	}
	for _, want := range []string{"ci-agent", "sweep", "pad_abc1", "pad_def2", "tok-1111", "tok-2222"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret") {
		t.Errorf("list output must never carry secret material:\n%s", out)
	}
}

// revoke DELETEs the exact id it was given.
func TestTokenRevoke_DeletesById(t *testing.T) {
	srv := setupTokenEnv(t)

	cmd := tokenRevokeCmd()
	var runErr error
	out := captureStdout(t, func() {
		runErr = cmd.RunE(cmd, []string{"tok-1111"})
	})
	if runErr != nil {
		t.Fatalf("token revoke: %v", runErr)
	}
	if len(srv.deletePaths) != 1 || !strings.HasSuffix(srv.deletePaths[0], "/auth/tokens/tok-1111") {
		t.Errorf("delete paths = %v, want exactly one ending in /auth/tokens/tok-1111", srv.deletePaths)
	}
	if !strings.Contains(strings.ToLower(out), "revoked") {
		t.Errorf("output should confirm the revoke:\n%s", out)
	}
}

// revoke surfaces the server's 404 instead of claiming success — an id
// typo must not read as a revoked token.
func TestTokenRevoke_NotFoundSurfacesError(t *testing.T) {
	setupTokenEnv(t)

	cmd := tokenRevokeCmd()
	var runErr error
	captureStdout(t, func() {
		runErr = cmd.RunE(cmd, []string{"tok-missing"})
	})
	if runErr == nil {
		t.Fatal("expected an error for an unknown token id")
	}
	if !strings.Contains(runErr.Error(), "not found") && !strings.Contains(runErr.Error(), "Token not found") {
		t.Errorf("error should say the token was not found, got %q", runErr.Error())
	}
}
