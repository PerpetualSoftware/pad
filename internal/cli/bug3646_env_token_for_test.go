package cli

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
)

// BUG-3646: EnvTokenFor withholds PAD_TOKEN from a server only .pad.toml
// chose, warns once, and is unchanged everywhere else.
func TestBUG3646_EnvTokenFor(t *testing.T) {
	t.Setenv("PAD_TOKEN", "pad_env")
	var warn bytes.Buffer
	envTokenWarnOut, envTokenWarnOnce = &warn, sync.Once{}
	t.Cleanup(func() {
		NotePadTomlURLOverride("", "")
		envTokenWarnOut, envTokenWarnOnce = os.Stderr, sync.Once{}
	})

	NotePadTomlURLOverride("", "")
	if got := EnvTokenFor("https://evil.example"); got != "pad_env" {
		t.Errorf("no override: got %q, want the token", got)
	}

	NotePadTomlURLOverride("https://app.getpad.dev", "https://evil.example")
	if got := EnvTokenFor("https://evil.example/"); got != "" {
		t.Errorf("override destination: got %q, want it withheld", got)
	}
	_ = EnvTokenFor("https://evil.example")
	if n := strings.Count(warn.String(), "PAD_TOKEN not sent"); n != 1 {
		t.Errorf("warning printed %d times, want once: %q", n, warn.String())
	}
	if !strings.Contains(warn.String(), "PAD_URL=https://evil.example") {
		t.Errorf("warning does not name the opt-in: %q", warn.String())
	}
	if got := EnvTokenFor("https://app.getpad.dev"); got != "pad_env" {
		t.Errorf("chosen server: got %q, want the token", got)
	}

	NotePadTomlURLOverride("https://app.getpad.dev/", "https://app.getpad.dev")
	if got := EnvTokenFor("https://app.getpad.dev"); got != "pad_env" {
		t.Errorf("override naming the chosen server: got %q, want the token", got)
	}
}
