package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindAgent_NameAndAliases(t *testing.T) {
	cases := []struct {
		input     string
		wantName  string
		wantError bool
	}{
		{"claude-desktop", "claude-desktop", false},
		{"claude", "claude-desktop", false}, // alias
		{"Claude", "claude-desktop", false}, // case-insensitive
		{"anthropic", "claude-desktop", false},
		{"cursor", "cursor", false},
		{"windsurf", "windsurf", false},
		{"vscode", "", true}, // unsupported
		{"", "", true},       // empty
	}
	for _, c := range cases {
		got, err := FindAgent(c.input)
		if c.wantError {
			if err == nil {
				t.Errorf("FindAgent(%q) expected error, got %+v", c.input, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("FindAgent(%q): %v", c.input, err)
			continue
		}
		if got.Name != c.wantName {
			t.Errorf("FindAgent(%q).Name = %q, want %q", c.input, got.Name, c.wantName)
		}
	}
}

func TestPathResolvers_LinuxAndDarwin(t *testing.T) {
	// Path-resolution logic is platform-aware. Test against fixed
	// home + goos values so the assertions don't depend on the
	// host OS.
	cases := []struct {
		agentName string
		goos      string
		wantPath  string
	}{
		{"claude-desktop", "linux", "/h/.config/Claude/claude_desktop_config.json"},
		{"claude-desktop", "darwin", "/h/Library/Application Support/Claude/claude_desktop_config.json"},
		{"cursor", "linux", "/h/.cursor/mcp.json"},
		{"cursor", "darwin", "/h/.cursor/mcp.json"},
		{"windsurf", "linux", "/h/.codeium/windsurf/mcp_config.json"},
	}
	for _, c := range cases {
		agent, _ := FindAgent(c.agentName)
		got, err := agent.PathFor("/h", c.goos)
		if err != nil {
			t.Errorf("%s/%s: %v", c.agentName, c.goos, err)
			continue
		}
		if got != c.wantPath {
			t.Errorf("%s on %s = %q, want %q", c.agentName, c.goos, got, c.wantPath)
		}
	}
}

func TestAddPadEntry_NewConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude_desktop_config.json")

	modified, err := AddPadEntry(path, "/usr/local/bin/pad")
	if err != nil {
		t.Fatalf("AddPadEntry: %v", err)
	}
	if !modified {
		t.Errorf("expected modified=true on fresh install")
	}

	cfg := readConfig(t, path)
	servers := cfg["mcpServers"].(map[string]any)
	pad := servers[MCPServerKey].(map[string]any)
	if pad["command"] != "/usr/local/bin/pad" {
		t.Errorf("command = %v, want /usr/local/bin/pad", pad["command"])
	}
	args := pad["args"].([]any)
	if len(args) != 2 || args[0] != "mcp" || args[1] != "serve" {
		t.Errorf("args = %v, want [mcp serve]", args)
	}
}

func TestAddPadEntry_PreservesOtherServers(t *testing.T) {
	// The user has another MCP server configured (e.g. a postgres
	// MCP). Our install MUST NOT clobber that entry — only modify
	// `mcpServers.pad`.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	existing := map[string]any{
		"mcpServers": map[string]any{
			"postgres": map[string]any{
				"command": "/usr/local/bin/postgres-mcp",
				"args":    []any{"--db", "main"},
			},
		},
		"theme": "dark",
	}
	writeConfig(t, path, existing)

	if _, err := AddPadEntry(path, "/usr/bin/pad"); err != nil {
		t.Fatalf("AddPadEntry: %v", err)
	}

	cfg := readConfig(t, path)
	servers := cfg["mcpServers"].(map[string]any)
	if _, ok := servers["postgres"]; !ok {
		t.Errorf("preserved postgres entry was removed; servers=%v", servers)
	}
	if _, ok := servers["pad"]; !ok {
		t.Errorf("pad entry not added; servers=%v", servers)
	}
	// Top-level keys outside mcpServers also preserved.
	if cfg["theme"] != "dark" {
		t.Errorf("unrelated top-level key 'theme' was lost: %v", cfg["theme"])
	}
}

func TestAddPadEntry_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	if _, err := AddPadEntry(path, "/usr/bin/pad"); err != nil {
		t.Fatalf("first install: %v", err)
	}
	modified, err := AddPadEntry(path, "/usr/bin/pad")
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if modified {
		t.Errorf("re-install with identical config should report modified=false")
	}
}

func TestAddPadEntry_BinaryPathChangeMarksModified(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	if _, err := AddPadEntry(path, "/old/pad"); err != nil {
		t.Fatalf("initial: %v", err)
	}
	modified, err := AddPadEntry(path, "/new/pad")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !modified {
		t.Errorf("binary path change should mark modified=true")
	}
	cfg := readConfig(t, path)
	servers := cfg["mcpServers"].(map[string]any)
	pad := servers[MCPServerKey].(map[string]any)
	if pad["command"] != "/new/pad" {
		t.Errorf("expected updated command, got %v", pad["command"])
	}
}

func TestAddPadEntry_RequiresBinary(t *testing.T) {
	if _, err := AddPadEntry("/tmp/x", ""); err == nil {
		t.Errorf("expected error when binary path empty")
	}
}

func TestRemovePadEntry_RemovesOnlyPad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeConfig(t, path, map[string]any{
		"mcpServers": map[string]any{
			"pad":      map[string]any{"command": "/usr/bin/pad"},
			"postgres": map[string]any{"command": "/usr/bin/postgres-mcp"},
		},
	})

	removed, err := RemovePadEntry(path)
	if err != nil {
		t.Fatalf("RemovePadEntry: %v", err)
	}
	if !removed {
		t.Errorf("expected removed=true")
	}

	cfg := readConfig(t, path)
	servers := cfg["mcpServers"].(map[string]any)
	if _, ok := servers["pad"]; ok {
		t.Errorf("pad entry not removed")
	}
	if _, ok := servers["postgres"]; !ok {
		t.Errorf("postgres entry should be preserved")
	}
}

