package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// profileAuthServer stubs the endpoints whoami and logout hit:
// GET /api/v1/health, GET /api/v1/auth/me, POST /api/v1/auth/logout.
// meByToken maps bearer token → user payload so we can assert the
// command attached the active profile, not always default.
func profileAuthServer(t *testing.T, meByToken map[string]map[string]string, logoutTokens *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/health":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/me":
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			user, ok := meByToken[token]
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(user)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/logout":
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if logoutTokens != nil {
				*logoutTokens = append(*logoutTokens, token)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Logf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

func writeV3Creds(t *testing.T, serverURL string) {
	t.Helper()
	home := os.Getenv("HOME")
	dir := filepath.Join(home, ".pad")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir ~/.pad: %v", err)
	}
	doc := map[string]any{
		"version": 3,
		"credentials": map[string]any{
			serverURL: map[string]any{
				"profiles": map[string]any{
					"default": map[string]string{
						"token":   "padsess_default",
						"user_id": "u-default",
						"email":   "dave@example.com",
						"name":    "Dave",
					},
					"cursor": map[string]string{
						"token":   "padsess_cursor",
						"user_id": "u-cursor",
						"email":   "cursor@example.com",
						"name":    "Cursor",
					},
				},
			},
		},
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), data, 0600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
}

func runAuthCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Cleanup(func() { cli.SetProfileOverride("") })
	root := newRootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(io.Discard)
	root.SetArgs(args)
	// Capture fmt.Print* which whoami/logout use instead of cmd.Print.
	orig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := root.Execute()
	_ = w.Close()
	os.Stdout = orig
	piped, _ := io.ReadAll(r)
	_ = r.Close()
	return string(piped) + stdout.String(), err
}

func TestWhoami_PrintsProfileWhenNonDefault(t *testing.T) {
	isolateHome(t)
	srv := profileAuthServer(t, map[string]map[string]string{
		"padsess_cursor": {
			"id":    "u-cursor",
			"email": "cursor@example.com",
			"name":  "Cursor",
			"role":  "member",
		},
	}, nil)
	defer srv.Close()
	t.Setenv("PAD_URL", srv.URL)
	writeV3Creds(t, srv.URL)

	out, err := runAuthCmd(t, "auth", "whoami", "--profile", "cursor")
	if err != nil {
		t.Fatalf("whoami --profile cursor: %v\nout=%s", err, out)
	}
	if !strings.Contains(out, "cursor@example.com") {
		t.Errorf("whoami should report the cursor identity, got:\n%s", out)
	}
	if !strings.Contains(out, "Profile:") || !strings.Contains(out, "cursor") {
		t.Errorf("whoami should print the active non-default profile, got:\n%s", out)
	}
}

func TestWhoami_OmitsProfileLineWhenDefault(t *testing.T) {
	isolateHome(t)
	srv := profileAuthServer(t, map[string]map[string]string{
		"padsess_default": {
			"id":    "u-default",
			"email": "dave@example.com",
			"name":  "Dave",
			"role":  "owner",
		},
	}, nil)
	defer srv.Close()
	t.Setenv("PAD_URL", srv.URL)
	writeV3Creds(t, srv.URL)

	out, err := runAuthCmd(t, "auth", "whoami")
	if err != nil {
		t.Fatalf("whoami: %v\nout=%s", err, out)
	}
	if !strings.Contains(out, "dave@example.com") {
		t.Errorf("whoami should report the default identity, got:\n%s", out)
	}
	if strings.Contains(out, "Profile:") {
		t.Errorf("whoami should omit Profile when default, got:\n%s", out)
	}
}

func TestWhoami_PAD_PROFILESelectsNamedProfile(t *testing.T) {
	isolateHome(t)
	srv := profileAuthServer(t, map[string]map[string]string{
		"padsess_cursor": {
			"id":    "u-cursor",
			"email": "cursor@example.com",
			"name":  "Cursor",
			"role":  "member",
		},
	}, nil)
	defer srv.Close()
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_PROFILE", "cursor")
	writeV3Creds(t, srv.URL)

	out, err := runAuthCmd(t, "auth", "whoami")
	if err != nil {
		t.Fatalf("whoami with PAD_PROFILE: %v\nout=%s", err, out)
	}
	if !strings.Contains(out, "cursor@example.com") {
		t.Errorf("PAD_PROFILE=cursor should report cursor identity, got:\n%s", out)
	}
	if !strings.Contains(out, "Profile:") {
		t.Errorf("PAD_PROFILE=cursor should print Profile line, got:\n%s", out)
	}
}

func TestLogout_ProfileOnlyRemovesNamedProfile(t *testing.T) {
	isolateHome(t)
	var loggedOut []string
	srv := profileAuthServer(t, map[string]map[string]string{
		"padsess_cursor": {
			"id":    "u-cursor",
			"email": "cursor@example.com",
			"name":  "Cursor",
			"role":  "member",
		},
	}, &loggedOut)
	defer srv.Close()
	t.Setenv("PAD_URL", srv.URL)
	writeV3Creds(t, srv.URL)

	out, err := runAuthCmd(t, "auth", "logout", "--profile", "cursor")
	if err != nil {
		t.Fatalf("logout --profile cursor: %v\nout=%s", err, out)
	}
	if !strings.Contains(out, "Logged out") {
		t.Errorf("logout output = %q, want Logged out", out)
	}
	if len(loggedOut) != 1 || loggedOut[0] != "padsess_cursor" {
		t.Errorf("logout should invalidate the cursor token, got %v", loggedOut)
	}

	store, err := cli.LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if store.GetProfile(srv.URL, "cursor") != nil {
		t.Error("cursor profile should be gone after logout --profile cursor")
	}
	if got := store.GetProfile(srv.URL, "default"); got == nil || got.Token != "padsess_default" {
		t.Errorf("default profile must survive logout --profile cursor: %+v", got)
	}
}

func TestRootPersistentFlag_ProfileExists(t *testing.T) {
	root := newRootCmd()
	if root.PersistentFlags().Lookup("profile") == nil {
		t.Fatal("expected persistent --profile flag on root")
	}
}
