package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/config"
)

func TestCollectServerInfoRemoteAuthenticated(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	projectDir := t.TempDir()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	if err := cli.WriteWorkspaceLink(projectDir, "pad", ""); err != nil {
		t.Fatalf("write workspace link: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/auth/session":
			if got := r.Header.Get("Authorization"); got != "Bearer padsess_test" {
				t.Fatalf("unexpected auth header %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authenticated":  true,
				"setup_required": false,
				"auth_method":    "password",
				"user": map[string]any{
					"id":    "user-1",
					"email": "dave@example.com",
					"name":  "Dave",
					"role":  "owner",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	store := &cli.CredentialStore{}
	store.Set(server.URL, &cli.Credentials{
		Token:  "padsess_test",
		UserID: "user-1",
		Email:  "dave@example.com",
		Name:   "Dave",
	})
	if err := store.Save(); err != nil {
		t.Fatalf("save credentials: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Mode = config.ModeRemote
	cfg.URL = server.URL
	cfg.LoadedFromFile = true

	report, err := collectServerInfo(cfg)
	if err != nil {
		t.Fatalf("collectServerInfo: %v", err)
	}

	if !report.Connection.Reachable {
		t.Fatal("expected server to be reachable")
	}
	if !report.Auth.CredentialsPresent {
		t.Fatal("expected credentials to be present")
	}
	if !report.Auth.CredentialsMatchServer {
		t.Fatal("expected credentials to match configured server")
	}
	if !report.Auth.Authenticated || !report.Auth.SessionValid {
		t.Fatalf("expected authenticated valid session, got auth=%v valid=%v", report.Auth.Authenticated, report.Auth.SessionValid)
	}
	if report.Auth.User == nil || report.Auth.User.Email != "dave@example.com" {
		t.Fatalf("expected authenticated user info, got %#v", report.Auth.User)
	}
	if report.Workspace.Current != "pad" {
		t.Fatalf("expected workspace pad, got %q", report.Workspace.Current)
	}
	if report.Local != nil {
		t.Fatal("expected no local runtime section for remote mode")
	}
}

func TestCollectServerInfoLocalRuntime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.DefaultConfig()
	cfg.Mode = config.ModeLocal
	cfg.LoadedFromFile = true
	cfg.Port = 65530

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		t.Fatalf("mkdir db dir: %v", err)
	}
	if err := os.WriteFile(cfg.DBPath, []byte("sqlite-data"), 0o644); err != nil {
		t.Fatalf("write db: %v", err)
	}
	if err := os.WriteFile(cfg.PIDFile(), []byte("4321"), 0o644); err != nil {
		t.Fatalf("write pid: %v", err)
	}

	report, err := collectServerInfo(cfg)
	if err != nil {
		t.Fatalf("collectServerInfo: %v", err)
	}

	if report.Local == nil {
		t.Fatal("expected local runtime info")
	}
	if report.Local.BindAddr != "127.0.0.1:65530" {
		t.Fatalf("unexpected bind addr %q", report.Local.BindAddr)
	}
	if report.Local.PID == nil || *report.Local.PID != 4321 {
		t.Fatalf("expected pid 4321, got %#v", report.Local.PID)
	}
	if !report.Local.DBExists {
		t.Fatal("expected DB to exist")
	}
	if report.Local.DBSizeBytes != int64(len("sqlite-data")) {
		t.Fatalf("unexpected db size %d", report.Local.DBSizeBytes)
	}
	if report.Connection.Reachable {
		t.Fatal("expected local server to be unreachable in test")
	}
}

func TestIncludeLocalRuntimeWhenUnconfigured(t *testing.T) {
	cfg := config.DefaultConfig()
	if !includeLocalRuntime(cfg) {
		t.Fatal("expected unconfigured default client to include local runtime defaults")
	}

	cfg.URL = "https://pad.example.com"
	if includeLocalRuntime(cfg) {
		t.Fatal("expected URL-configured client to skip local runtime")
	}
}

func TestServerInfoReportsIdentityFingerprint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_TOKEN", "probe-must-not-send-this")
	priv := ed25519.NewKeyFromSeed(make([]byte, 32))
	pub := priv.Public().(ed25519.PublicKey)
	// Fixed independently of the response under test.
	const fingerprint = "139e-3940-e64b-5491-7220-88d9-a0d7-4162"
	for _, mode := range []string{"valid", "bad_signature", "not_found", "wrong_version", "wrong_algorithm"} {
		t.Run(mode, func(t *testing.T) {
			probes := 0
			seen := map[string]bool{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/health":
					w.WriteHeader(200)
				case "/api/v1/auth/session":
					_, _ = w.Write([]byte(`{"authenticated":false}`))
				case "/api/v1/server/identity":
					probes++
					if r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Pad-Agent") != "" || r.URL.User != nil {
						t.Error("identity probe carried credentials or unexpected method")
					}
					value := r.URL.Query().Get("challenge")
					challenge, err := base64.RawURLEncoding.Strict().DecodeString(value)
					if err != nil || len(challenge) != 32 || seen[value] {
						t.Error("challenge is not fresh 32-byte randomness")
					}
					seen[value] = true
					if mode == "not_found" {
						http.NotFound(w, r)
						return
					}
					message := append([]byte("pad-server-identity/v1/ed25519/challenge\x00"), pub...)
					message = append(message, challenge...)
					sig := ed25519.Sign(priv, message)
					if mode == "bad_signature" {
						sig[0] ^= 1
					}
					version, algorithm := 1, "ed25519"
					if mode == "wrong_version" {
						version = 2
					}
					if mode == "wrong_algorithm" {
						algorithm = "other"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"version": version, "algorithm": algorithm, "public_key": base64.RawURLEncoding.EncodeToString(pub), "signature": base64.RawURLEncoding.EncodeToString(sig)})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			cfg := config.DefaultConfig()
			cfg.Mode = config.ModeRemote
			cfg.URL = srv.URL
			cfg.LoadedFromFile = true
			for i := 0; i < 2; i++ {
				client := cli.NewClientFromURLWithAgentName(srv.URL, "")
				client.SetHTTPClient(srv.Client())
				report, err := collectServerInfoWithClient(cfg, client)
				if err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(report)
				if err != nil {
					t.Fatal(err)
				}
				table := captureStdout(t, func() { printServerInfo(report) })
				if mode == "valid" {
					if !strings.Contains(string(data), `"fingerprint":"`+fingerprint+`"`) || !strings.Contains(table, fingerprint) {
						t.Fatalf("fingerprint missing: %s\n%s", data, table)
					}
				} else {
					if strings.Contains(string(data), `"fingerprint"`) || strings.Contains(table, fingerprint) || !strings.Contains(string(data), "could not be verified") || !strings.Contains(table, "could not be verified") {
						t.Fatalf("unverified identity output: %s\n%s", data, table)
					}
				}
			}
			if probes != 2 {
				t.Fatalf("identity probes = %d, want 2", probes)
			}
		})
	}
}

func TestServerInfoSkipsIdentityProbeWhenUnhealthy(t *testing.T) {
	probes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/server/identity" {
			probes++
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	cfg := config.DefaultConfig()
	cfg.Mode = config.ModeRemote
	cfg.URL = srv.URL
	cfg.LoadedFromFile = true
	client := cli.NewClientFromURLWithAgentName(srv.URL, "")
	client.SetHTTPClient(srv.Client())
	report, err := collectServerInfoWithClient(cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	if report.Connection.Reachable || probes != 0 {
		t.Fatalf("reachable=%v probes=%d", report.Connection.Reachable, probes)
	}
	if report.Connection.Fingerprint != "" || !strings.Contains(report.Connection.IdentityError, "could not be verified") {
		t.Fatalf("identity report: %+v", report.Connection)
	}
}
