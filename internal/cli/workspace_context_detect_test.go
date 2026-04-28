package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
)

func TestDetectProjectCapturesMakeAndNestedWebSignals(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test\n"), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("install:\n\tgo build ./...\n\nbuild:\n\tgo build ./...\n\ntest:\n\tgo test ./...\n\ndev:\n\tgo run ./cmd/pad\n"), 0644); err != nil {
		t.Fatalf("write Makefile: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "web"), 0755); err != nil {
		t.Fatalf("mkdir web: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "web", "package.json"), []byte(`{"dependencies":{"@sveltejs/kit":"^2.0.0"},"scripts":{"build":"vite build","dev":"vite dev","lint":"eslint ."}}`), 0644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}

	info := DetectProject(dir)
	if info.Language != "go" {
		t.Fatalf("expected language go, got %q", info.Language)
	}
	if info.BuildCmd != "make build" {
		t.Fatalf("expected build command make build, got %q", info.BuildCmd)
	}
	if info.SetupCmd != "make install" {
		t.Fatalf("expected setup command make install, got %q", info.SetupCmd)
	}
	if info.DevCmd != "make dev" {
		t.Fatalf("expected dev command make dev, got %q", info.DevCmd)
	}
	if info.WebBuildCmd != "npm run build" {
		t.Fatalf("expected web build command npm run build, got %q", info.WebBuildCmd)
	}
	if len(info.Frameworks) == 0 || info.Frameworks[0] != "sveltekit" {
		t.Fatalf("expected sveltekit framework detection, got %#v", info.Frameworks)
	}
}

func TestBuildWorkspaceContext(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "web"), 0755); err != nil {
		t.Fatalf("mkdir web: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "internal", "server"), 0755); err != nil {
		t.Fatalf("mkdir server: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0755); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "..", "pad-web"), 0755); err != nil {
		t.Fatalf("mkdir docs repo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "..", "pad-web", "package.json"), []byte(`{}`), 0644); err != nil {
		t.Fatalf("write sibling package.json: %v", err)
	}

	info := ProjectInfo{
		Language:        "go",
		BuildCmd:        "make build",
		SetupCmd:        "make install",
		TestCmd:         "go test ./...",
		WebBuildCmd:     "npm run build",
		PackageManagers: []string{"go", "npm"},
		Frameworks:      []string{"sveltekit"},
	}
	cfg := &config.Config{Mode: config.ModeLocal, URL: "http://127.0.0.1:7777", Host: "127.0.0.1"}

	context := BuildWorkspaceContext(dir, info, cfg)
	if context.Paths == nil || context.Paths.Web != "web" {
		t.Fatalf("expected web path, got %#v", context.Paths)
	}
	if context.Paths == nil || context.Paths.DocsRepo != "../pad-web" {
		t.Fatalf("expected docs repo path, got %#v", context.Paths)
	}
	if context.Commands == nil || context.Commands.Setup != "make install" {
		t.Fatalf("expected setup command, got %#v", context.Commands)
	}
	if context.Stack == nil || len(context.Stack.PackageManagers) != 2 {
		t.Fatalf("expected package managers, got %#v", context.Stack)
	}
	if context.Deployment == nil || context.Deployment.Mode != config.ModeLocal {
		t.Fatalf("expected local deployment, got %#v", context.Deployment)
	}
	if len(context.Repositories) < 2 {
		t.Fatalf("expected primary and docs repositories, got %#v", context.Repositories)
	}
}

func TestNormalizeGitRemoteSlug(t *testing.T) {
	if got := normalizeGitRemoteSlug("git@github.com:PerpetualSoftware/pad.git\n"); got != "PerpetualSoftware/pad" {
		t.Fatalf("expected SSH slug PerpetualSoftware/pad, got %q", got)
	}
	if got := normalizeGitRemoteSlug("https://github.com/PerpetualSoftware/pad.git"); got != "PerpetualSoftware/pad" {
		t.Fatalf("expected HTTPS slug PerpetualSoftware/pad, got %q", got)
	}
	if got := normalizeGitRemoteSlug("ssh://git@example.com/foo"); got != "" {
		t.Fatalf("expected unsupported remote to be blank, got %q", got)
	}
}
