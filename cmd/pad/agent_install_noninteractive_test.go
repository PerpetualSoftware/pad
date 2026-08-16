package main

// Test for BUG-2593: installInteractive (pad agent install) must not print
// the dangling "(Y/n): " prompt in non-interactive contexts — the same
// IsTerminal()→canPromptForConfig() swap offerSkillInstall got for
// BUG-2577 (PR #1111), whose test this mirrors.
//
// Scope note (same limitation as #1111's test): with a closed-stdin pipe,
// the old stdin-only guard and the new stdin+stdout guard agree, so this
// test pins the non-interactive no-prompt behavior but cannot discriminate
// the pty-backed-stdin case that motivates the swap — canPromptForConfig()
// reads the real process fds and the repo carries no direct pty dep. The
// discriminating case is covered by the live verification recorded on
// BUG-2593's trail: stdin on an undriven pty + stdout on a pipe, where the
// pre-fix binary prints the prompt and hangs at readChoice while the fixed
// binary installs silently and exits.

import (
	"os"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

func TestAgentInstallNonInteractiveNoPrompt(t *testing.T) {
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

	instErr := installInteractive()

	pw.Close()
	os.Stdout = origStdout
	output := <-done

	if instErr != nil {
		t.Fatalf("installInteractive: %v", instErr)
	}
	if strings.Contains(output, "(Y/n)") {
		t.Errorf("non-interactive pad agent install printed a dangling prompt: %q", output)
	}

	// Claude is always in the detected set (installInteractive's hardcoded
	// fallback), so the skill should have been installed silently.
	path := cli.ToolSkillPath(cli.SupportedTools[0])
	if path == "" {
		t.Fatal("expected a skill path for the Claude tool")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected /pad skill to be installed at %s, got: %v", path, err)
	}
}
