package main

// Test for BUG-2592: `pad init`'s Step 4 (configured instance, not
// authenticated) must fail fast in non-interactive contexts instead of
// driving doBrowserLogin's poll wait — the same canPromptForConfig() gate
// Step 3 already has (init.go:205-206) and cmd_workspace.go got for
// BUG-2538 (PR #1111). Reuses that PR's harness (headlessTestServer,
// localHeadlessCfg, withClosedStdin).

import (
	"io"
	"strings"
	"testing"
)

// TestInitNonTTYNotAuthenticated verifies the BUG-2592 gate: a
// non-interactive `pad init` against an already-set-up but unauthenticated
// local instance errors immediately with the corrected remedy — piped
// `pad auth login --interactive` — rather than entering doBrowserLogin.
// The message assertions discriminate against the ungated path: a
// doBrowserLogin failure surfaces as "login: ..." and never mentions
// `pad auth login`.
func TestInitNonTTYNotAuthenticated(t *testing.T) {
	isolateHome(t)
	withClosedStdin(t)
	srv := headlessTestServer(t, false /* setup_required */, false)
	defer srv.Close()
	localHeadlessCfg(t, srv)

	root := newRootCmd()
	root.SetArgs([]string{"init"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for non-interactive unauthenticated init, got nil")
	}
	if !strings.Contains(err.Error(), "not authenticated") {
		t.Errorf("error %q should mention not being authenticated", err.Error())
	}
	if !strings.Contains(err.Error(), "pad auth login --interactive") {
		t.Errorf("error %q should point at piped 'pad auth login --interactive' for scripted use", err.Error())
	}
	if strings.Contains(err.Error(), "--email") {
		t.Errorf("error %q must not suggest pad init's --email/--name/--password — those only fire when SetupRequired (the wrong-remedy shape codex r1/r2 flagged on BUG-2538)", err.Error())
	}
}
