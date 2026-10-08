package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TASK-3488: pad init points a Claude Code user at the pad plugin, which
// carries the /pad skill plus the panel and live notifications and updates
// itself. Printed only when Claude Code is detected and the plugin is not
// already installed (Claude Code's plugins/installed_plugins.json).

// pluginHintEnv isolates HOME, cwd and PATH (so a real `claude` binary does not
// count as detection), and clears CLAUDE_CONFIG_DIR. claudeHome creates
// ~/.claude, which is how Claude Code is detected here.
func pluginHintEnv(t *testing.T, claudeHome bool) string {
	t.Helper()
	isolateHome(t)
	withClosedStdin(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home := os.Getenv("HOME")
	if claudeHome {
		if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func writeInstalledPlugins(t *testing.T, claudeDir, body string) {
	t.Helper()
	dir := filepath.Join(claudeDir, "plugins")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "installed_plugins.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hintShown(out string) bool {
	return strings.Contains(out, "/plugin marketplace add PerpetualSoftware/pad") &&
		strings.Contains(out, "/plugin install pad@pad")
}

func TestTASK3488_PluginHint(t *testing.T) {
	t.Run("Claude Code detected, plugin not installed: both commands are printed", func(t *testing.T) {
		pluginHintEnv(t, true)
		out := captureSkillStdout(t, offerSkillInstall)
		if !hintShown(out) {
			t.Fatalf("expected the plugin commands, got:\n%s", out)
		}
		if !strings.Contains(out, "updates itself") {
			t.Errorf("expected the hint to say what the plugin carries, got:\n%s", out)
		}
	})

	t.Run("Claude Code detected, plugin installed: no hint", func(t *testing.T) {
		home := pluginHintEnv(t, true)
		writeInstalledPlugins(t, filepath.Join(home, ".claude"),
			`{"version":2,"plugins":{"pad@pad":[{"scope":"user","version":"0.3.3"}],"svelte@svelte":[]}}`)
		if out := captureSkillStdout(t, offerSkillInstall); hintShown(out) {
			t.Fatalf("hint printed although the plugin is installed:\n%s", out)
		}
	})

	t.Run("Claude Code not detected: no hint", func(t *testing.T) {
		pluginHintEnv(t, false)
		if out := captureSkillStdout(t, offerSkillInstall); hintShown(out) {
			t.Fatalf("hint printed although Claude Code was not detected:\n%s", out)
		}
	})

	t.Run("every return path ends with the hint, including 'already installed'", func(t *testing.T) {
		pluginHintEnv(t, true)
		_ = captureSkillStdout(t, offerSkillInstall) // installs the skill
		out := captureSkillStdout(t, offerSkillInstall)
		if !strings.Contains(out, "skill is installed") {
			t.Fatalf("precondition: the second run takes the already-installed return, got:\n%s", out)
		}
		if !hintShown(out) {
			t.Fatalf("the already-installed return skipped the hint:\n%s", out)
		}
	})

	t.Run("an unreadable plugins file errs toward showing the hint", func(t *testing.T) {
		home := pluginHintEnv(t, true)
		writeInstalledPlugins(t, filepath.Join(home, ".claude"), `{not json`)
		if out := captureSkillStdout(t, offerSkillInstall); !hintShown(out) {
			t.Fatalf("expected the hint when the plugins file is malformed:\n%s", out)
		}
	})
}

func TestTASK3488_ClaudePadPluginInstalled(t *testing.T) {
	t.Run("another marketplace's plugins are not pad", func(t *testing.T) {
		home := pluginHintEnv(t, true)
		writeInstalledPlugins(t, filepath.Join(home, ".claude"), `{"plugins":{"svelte@svelte":[],"padding@other":[],"pad@other":[]}}`)
		if claudePadPluginInstalled() {
			t.Fatal("a non-pad plugin read as the pad plugin")
		}
	})
	t.Run("CLAUDE_CONFIG_DIR is where Claude Code keeps its plugins when set", func(t *testing.T) {
		pluginHintEnv(t, true)
		alt := t.TempDir()
		writeInstalledPlugins(t, alt, `{"plugins":{"pad@pad":[]}}`)
		t.Setenv("CLAUDE_CONFIG_DIR", alt)
		if !claudePadPluginInstalled() {
			t.Fatal("the plugin under CLAUDE_CONFIG_DIR was not found")
		}
	})
}
