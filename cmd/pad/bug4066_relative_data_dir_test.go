package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The relative data-dir warning is once per process, so each case runs in a
// fresh child of this test binary (BUG4066_CHILD selects it).
func TestMain_BUG4066Child(t *testing.T) {
	switch os.Getenv("BUG4066_CHILD") {
	case "":
		t.Skip("child-process entry point")
	case "watch":
		root := newRootCmd()
		root.SetArgs([]string{"watch", "--stream", "--for-session"})
		_ = root.Execute()
	case "mcp":
		root := newRootCmd()
		root.SetArgs([]string{"mcp", "serve"})
		_ = root.Execute()
	case "complete":
		runMainWith([]string{"__complete", "item", "list", "--workspace", ""}...)
	case "complete-collections":
		// completeCollectionNames loads config with no sink of its own.
		runMainWith([]string{"__complete", "item", "list", ""}...)
	case "complete-nodesc":
		runMainWith([]string{"__completeNoDesc", "item", "list", ""}...)
	case "not-complete":
		// A look-alike that is NOT the completion command still warns.
		runMainWith([]string{"item", "list", "__complete"}...)
	case "server":
		root := newRootCmd()
		root.SetArgs([]string{"server", "start", "--host", "127.0.0.1", "--port", os.Getenv("BUG4066_PORT")})
		_ = root.Execute()
	}
}

// runMainWith runs the real main() with these arguments, so a test of what
// main() does before Execute (the completion silence) tests main() itself and
// not a copy of its steps (BUG-4066 r4 item 6). main may os.Exit; this is a
// child process, and the parent reads only its output.
func runMainWith(args ...string) {
	os.Args = append([]string{os.Args[0]}, args...)
	main()
}

func runBUG4066Child(t *testing.T, which string, extra ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run", "^TestMain_BUG4066Child$", "-test.count=1")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "BUG4066_CHILD="+which, "HOME="+t.TempDir(), "PAD_DATA_DIR=data", "PAD_DB_PATH=")
	cmd.Env = append(cmd.Env, extra...)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// BUG-4066 (4b4cb4ff): the warning never reaches the plugin monitor. `pad
// watch --stream --for-session` reads consent through the config readers, so
// without the suppression the line would print there.
func TestBUG4066_MonitorStreamDoesNotWarn(t *testing.T) {
	out := runBUG4066Child(t, "watch")
	if strings.Contains(out, "is relative") {
		t.Fatalf("the monitor stream printed the relative data-dir warning:\n%s", out)
	}
}

// The server warns once through slog, not as a bare stderr line. A held port
// makes `server start` return right after logging is set up.
func TestBUG4066_ServerWarnsThroughSlog(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	out := runBUG4066Child(t, "server", "BUG4066_PORT="+port, "PAD_LOG_FORMAT=text")
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "PAD_DATA_DIR is relative (data)") {
		t.Fatalf("no slog WARN for the relative data dir:\n%s", out)
	}
	if strings.Contains(out, "Warning: PAD_DATA_DIR is relative") {
		t.Fatalf("the server printed the bare stderr line instead of logging it:\n%s", out)
	}
	if n := strings.Count(out, "is relative"); n != 1 {
		t.Fatalf("want one warning, got %d:\n%s", n, out)
	}
}

// `pad mcp serve` never loads config, so it checks the data dir itself and
// warns once on its own stderr (BUG-4066 r1, Q1). stdin is closed, so the
// server sees EOF and returns.
func TestBUG4066_MCPServeWarnsOnce(t *testing.T) {
	out := runBUG4066Child(t, "mcp")
	if n := strings.Count(out, "PAD_DATA_DIR is relative (data)"); n != 1 {
		t.Fatalf("want exactly one warning from mcp serve, got %d:\n%s", n, out)
	}
}

// Tab completion prints no warning in the middle of the user's line: the
// --workspace completion, and (at the root, in main) every other completion
// function, including the ones that load config themselves.
func TestBUG4066_CompletionDoesNotWarn(t *testing.T) {
	for _, which := range []string{"complete", "complete-collections", "complete-nodesc"} {
		t.Run(which, func(t *testing.T) {
			out := runBUG4066Child(t, which)
			if strings.Contains(out, "is relative") {
				t.Fatalf("shell completion printed the relative data-dir warning:\n%s", out)
			}
		})
	}
}

// The silence is NARROW: a command that merely mentions __complete, or any
// other, still warns.
func TestBUG4066_OnlyTheCompletionCommandIsSilenced(t *testing.T) {
	out := runBUG4066Child(t, "not-complete")
	if !strings.Contains(out, "PAD_DATA_DIR is relative (data)") {
		t.Fatalf("a non-completion command did not warn:\n%s", out)
	}
}
