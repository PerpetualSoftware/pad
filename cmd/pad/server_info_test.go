package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/xarmian/pad/internal/cli"
	"github.com/xarmian/pad/internal/config"
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

	if err := cli.WriteWorkspaceLink(projectDir, "pad"); err != nil {
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
				"authenticated": true,
				"setup_required": false,
				"auth_method": "password",
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

	if err := cli.SaveCredentials(&cli.Credentials{
		ServerURL: server.URL,
		Token:     "padsess_test",
		UserID:    "user-1",
		Email:     "dave@example.com",
		Name:      "Dave",
	}); err != nil {
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
