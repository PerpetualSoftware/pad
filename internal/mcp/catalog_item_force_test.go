package mcp

// IDEA-1494 — MCP-level coverage for the `force` parameter on
// pad_item.action: update / bulk-update. Confirms the catalog's force
// param translates into the `--force` CLI flag on the dispatched
// command so the open-children guard's escape hatch is identical on
// stdio MCP (ExecDispatcher) and the legacy CLI surface.

import (
	"context"
	"strings"
	"testing"
)

func TestPadItemUpdate_ForcePassesThroughToCLI(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}

	handler, ok := padItemTool.Actions["update"]
	if !ok {
		t.Fatal("pad_item.action: update is not registered")
	}

	if _, err := handler(context.Background(), map[string]any{
		"ref":    "PLAN-5",
		"status": "completed",
		"force":  true,
	}, env); err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	if !equalStrings(disp.gotPath, []string{"item", "update"}) {
		t.Fatalf("cmdPath = %v, want [item update]", disp.gotPath)
	}
	joined := strings.Join(disp.gotArgs, " ")
	if !strings.Contains(joined, "--force") {
		t.Errorf("cliArgs should contain '--force' when force=true; got %q", joined)
	}
	if !strings.Contains(joined, "--status completed") {
		t.Errorf("cliArgs should preserve other flags; got %q", joined)
	}
}

func TestPadItemUpdate_ForceFalse_Omitted(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}

	handler := padItemTool.Actions["update"]
	if _, err := handler(context.Background(), map[string]any{
		"ref":    "PLAN-5",
		"status": "completed",
		"force":  false,
	}, env); err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	joined := strings.Join(disp.gotArgs, " ")
	if strings.Contains(joined, "--force") {
		t.Errorf("cliArgs should NOT contain '--force' when force=false; got %q", joined)
	}
}

func TestPadItemBulkUpdate_ForcePassesThroughToCLI(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}

	handler := padItemTool.Actions["bulk-update"]
	if _, err := handler(context.Background(), map[string]any{
		"refs":   []any{"TASK-1", "TASK-2"},
		"status": "done",
		"force":  true,
	}, env); err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	joined := strings.Join(disp.gotArgs, " ")
	if !strings.Contains(joined, "--force") {
		t.Errorf("cliArgs should contain '--force' for bulk-update force=true; got %q", joined)
	}
}
