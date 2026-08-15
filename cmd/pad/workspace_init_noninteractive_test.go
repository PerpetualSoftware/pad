package main

// Tests for BUG-2538 and BUG-2577: `pad workspace init` (and the
// offerSkillInstall prompt it shares with `pad workspace link`) must fail
// fast and print no dangling prompt text in non-interactive contexts,
// mirroring `pad init`'s existing canPromptForConfig() gate (init.go:205-206).

import (
	"io"
	"net"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// localHeadlessCfg points a Local-mode config at srv via env vars, so
// EnsureServer's isServerHealthy(host, port) check finds the fake server
// live and skips spawning a real `pad server start` subprocess.
// Local mode (rather than Remote) matters here: cmd_workspace.go's
// initCmd only reaches the BUG-2538 canPromptForConfig() gate for local
// instances — Remote/Cloud instances return earlier via printSetupRequiredHint.
func localHeadlessCfg(t *testing.T, srv *httptest.Server) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split srv addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse srv port: %v", err)
	}
	t.Setenv("PAD_MODE", "local")
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_HOST", host)
	t.Setenv("PAD_PORT", strconv.Itoa(port))
}

// withClosedStdin replaces os.Stdin with a pipe whose write end is already
// closed, so term.IsTerminal(stdin) is false and any read hits EOF
// immediately rather than blocking. Mirrors setup_headless_test.go's
// TestHeadlessSetupNonTTYWithoutFlags.
func withClosedStdin(t *testing.T) {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	pw.Close()
	orig := os.Stdin
	os.Stdin = pr
	t.Cleanup(func() {
		os.Stdin = orig
		pr.Close()
	})
}

// TestWorkspaceInitNonTTYSetupRequired verifies BUG-2538's fast-fail path:
// a non-interactive `pad workspace init` against a configured-but-not-set-up
// local instance errors immediately with an actionable hint instead of
// entering runBrowserSetup's wait.
func TestWorkspaceInitNonTTYSetupRequired(t *testing.T) {
	isolateHome(t)
	withClosedStdin(t)
	srv := headlessTestServer(t, true /* setup_required */, false)
	defer srv.Close()
	localHeadlessCfg(t, srv)

	root := newRootCmd()
	root.SetArgs([]string{"workspace", "init", "testws"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for non-interactive setup-required init, got nil")
	}
	if !strings.Contains(err.Error(), "not been initialized yet") {
		t.Errorf("error %q should mention the instance is not initialized", err.Error())
	}
	if !strings.Contains(err.Error(), "pad auth setup") {
		t.Errorf("error %q should point at 'pad auth setup' for non-interactive bootstrap", err.Error())
	}
}

// TestWorkspaceInitNonTTYNotAuthenticated verifies BUG-2538's second gate:
// a non-interactive `pad workspace init` against an already-set-up but
// unauthenticated instance errors immediately instead of entering
// doBrowserLogin's wait.
func TestWorkspaceInitNonTTYNotAuthenticated(t *testing.T) {
	isolateHome(t)
	withClosedStdin(t)
	srv := headlessTestServer(t, false /* setup_required */, false)
	defer srv.Close()
	localHeadlessCfg(t, srv)

	root := newRootCmd()
	root.SetArgs([]string{"workspace", "init", "testws"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for non-interactive unauthenticated init, got nil")
	}
	if !strings.Contains(err.Error(), "not authenticated") {
		t.Errorf("error %q should mention not being authenticated", err.Error())
	}
	if !strings.Contains(err.Error(), "pad auth login") {
		t.Errorf("error %q should point at 'pad auth login'", err.Error())
	}
}

// TestOfferSkillInstallNonInteractiveNoPrompt verifies BUG-2577: when
// canPromptForConfig() is false, offerSkillInstall must not print the
// dangling "(Y/n): " prompt text — only a plain statement of what it did —
// while still installing the skill for detected tools (behavior unchanged,
// only the printed text differs).
func TestOfferSkillInstallNonInteractiveNoPrompt(t *testing.T) {
	isolateHome(t)
	withClosedStdin(t)

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = pw
	t.Cleanup(func() { os.Stdout = origStdout })

	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 4096)
		for {
			n, err := pr.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if err != nil {
				break
			}
		}
		done <- string(buf)
	}()

	offerSkillInstall()

	pw.Close()
	os.Stdout = origStdout
	output := <-done

	if strings.Contains(output, "(Y/n)") {
		t.Errorf("non-interactive offerSkillInstall printed a dangling prompt: %q", output)
	}

	// Claude is always in the detected set (offerSkillInstall's hardcoded
	// fallback), so the skill should have been installed silently.
	path := cli.ToolSkillPath(cli.SupportedTools[0])
	if path == "" {
		t.Fatal("expected a skill path for the Claude tool")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected /pad skill to be installed at %s, got: %v", path, err)
	}
}
