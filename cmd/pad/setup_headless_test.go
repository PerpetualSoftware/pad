package main

// Tests for the headless (non-interactive) admin bootstrap path introduced by
// BUG-988. Each test spins up an httptest.Server so no real server process is
// needed. The cfg is wired in Remote mode so cli.EnsureServer no-ops.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/config"
)

// headlessTestServer builds an httptest.Server that speaks the minimal subset
// of the Pad API that the headless bootstrap path touches:
//
//   - GET  /api/v1/auth/session  → setup_required controlled by setupRequired
//   - GET  /api/v1/health        → 200 (so EnsureServer sees a live server)
//   - POST /api/v1/auth/bootstrap → 201 with token+user, or 409 conflict when
//     alreadyInitialised is true
func headlessTestServer(t *testing.T, setupRequired bool, alreadyInitialised bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/health":
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/session":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"setup_required": setupRequired,
				"authenticated":  false,
				"setup_method":   "local_cli",
			})

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/bootstrap":
			w.Header().Set("Content-Type", "application/json")
			if alreadyInitialised {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"code":    "conflict",
						"message": "This Pad instance has already been initialized",
					},
				})
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"token": "padsess_headless_test",
				"user": map[string]interface{}{
					"id":    "user-1",
					"email": "admin@example.com",
					"name":  "Test Admin",
					"role":  "admin",
				},
			})

		default:
			t.Logf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

// remoteHeadlessCfg builds a Config wired to point at the given httptest
// server URL in Remote mode. Remote mode makes EnsureServer a no-op.
func remoteHeadlessCfg(serverURL string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Mode = config.ModeRemote
	cfg.URL = serverURL
	cfg.LoadedFromFile = true
	return cfg
}

// isolateHome redirects HOME to a temp dir and chdirs to a fresh temp
// project dir so credential writes and .pad.toml detection don't leak into
// the real user environment. Returns the project dir.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	return project
}

// TestHeadlessSetupSuccess verifies the happy path: all three flags supplied,
// server returns a 201, credentials are saved, and --format json emits a
// LoginResponse-shaped object containing the expected user and token.
func TestHeadlessSetupSuccess(t *testing.T) {
	isolateHome(t)
	srv := headlessTestServer(t, true /* setup_required */, false)
	defer srv.Close()

	cfg := remoteHeadlessCfg(srv.URL)
	client := cli.NewClientFromURL(srv.URL)

	// Capture stdout for JSON assertion
	orig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Set global formatFlag so runHeadlessSetup emits JSON
	prevFormat := formatFlag
	formatFlag = "json"
	t.Cleanup(func() {
		formatFlag = prevFormat
		os.Stdout = orig
	})

	err := runHeadlessSetup(cfg, client, "admin@example.com", "Test Admin", "correct-horse-battery-staple")
	w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	os.Stdout = orig

	if err != nil {
		t.Fatalf("runHeadlessSetup: unexpected error: %v", err)
	}

	// Verify JSON output shape
	var result map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("json parse of output: %v\noutput was: %s", err, buf.String())
	}
	user, ok := result["user"].(map[string]interface{})
	if !ok || user == nil {
		t.Fatalf("expected 'user' in JSON output, got: %s", buf.String())
	}
	if user["role"] != "admin" {
		t.Errorf("expected role=admin, got %v", user["role"])
	}
	if result["token"] == "" || result["token"] == nil {
		t.Errorf("expected non-empty token in JSON output, got: %v", result["token"])
	}

	// Verify credentials were saved
	store, err := cli.LoadStore()
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	creds := store.Get(srv.URL)
	if creds == nil || creds.Token == "" {
		t.Error("expected credentials to be saved after headless bootstrap")
	}
}

