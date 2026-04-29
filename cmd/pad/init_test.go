package main

import (
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

// setupEnsureWorkspaceTest produces a clean tempdir, points HOME at a tempdir
// (so credential/config writes don't leak), chdirs into the project so
// cli.DetectWorkspace can't walk up into the real repo's .pad.toml, and
// returns the project path.
func setupEnsureWorkspaceTest(t *testing.T) string {
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

// remoteCfg builds a Config wired to point at the given test server URL.
func remoteCfg(url string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Mode = config.ModeRemote
	cfg.URL = url
	cfg.LoadedFromFile = true
	return cfg
}

// TestEnsureWorkspaceSlugAttachExisting: --workspace <slug> on a fresh dir
// links to the existing workspace and writes .pad.toml.
func TestEnsureWorkspaceSlugAttachExisting(t *testing.T) {
	project := setupEnsureWorkspaceTest(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/workspaces/my-cool-project":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   "ws-1",
				"slug": "my-cool-project",
				"name": "My Cool Project",
			})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := cli.NewClientFromURL(server.URL)
	cfg := remoteCfg(server.URL)

	ws, created, err := ensureWorkspace(client, cfg, project, "ignored-name", "my-cool-project", "")
	if err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	if created {
		t.Fatal("expected attach (not create), got created=true")
	}
	if ws == nil || ws.Slug != "my-cool-project" {
		t.Fatalf("expected workspace slug my-cool-project, got %#v", ws)
	}

	data, err := os.ReadFile(filepath.Join(project, ".pad.toml"))
	if err != nil {
		t.Fatalf("read .pad.toml: %v", err)
	}
	if !strings.Contains(string(data), `workspace = "my-cool-project"`) {
		t.Fatalf("expected workspace link in .pad.toml, got: %s", data)
	}
}

// TestEnsureWorkspaceSlugNotFound: a missing slug must produce a clear error
// AND must NOT silently fall through to "create a workspace named after the
// slug." That fallthrough was the keystone bug for the web-first onramp.
func TestEnsureWorkspaceSlugNotFound(t *testing.T) {
	project := setupEnsureWorkspaceTest(t)

	createCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/workspaces" {
			createCalled = true
			http.Error(w, "must-not-create", http.StatusInternalServerError)
			return
		}
		// 404 for any GET: the slug doesn't exist on this server.
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "not_found",
				"message": "Workspace not found",
			},
		})
	}))
	defer server.Close()

	client := cli.NewClientFromURL(server.URL)
	cfg := remoteCfg(server.URL)

	_, _, err := ensureWorkspace(client, cfg, project, "ignored-name", "missing-slug", "")
	if err == nil {
		t.Fatal("expected error when workspace slug is missing")
	}
	if !strings.Contains(err.Error(), `"missing-slug" not found`) {
		t.Fatalf("expected helpful 'not found' message, got: %v", err)
	}
	if !strings.Contains(err.Error(), server.URL) {
		t.Fatalf("expected error to mention server URL %s, got: %v", server.URL, err)
	}
	if createCalled {
		t.Fatal("must NOT create a workspace when the slug is missing")
	}
	if _, err := os.Stat(filepath.Join(project, ".pad.toml")); err == nil {
		t.Fatal(".pad.toml must not be written when slug attach fails")
	}
}

// TestEnsureWorkspaceSlugRefusesClobber: when the CWD is already linked to a
// different workspace, a slug attach must error rather than rewrite the link.
func TestEnsureWorkspaceSlugRefusesClobber(t *testing.T) {
	project := setupEnsureWorkspaceTest(t)
	if err := cli.WriteWorkspaceLink(project, "already-linked"); err != nil {
		t.Fatalf("write existing link: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// We must never hit the network — clobber check is local-first.
		t.Errorf("unexpected request when CWD is pre-linked: %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := cli.NewClientFromURL(server.URL)
	cfg := remoteCfg(server.URL)

	_, _, err := ensureWorkspace(client, cfg, project, "ignored", "my-cool-project", "")
	if err == nil {
		t.Fatal("expected error when CWD is already linked to a different workspace")
	}
	if !strings.Contains(err.Error(), "already-linked") {
		t.Fatalf("expected error to mention existing link, got: %v", err)
	}
	if !strings.Contains(err.Error(), "my-cool-project") {
		t.Fatalf("expected error to mention requested slug, got: %v", err)
	}

	// The existing link must be untouched.
	data, err := os.ReadFile(filepath.Join(project, ".pad.toml"))
	if err != nil {
		t.Fatalf("read .pad.toml: %v", err)
	}
	if !strings.Contains(string(data), `workspace = "already-linked"`) {
		t.Fatalf("existing link clobbered, got: %s", data)
	}
}

// TestEnsureWorkspaceSlugIdempotent: re-running with the same slug a directory
// is already linked to is a no-op (returns the workspace, doesn't error).
func TestEnsureWorkspaceSlugIdempotent(t *testing.T) {
	project := setupEnsureWorkspaceTest(t)
	if err := cli.WriteWorkspaceLink(project, "my-cool-project"); err != nil {
		t.Fatalf("write existing link: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/workspaces/my-cool-project":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   "ws-1",
				"slug": "my-cool-project",
				"name": "My Cool Project",
			})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := cli.NewClientFromURL(server.URL)
	cfg := remoteCfg(server.URL)

	ws, created, err := ensureWorkspace(client, cfg, project, "x", "my-cool-project", "")
	if err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	if created {
		t.Fatal("expected created=false on idempotent re-run")
	}
	if ws == nil || ws.Slug != "my-cool-project" {
		t.Fatalf("expected workspace slug my-cool-project, got %#v", ws)
	}
}

// TestEnsureWorkspaceNameDrivenStillWorks: the legacy name-driven path
// (no --workspace flag) must keep behaving as before — match by name and
// link without creating.
func TestEnsureWorkspaceNameDrivenStillWorks(t *testing.T) {
	project := setupEnsureWorkspaceTest(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/workspaces":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "ws-1", "slug": "legacy-name", "name": "Legacy Name"},
			})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := cli.NewClientFromURL(server.URL)
	cfg := remoteCfg(server.URL)

	ws, created, err := ensureWorkspace(client, cfg, project, "Legacy Name", "", "")
	if err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	if created {
		t.Fatal("expected attach (not create) when name matches existing")
	}
	if ws == nil || ws.Slug != "legacy-name" {
		t.Fatalf("expected workspace slug legacy-name, got %#v", ws)
	}

	data, err := os.ReadFile(filepath.Join(project, ".pad.toml"))
	if err != nil {
		t.Fatalf("read .pad.toml: %v", err)
	}
	if !strings.Contains(string(data), `workspace = "legacy-name"`) {
		t.Fatalf("expected workspace link, got: %s", data)
	}
}