func TestRemovePadEntry_MissingFileIsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")

	removed, err := RemovePadEntry(path)
	if err != nil {
		t.Fatalf("missing file should be no-op, got: %v", err)
	}
	if removed {
		t.Errorf("expected removed=false for missing file")
	}
}

func TestRemovePadEntry_MissingEntryIsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeConfig(t, path, map[string]any{"mcpServers": map[string]any{"postgres": map[string]any{}}})

	removed, err := RemovePadEntry(path)
	if err != nil {
		t.Fatalf("RemovePadEntry: %v", err)
	}
	if removed {
		t.Errorf("expected removed=false when pad entry not present")
	}
}

func TestHasPadEntry_AllPaths(t *testing.T) {
	dir := t.TempDir()

	// missing file
	missing := filepath.Join(dir, "missing.json")
	installed, _, err := HasPadEntry(missing)
	if err != nil {
		t.Errorf("missing file: unexpected error: %v", err)
	}
	if installed {
		t.Errorf("missing file: expected installed=false")
	}

	// no mcpServers
	bare := filepath.Join(dir, "bare.json")
	writeConfig(t, bare, map[string]any{"theme": "dark"})
	installed, _, err = HasPadEntry(bare)
	if err != nil || installed {
		t.Errorf("bare config: installed=%v err=%v, want (false, nil)", installed, err)
	}

	// pad present
	full := filepath.Join(dir, "full.json")
	writeConfig(t, full, map[string]any{"mcpServers": map[string]any{"pad": map[string]any{"command": "/p"}}})
	installed, cmd, err := HasPadEntry(full)
	if err != nil {
		t.Errorf("full config: %v", err)
	}
	if !installed {
		t.Errorf("expected installed=true")
	}
	if cmd != "/p" {
		t.Errorf("command = %q, want /p", cmd)
	}
}

func TestInstaller_Install_RoundTripWithTempHome(t *testing.T) {
	tmpHome := t.TempDir()
	inst := &Installer{
		Binary: "/usr/local/bin/pad",
		Home:   tmpHome,
		GOOS:   "linux",
	}
	path, modified, err := inst.Install("claude-desktop")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !modified {
		t.Errorf("first install should modify=true")
	}
	wantPath := filepath.Join(tmpHome, ".config", "Claude", "claude_desktop_config.json")
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected config file written at %s, got: %v", path, err)
	}
}

func TestInstaller_Install_MissingBinaryErrors(t *testing.T) {
	inst := &Installer{Home: t.TempDir(), GOOS: "linux"}
	if _, _, err := inst.Install("cursor"); err == nil {
		t.Errorf("expected error when Binary is empty")
	}
}

func TestInstaller_Uninstall_AfterInstall(t *testing.T) {
	tmpHome := t.TempDir()
	inst := &Installer{Binary: "/p", Home: tmpHome, GOOS: "linux"}
	if _, _, err := inst.Install("cursor"); err != nil {
		t.Fatalf("install: %v", err)
	}
	_, removed, err := inst.Uninstall("cursor")
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !removed {
		t.Errorf("expected removed=true after install")
	}
	// Re-uninstall is idempotent.
	_, removed, err = inst.Uninstall("cursor")
	if err != nil {
		t.Fatalf("second uninstall: %v", err)
	}
	if removed {
		t.Errorf("second uninstall should be no-op")
	}
}

func TestInstaller_Status_ReportsAllAgents(t *testing.T) {
	tmpHome := t.TempDir()
	inst := &Installer{Binary: "/p", Home: tmpHome, GOOS: "linux"}

	// install only cursor; status should reflect mixed state.
	if _, _, err := inst.Install("cursor"); err != nil {
		t.Fatalf("install cursor: %v", err)
	}
	status := inst.Status()
	if len(status) != len(SupportedAgents) {
		t.Fatalf("status returned %d rows, want %d", len(status), len(SupportedAgents))
	}
	for _, row := range status {
		switch row.Name {
		case "cursor":
			if !row.Installed {
				t.Errorf("cursor should be installed")
			}
			if row.Command != "/p" {
				t.Errorf("cursor command = %q, want /p", row.Command)
			}
		default:
			if row.Installed {
				t.Errorf("%s reports installed but we never wrote it", row.Name)
			}
		}
		if row.ConfigPath == "" && row.Error == "" {
			t.Errorf("%s: ConfigPath and Error both empty", row.Name)
		}
	}
}

func TestAddPadEntry_HandlesEmptyAndWhitespaceFile(t *testing.T) {
	dir := t.TempDir()
	cases := []string{"", "   \n\t", "\n"}
	for _, body := range cases {
		path := filepath.Join(dir, "x.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := AddPadEntry(path, "/p"); err != nil {
			t.Errorf("empty body %q should be treated as fresh config; got %v", body, err)
		}
		_ = os.Remove(path)
	}
}

func TestAddPadEntry_RejectsCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers": {`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AddPadEntry(path, "/p"); err == nil {
		t.Errorf("expected error on malformed JSON; we should NOT silently overwrite")
	}
}

// Helpers ------------------------------------------------------------------

func writeConfig(t *testing.T, path string, data map[string]any) {
	t.Helper()
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, b)
	}
	return raw
}

// Quiet the "unused" lint when one or more helpers go unused in a
// future trim — we want to keep them around.
var _ = strings.Contains
