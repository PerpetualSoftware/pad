package mcp

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestPadItemExport_ForcesStdout verifies action=export injects the
// CLI's stdout sink (`--output -`) so the artifact bytes come back as
// the tool result rather than being written to a file the MCP host
// can't see. Regression guard for the "MCP export must not write a
// file" contract.
func TestPadItemExport_ForcesStdout(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}
	res, err := actionItemExport(context.Background(), map[string]any{
		"ref": "PLAYB-3",
	}, env)
	if err != nil {
		t.Fatalf("actionItemExport error: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("error result: %s", textOf(res))
	}
	if !equalStrings(disp.gotPath, []string{"item", "export"}) {
		t.Errorf("cmdPath = %v, want [item export]", disp.gotPath)
	}
	joined := strings.Join(disp.gotArgs, " ")
	if !strings.Contains(joined, "PLAYB-3") {
		t.Errorf("cliArgs %q should carry the ref positional", joined)
	}
	if !strings.Contains(joined, "--output -") {
		t.Errorf("cliArgs %q should force stdout via --output -", joined)
	}
}

// TestPadItemExport_OverridesAgentOutput confirms an agent-supplied
// `output` (a local path the MCP host can't see) is overridden with
// `-` so export always streams to the result.
func TestPadItemExport_OverridesAgentOutput(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}
	_, err := actionItemExport(context.Background(), map[string]any{
		"ref":    "CONVE-7",
		"output": "/tmp/agent-chosen.pad.md",
	}, env)
	if err != nil {
		t.Fatalf("actionItemExport error: %v", err)
	}
	joined := strings.Join(disp.gotArgs, " ")
	if strings.Contains(joined, "/tmp/agent-chosen.pad.md") {
		t.Errorf("cliArgs %q must not carry the agent's local output path", joined)
	}
	if !strings.Contains(joined, "--output -") {
		t.Errorf("cliArgs %q should force stdout via --output -", joined)
	}
}

// TestPadItemExport_MissingRef returns a structured error rather than
// dispatching with an empty ref.
func TestPadItemExport_MissingRef(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}
	res, err := actionItemExport(context.Background(), map[string]any{}, env)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError, got %s", textOf(res))
	}
	if !strings.Contains(textOf(res), "ref is required") {
		t.Errorf("message %q should mention ref requirement", textOf(res))
	}
	if len(disp.gotPath) > 0 {
		t.Errorf("dispatcher should not have been called; got %v", disp.gotPath)
	}
}

// TestPadItemImport_WritesTempFileAndDispatches verifies action=import
// spills the `artifact` body to a temp file and dispatches
// `item import <tmpfile>` with the body intact. Stdin isn't piped by
// ExecDispatcher, so the temp-file route is the contract.
func TestPadItemImport_WritesTempFileAndDispatches(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}
	const artifact = "---\ncollection: playbooks\ntitle: Ship\n---\n\n# Ship\n"
	res, err := actionItemImport(context.Background(), map[string]any{
		"artifact": artifact,
	}, env)
	if err != nil {
		t.Fatalf("actionItemImport error: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("error result: %s", textOf(res))
	}
	if !equalStrings(disp.gotPath, []string{"item", "import"}) {
		t.Errorf("cmdPath = %v, want [item import]", disp.gotPath)
	}
	if len(disp.gotArgs) == 0 {
		t.Fatalf("cliArgs empty; want a tempfile positional")
	}
	tmpPath := disp.gotArgs[0]
	if !strings.HasSuffix(tmpPath, ".pad.md") {
		t.Errorf("positional %q should be a .pad.md temp path", tmpPath)
	}
	// The artifact key must NOT leak onto the CLI as a flag/positional.
	joined := strings.Join(disp.gotArgs, " ")
	if strings.Contains(joined, "--artifact") {
		t.Errorf("cliArgs %q should not carry an --artifact flag", joined)
	}
}

// TestPadItemImport_TempFileCleanedUp confirms the temp file is removed
// after dispatch returns — no artifact litter left on disk.
func TestPadItemImport_TempFileCleanedUp(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}
	if _, err := actionItemImport(context.Background(), map[string]any{
		"artifact": "---\ncollection: conventions\ntitle: X\n---\nbody\n",
	}, env); err != nil {
		t.Fatalf("actionItemImport error: %v", err)
	}
	if len(disp.gotArgs) == 0 {
		t.Fatalf("no positional captured")
	}
	tmpPath := disp.gotArgs[0]
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("temp file %q should be removed after dispatch (stat err=%v)", tmpPath, err)
	}
}

// TestPadItemImport_MissingArtifact returns a structured error and
// doesn't dispatch when no artifact body is supplied.
func TestPadItemImport_MissingArtifact(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}
	res, err := actionItemImport(context.Background(), map[string]any{}, env)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError, got %s", textOf(res))
	}
	if !strings.Contains(textOf(res), "artifact is required") {
		t.Errorf("message %q should mention artifact requirement", textOf(res))
	}
	if len(disp.gotPath) > 0 {
		t.Errorf("dispatcher should not have been called; got %v", disp.gotPath)
	}
}