// TestHeadlessSetupMissingFlag verifies that supplying any one or two of the
// three headless flags — but not all three — produces a clear error naming
// the missing flag(s), rather than falling through to an interactive prompt
// or a panic.
func TestHeadlessSetupMissingFlag(t *testing.T) {
	cases := []struct {
		name    string
		email   string
		uname   string
		pass    string
		wantErr string
	}{
		{
			name:    "only email",
			email:   "a@b.com",
			wantErr: "--name is required",
		},
		{
			name:    "only name",
			uname:   "Admin",
			wantErr: "--email is required",
		},
		{
			name:    "only password",
			pass:    "hunter2",
			wantErr: "--email is required",
		},
		{
			name:    "email and name, missing password",
			email:   "a@b.com",
			uname:   "Admin",
			wantErr: "--password is required",
		},
		{
			name:    "email and password, missing name",
			email:   "a@b.com",
			pass:    "hunter2",
			wantErr: "--name is required",
		},
		{
			name:    "name and password, missing email",
			uname:   "Admin",
			pass:    "hunter2",
			wantErr: "--email is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			srv := headlessTestServer(t, true, false)
			defer srv.Close()

			// Build and execute setupCmd with only partial flags
			cmd := setupCmd()
			root := &cobra.Command{Use: "pad"}
			// Attach persistent flags that getConfig/formatFlag reads
			root.PersistentFlags().StringVar(&formatFlag, "format", "table", "output format")
			root.PersistentFlags().StringVar(&urlFlag, "url", "", "server URL")
			root.AddCommand(cmd)

			// Pass --url as a top-level flag so getConfig() picks it up via
			// urlFlag (which is bound to the root persistent flag).
			args := []string{"--url", srv.URL, "setup"}
			if tc.email != "" {
				args = append(args, "--email", tc.email)
			}
			if tc.uname != "" {
				args = append(args, "--name", tc.uname)
			}
			if tc.pass != "" {
				args = append(args, "--password", tc.pass)
			}
			root.SetArgs(args)

			err := root.Execute()
			if err == nil {
				t.Fatal("expected error for missing flag, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain expected hint %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestHeadlessSetupNonTTYWithoutFlags verifies that running --cli-prompt on a
// non-TTY stdin (simulated via a closed pipe) exits with a clear error
// pointing at the new headless flags rather than blocking on a pipe read.
func TestHeadlessSetupNonTTYWithoutFlags(t *testing.T) {
	isolateHome(t)
	srv := headlessTestServer(t, true, false)
	defer srv.Close()

	// Replace stdin with a closed reader so term.IsTerminal returns false
	// and any bufio.ReadString call would hit EOF immediately.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	pw.Close() // EOF immediately on any read

	origStdin := os.Stdin
	os.Stdin = pr
	t.Cleanup(func() {
		os.Stdin = origStdin
		pr.Close()
	})

	cfg := remoteHeadlessCfg(srv.URL)
	client := cli.NewClientFromURL(srv.URL)

	// promptAndBootstrap (reached via runCLISetup) should now reject without
	// blocking because stdin is not a TTY.
	_, gotErr := promptAndBootstrap(client)
	if gotErr == nil {
		t.Fatal("expected error when stdin is not a terminal, got nil")
	}
	if !strings.Contains(gotErr.Error(), "stdin is not a terminal") {
		t.Errorf("error %q should mention 'stdin is not a terminal'", gotErr.Error())
	}
	// Must also point at the headless flags so the agent knows what to use
	if !strings.Contains(gotErr.Error(), "--email") {
		t.Errorf("error %q should mention --email flag", gotErr.Error())
	}
	_ = cfg // kept for documentation — cfg used indirectly via promptAndBootstrap's error text
}

// TestHeadlessSetupAlreadyInitialised verifies that when the server reports the
// instance is already set up (409 conflict), runHeadlessSetup returns a clear
// error and — under --format json — emits a parseable error object rather than
// bare output or a raw HTTP body.
func TestHeadlessSetupAlreadyInitialised(t *testing.T) {
	isolateHome(t)
	// alreadyInitialised=true makes the mock bootstrap endpoint return 409
	srv := headlessTestServer(t, true, true)
	defer srv.Close()

	cfg := remoteHeadlessCfg(srv.URL)
	client := cli.NewClientFromURL(srv.URL)

	// Capture stdout
	origOut := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	prevFormat := formatFlag
	formatFlag = "json"
	t.Cleanup(func() {
		formatFlag = prevFormat
		os.Stdout = origOut
	})

	err := runHeadlessSetup(cfg, client, "admin@example.com", "Test Admin", "correct-horse-battery-staple")
	w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	os.Stdout = origOut

	if err == nil {
		t.Fatal("expected error for already-initialized instance, got nil")
	}
	if !strings.Contains(err.Error(), "already initialized") {
		t.Errorf("error %q should mention 'already initialized'", err.Error())
	}

	// JSON output must be parseable and contain an error marker
	if buf.Len() > 0 {
		var result map[string]interface{}
		if jerr := json.Unmarshal(buf.Bytes(), &result); jerr != nil {
			t.Fatalf("json parse of error output: %v\noutput was: %s", jerr, buf.String())
		}
		errObj, ok := result["error"].(map[string]interface{})
		if !ok || errObj == nil {
			t.Errorf("expected 'error' object in JSON output, got: %s", buf.String())
		}
		if code, _ := errObj["code"].(string); code != "already_initialized" {
			t.Errorf("expected error.code=already_initialized, got %q", code)
		}
	}
}

// TestHeadlessInitPathSuccess verifies that `pad init` with all three headless
// flags drives the bootstrap step and then falls through to complete the rest
// of init — specifically that a workspace is created. An accidental early
// return after the bootstrap would mean the workspace step never runs, and
// this test catches that.
func TestHeadlessInitPathSuccess(t *testing.T) {
	project := isolateHome(t)

	workspaceCreated := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/health":
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/session":
			// First call: setup_required. Subsequent calls (after bootstrap):
			// authenticated so pad init doesn't try to browser-login again.
			// We detect "after bootstrap" by whether workspaceCreated has
			// been set yet — simpler: just always return authenticated after
			// the bootstrap call, which we mark via workspaceCreated-adjacent
			// state. Use a closure bool instead.
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"setup_required": !workspaceCreated,
				"authenticated":  workspaceCreated,
				"setup_method":   "local_cli",
			})

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/bootstrap":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"token": "padsess_init_test",
				"user": map[string]interface{}{
					"id":    "user-1",
					"email": "admin@example.com",
					"name":  "Init Admin",
					"role":  "admin",
				},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/workspaces":
			_ = json.NewEncoder(w).Encode([]interface{}{})

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/workspaces":
			workspaceCreated = true
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   "ws-1",
				"slug": "test-project",
				"name": "test-project",
			})

		default:
			// Workspace GET, skills, etc. — return safe empty responses
			t.Logf("init test: %s %s", r.Method, r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{})
		}
	}))
	defer srv.Close()

	prevFormat := formatFlag
	formatFlag = "table"
	t.Cleanup(func() { formatFlag = prevFormat })

	cmd := padInitCmd()
	root := &cobra.Command{Use: "pad"}
	root.PersistentFlags().StringVar(&workspaceFlag, "workspace", "", "workspace slug")
	root.PersistentFlags().StringVar(&urlFlag, "url", "", "server URL")
	root.PersistentFlags().StringVar(&formatFlag, "format", "table", "output format")
	root.AddCommand(cmd)

	root.SetArgs([]string{
		"--url", srv.URL,
		"init",
		"test-project",
		"--email", "admin@example.com",
		"--name", "Init Admin",
		"--password", "correct-horse-battery-staple",
		"--template", "startup",
	})

	// Suppress output during test
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)

	if err := root.Execute(); err != nil {
		t.Fatalf("pad init: unexpected error: %v (output: %s)", err, out.String())
	}

	if !workspaceCreated {
		t.Error("expected workspace to be created after headless bootstrap; got workspaceCreated=false — likely an accidental early return")
	}

	// Verify credentials saved
	store, serr := cli.LoadStore()
	if serr != nil {
		t.Fatalf("load store: %v", serr)
	}
	creds := store.Get(srv.URL)
	if creds == nil || creds.Token == "" {
		t.Error("expected credentials to be saved after headless init bootstrap")
	}

	_ = project // used implicitly by os.Chdir in isolateHome
}

