package mcp

// BUG-2020 — MCP-level coverage for the draft-playbook gate escape
// hatch on pad_playbook.action: run. The ExecDispatcher path can't send
// a structured JSON body, so the allow_draft boolean must be forwarded
// to the CLI as the `allow-draft` bareword token (which the server
// strips out of raw_args before strict parsing and treats as the
// escape hatch).

import (
	"context"
	"strings"
	"testing"
)

func TestPadPlaybookRun_AllowDraftForwardsBareword(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}

	handler, ok := padPlaybookTool.Actions["run"]
	if !ok {
		t.Fatal("pad_playbook.action: run is not registered")
	}

	if _, err := handler(context.Background(), map[string]any{
		"ref":         "wip",
		"allow_draft": true,
	}, env); err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	if !equalStrings(disp.gotPath, []string{"playbook", "run"}) {
		t.Fatalf("cmdPath = %v, want [playbook run]", disp.gotPath)
	}
	joined := strings.Join(disp.gotArgs, " ")
	if !strings.Contains(joined, "allow-draft") {
		t.Errorf("cliArgs should contain the 'allow-draft' bareword when allow_draft=true; got %q", joined)
	}
}

func TestPadPlaybookRun_AllowDraftFalse_Omitted(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}

	handler := padPlaybookTool.Actions["run"]
	if _, err := handler(context.Background(), map[string]any{
		"ref":         "wip",
		"allow_draft": false,
	}, env); err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	joined := strings.Join(disp.gotArgs, " ")
	if strings.Contains(joined, "allow-draft") {
		t.Errorf("cliArgs should NOT contain 'allow-draft' when allow_draft=false; got %q", joined)
	}
}
