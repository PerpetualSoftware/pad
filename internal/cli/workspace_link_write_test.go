package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

// TASK-3612: WriteWorkspaceLink updates an existing .pad.toml instead of
// replacing it, so a relink keeps agent_name, the [push] consent table and
// any key a newer pad wrote.

func readPadTomlMap(t *testing.T, dir string) map[string]any {
	t.Helper()
	doc := map[string]any{}
	if _, err := toml.DecodeFile(filepath.Join(dir, ".pad.toml"), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return doc
}

func TestWriteWorkspaceLinkKeepsTheFilesOtherKeys(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := "workspace = \"old\"\nurl = \"https://pad.example.com\"\nagent_name = \"codex\"\nfuture_key = 7\n\n[push]\n  auto_arm = true\n"
	if err := os.WriteFile(filepath.Join(dir, ".pad.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteWorkspaceLink(dir, "new", "https://app.getpad.dev"); err != nil {
		t.Fatal(err)
	}
	doc := readPadTomlMap(t, dir)
	if doc["workspace"] != "new" || doc["url"] != "https://app.getpad.dev" {
		t.Errorf("link not updated: %v", doc)
	}
	if doc["agent_name"] != "codex" {
		t.Errorf("agent_name = %v, want codex", doc["agent_name"])
	}
	if doc["future_key"] != int64(7) {
		t.Errorf("future_key = %#v, want 7", doc["future_key"])
	}
	push, _ := doc["push"].(map[string]any)
	if push == nil || push["auto_arm"] != true {
		t.Errorf("[push] = %v, want auto_arm kept", doc["push"])
	}

	// And the typed reader still sees what it always did.
	var typed PadToml
	if _, err := toml.DecodeFile(filepath.Join(dir, ".pad.toml"), &typed); err != nil {
		t.Fatal(err)
	}
	if typed.Workspace != "new" || typed.AgentName != "codex" || !typed.PadTomlAutoArm() {
		t.Errorf("typed read = %+v", typed)
	}
}

func TestWriteWorkspaceLinkDropsTheURLForALocalLink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".pad.toml"), []byte("workspace = \"a\"\nurl = \"https://pad.example.com\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteWorkspaceLink(dir, "a", ""); err != nil {
		t.Fatal(err)
	}
	if doc := readPadTomlMap(t, dir); doc["url"] != nil {
		t.Errorf("url = %v, want it removed for a local link", doc["url"])
	}
}

func TestWriteWorkspaceLinkCreatesAMissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := WriteWorkspaceLink(dir, "fresh", ""); err != nil {
		t.Fatal(err)
	}
	doc := readPadTomlMap(t, dir)
	if doc["workspace"] != "fresh" || len(doc) != 1 {
		t.Errorf("new file = %v, want only workspace", doc)
	}
}

func TestWriteWorkspaceLinkRefusesAFileItCannotRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	broken := []byte("workspace = \"a\"\nthis is not toml\n")
	path := filepath.Join(dir, ".pad.toml")
	if err := os.WriteFile(path, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteWorkspaceLink(dir, "b", ""); err == nil {
		t.Fatal("wrote over a .pad.toml it could not parse")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(broken) {
		t.Errorf("the unreadable file was changed:\n%s", got)
	}
}
