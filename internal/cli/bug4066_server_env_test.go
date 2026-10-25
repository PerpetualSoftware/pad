package cli

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
)

// An MCP tool-call child carries the warning-silencing variable, and it is
// often the process that auto-starts the server. The server it starts must
// not inherit it, or that long-lived server never logs the relative data-dir
// warning; every other variable carries through (BUG-4066 r3 item 2).
func TestBUG4066_AutoStartedServerDropsOnlyTheSilencingVar(t *testing.T) {
	t.Setenv(config.InternalNoDataDirWarningEnv, "1")
	t.Setenv("PAD_DATA_DIR", "data")
	t.Setenv("BUG4066_KEEP", "kept")

	cmd := serverStartCommand("/bin/pad")
	if got := cmd.Args; !slices.Equal(got, []string{"/bin/pad", "server", "start"}) {
		t.Fatalf("args = %v, want [/bin/pad server start]", got)
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, config.InternalNoDataDirWarningEnv+"=") {
			t.Fatalf("the auto-started server inherits %s", kv)
		}
	}
	for _, want := range []string{"PAD_DATA_DIR=data", "BUG4066_KEEP=kept"} {
		if !slices.Contains(cmd.Env, want) {
			t.Errorf("the auto-started server lost %s", want)
		}
	}
	if len(cmd.Env) != len(os.Environ())-1 {
		t.Errorf("env has %d entries, want this process's %d minus the one dropped", len(cmd.Env), len(os.Environ()))
	}
}

// The binding (CONVE-19): the spawn goes through serverStartCommand, and no
// other non-test file in this package builds a `server start` command.
func TestBUG4066_TheOnlyServerSpawnUsesTheFilteredEnv(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func startBackgroundServer(")
	if start < 0 {
		t.Fatal("startBackgroundServer not found")
	}
	fn := body[start:]
	if end := strings.Index(fn[1:], "\nfunc "); end >= 0 {
		fn = fn[:end+1]
	}
	if !strings.Contains(fn, "serverStartCommand(") {
		t.Fatal("startBackgroundServer does not build its command with serverStartCommand")
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		n += strings.Count(string(b), `"server", "start"`)
	}
	if n != 1 {
		t.Fatalf("found %d `server start` commands in internal/cli, want exactly the one in serverStartCommand", n)
	}
}