// TestHeadlessSetupSendsBootstrapToken verifies that when the DataDir contains
// a .bootstrap-token file, doHeadlessBootstrap (and thus the headless path of
// both setup and init) forwards it as the X-Bootstrap-Token header. This is
// required for self-host deployments where the server uses setup_method=logs_token
// and would otherwise reject the request with 403 (finding #1 from round-1 codex
// review).
func TestHeadlessSetupSendsBootstrapToken(t *testing.T) {
	isolateHome(t)

	tokenReceived := ""

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/health":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/bootstrap":
			tokenReceived = r.Header.Get("X-Bootstrap-Token")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"token": "padsess_token_test",
				"user": map[string]interface{}{
					"id": "u1", "email": "a@b.com", "name": "A", "role": "admin",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := remoteHeadlessCfg(srv.URL)
	// Write a .bootstrap-token file into DataDir so ReadBootstrapToken finds it
	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		t.Fatalf("mkdir datadir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, ".bootstrap-token"), []byte("test-token-value\n"), 0600); err != nil {
		t.Fatalf("write bootstrap token: %v", err)
	}

	client := cli.NewClientFromURL(srv.URL)
	resp, err := doHeadlessBootstrap(cfg, client, "a@b.com", "A", "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("doHeadlessBootstrap: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if tokenReceived != "test-token-value" {
		t.Errorf("expected X-Bootstrap-Token=test-token-value, got %q", tokenReceived)
	}
}

// TestHeadlessSetupNoTokenFileOK verifies that when no .bootstrap-token file
// exists, doHeadlessBootstrap silently omits the header (best-effort) and the
// request still succeeds — covering the pure-loopback case where no token is
// required.
func TestHeadlessSetupNoTokenFileOK(t *testing.T) {
	isolateHome(t)

	tokenReceived := "sentinel" // will be overwritten to "" if no header sent

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/health":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/bootstrap":
			tokenReceived = r.Header.Get("X-Bootstrap-Token")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"token": "padsess_notoken_test",
				"user": map[string]interface{}{
					"id": "u1", "email": "a@b.com", "name": "A", "role": "admin",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := remoteHeadlessCfg(srv.URL)
	// DataDir does NOT contain a .bootstrap-token file
	client := cli.NewClientFromURL(srv.URL)
	_, err := doHeadlessBootstrap(cfg, client, "a@b.com", "A", "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("doHeadlessBootstrap without token file: %v", err)
	}
	if tokenReceived != "" {
		t.Errorf("expected no X-Bootstrap-Token header when file absent, got %q", tokenReceived)
	}
}

// TestReadPasswordBufioFallback verifies that readPassword's bufio fallback
// returns a line from a non-TTY (pipe) reader. It exercises readPassword IN
// ISOLATION only — it does NOT cover doInteractiveLogin end-to-end.
//
// Known limitation (BUG-1886): doInteractiveLogin constructs its own
// bufio.Reader on stdin for the email prompt, then readPassword constructs a
// second independent bufio.Reader on the same fd. Because bufio reads ahead in
// chunks the first reader can consume bytes belonging to the password line,
// breaking piped `pad auth login --interactive`. That bug predates BUG-988
// and is tracked separately.
func TestReadPasswordBufioFallback(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	// Write the password before switching stdin (pipe buffer is large enough)
	if _, err := pw.WriteString("my-piped-password\n"); err != nil {
		t.Fatalf("write password to pipe: %v", err)
	}
	pw.Close()

	origStdin := os.Stdin
	os.Stdin = pr
	t.Cleanup(func() {
		os.Stdin = origStdin
		pr.Close()
	})

	got, err := readPassword()
	if err != nil {
		t.Fatalf("readPassword with piped input: %v", err)
	}
	if got != "my-piped-password" {
		t.Errorf("readPassword = %q, want %q", got, "my-piped-password")
	}
}
