package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// Tests for the PAD_TOKEN environment override (issue #879, layer 1).
//
// Environment isolation sets BOTH HOME and USERPROFILE: CredentialsPath
// resolves through os.UserHomeDir, which reads HOME on Unix and
// USERPROFILE on Windows — setting only HOME (the older convention in
// this package) leaves Windows runs pointed at the developer's real
// ~/.pad.

func setTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// PAD_TOKEN is an explicit per-process override, so it must win exactly
// when set to something non-blank and be invisible otherwise.
func TestEnvToken_SetReturnsTrimmed(t *testing.T) {
	t.Setenv("PAD_TOKEN", "  pad_abc123  ")
	if got := EnvToken(); got != "pad_abc123" {
		t.Errorf("EnvToken() = %q, want %q", got, "pad_abc123")
	}
}

func TestEnvToken_UnsetReturnsEmpty(t *testing.T) {
	t.Setenv("PAD_TOKEN", "")
	if got := EnvToken(); got != "" {
		t.Errorf("EnvToken() = %q, want empty", got)
	}
}

func TestEnvToken_WhitespaceOnlyReturnsEmpty(t *testing.T) {
	t.Setenv("PAD_TOKEN", "   ")
	if got := EnvToken(); got != "" {
		t.Errorf("EnvToken() = %q, want empty", got)
	}
}

func writeStoreForClientTest(t *testing.T) {
	t.Helper()
	home := setTempHome(t)
	dir := filepath.Join(home, ".pad")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `{"version": 2, "credentials": {"http://127.0.0.1:7777": {
		"server_url": "http://127.0.0.1:7777", "token": "padsess_fromstore",
		"user_id": "u-1", "email": "a@b.c", "name": "A"}}}`
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(body), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// PAD_TOKEN must beat the stored credential: the env var is the caller's
// explicit, per-process choice; the file is ambient machine state.
func TestNewClientFromURL_EnvTokenOverridesStore(t *testing.T) {
	writeStoreForClientTest(t)
	t.Setenv("PAD_TOKEN", "pad_envtoken")

	c := NewClientFromURL("http://127.0.0.1:7777")
	if c.authToken != "pad_envtoken" {
		t.Errorf("authToken = %q, want env token to win over store", c.authToken)
	}
}

func TestNewClientFromURL_NoEnvFallsBackToStore(t *testing.T) {
	writeStoreForClientTest(t)
	t.Setenv("PAD_TOKEN", "")

	c := NewClientFromURL("http://127.0.0.1:7777")
	if c.authToken != "padsess_fromstore" {
		t.Errorf("authToken = %q, want stored token when PAD_TOKEN unset", c.authToken)
	}
}

// The override must not touch the store: reads stay side-effect-free
// (the invariant the v2 store documents), so a process running under
// PAD_TOKEN never contends over credentials.json.
func TestNewClientFromURL_EnvTokenLeavesStoreUntouched(t *testing.T) {
	writeStoreForClientTest(t)
	t.Setenv("PAD_TOKEN", "pad_envtoken")

	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".pad", "credentials.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	_ = NewClientFromURL("http://127.0.0.1:7777")
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(before) != string(after) {
		t.Error("credentials.json changed during client construction under PAD_TOKEN")
	}
}
