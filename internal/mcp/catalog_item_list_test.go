package mcp

import (
	"context"
	"strings"
	"testing"
)

// TestPadItemList_InjectsDefaultLimit verifies action=list adds a
// default `--limit` (mcpItemListDefaultLimit) when the agent didn't
// ask for one, so a bare agent list can't dump the whole workspace
// into context. TASK-2000.
func TestPadItemList_InjectsDefaultLimit(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}
	res, err := actionItemList(context.Background(), map[string]any{
		"collection": "tasks",
	}, env)
	if err != nil {
		t.Fatalf("actionItemList error: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("error result: %s", textOf(res))
	}
	if !equalStrings(disp.gotPath, []string{"item", "list"}) {
		t.Errorf("cmdPath = %v, want [item list]", disp.gotPath)
	}
	joined := strings.Join(disp.gotArgs, " ")
	wantLimit := "--limit 50"
	if !strings.Contains(joined, wantLimit) {
		t.Errorf("cliArgs %q should inject the default %q", joined, wantLimit)
	}
}

// TestPadItemList_ClampsOversizedLimit verifies an agent-supplied limit
// above the hard max is clamped down to mcpItemListMaxLimit rather than
// forwarded verbatim. TASK-2000.
func TestPadItemList_ClampsOversizedLimit(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}
	// JSON decoders deliver numbers as float64 — mirror that here.
	_, err := actionItemList(context.Background(), map[string]any{
		"collection": "tasks",
		"limit":      float64(999999),
	}, env)
	if err != nil {
		t.Fatalf("actionItemList error: %v", err)
	}
	joined := strings.Join(disp.gotArgs, " ")
	if !strings.Contains(joined, "--limit 300") {
		t.Errorf("cliArgs %q should clamp oversized limit to 300", joined)
	}
	if strings.Contains(joined, "999999") {
		t.Errorf("cliArgs %q must not forward the oversized limit verbatim", joined)
	}
}

// TestPadItemList_RespectsInRangeLimit verifies a reasonable
// agent-supplied limit passes through untouched. TASK-2000.
func TestPadItemList_RespectsInRangeLimit(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}
	_, err := actionItemList(context.Background(), map[string]any{
		"collection": "tasks",
		"limit":      float64(25),
	}, env)
	if err != nil {
		t.Fatalf("actionItemList error: %v", err)
	}
	joined := strings.Join(disp.gotArgs, " ")
	if !strings.Contains(joined, "--limit 25") {
		t.Errorf("cliArgs %q should honor an in-range limit of 25", joined)
	}
}

func TestPadItemList_RejectsParentWithUnparented(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
	res, err := actionItemList(context.Background(), map[string]any{
		"parent": "PLAN-3", "unparented": true,
	}, env)
	if err != nil {
		t.Fatalf("actionItemList error: %v", err)
	}
	if res == nil || !res.IsError || !strings.Contains(textOf(res), "mutually exclusive") {
		t.Fatalf("expected structured mutual-exclusion error, got %#v", res)
	}
	if len(disp.gotPath) != 0 {
		t.Fatalf("invalid input reached dispatcher: %v", disp.gotPath)
	}
}

func TestPadItemList_ForwardsUnparentedToLocalCLI(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
	res, err := actionItemList(context.Background(), map[string]any{"unparented": true}, env)
	if err != nil {
		t.Fatalf("actionItemList error: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("error result: %s", textOf(res))
	}
	joined := strings.Join(disp.gotArgs, " ")
	if !strings.Contains(joined, "--unparented") {
		t.Fatalf("local CLI args %q omit --unparented", joined)
	}
}
