package mcp

import (
	"context"
	"strings"
	"testing"
)

// BUG-4026: `pad item backlinks --format json` without `--limit` now pages to
// completion, so MCP's `pad_item` `backlinks` action must inject a default
// limit (50) and clamp oversized limits (300), keeping agents bounded on both
// stdio and HTTP transports.

func backlinksArgs(t *testing.T, input map[string]any) string {
	t.Helper()
	disp := &fakeDispatcher{}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
	handler := padItemTool.Actions["backlinks"]
	if handler == nil {
		t.Fatalf("pad_item.backlinks action not registered")
	}
	res, err := handler(context.Background(), input, env)
	if err != nil {
		t.Fatalf("pad_item.backlinks error: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("error result: %s", textOf(res))
	}
	return strings.Join(disp.gotArgs, " ")
}

func TestBUG4026_PadItemBacklinks_AppliesDefaultLimit(t *testing.T) {
	joined := backlinksArgs(t, map[string]any{"ref": "TASK-5"})
	if !strings.Contains(joined, "--limit 50") {
		t.Errorf("cliArgs %q should carry injected default \"--limit 50\"", joined)
	}
}

func TestBUG4026_PadItemBacklinks_ClampsOversizedLimit(t *testing.T) {
	joined := backlinksArgs(t, map[string]any{"ref": "TASK-5", "limit": float64(1000)})
	if !strings.Contains(joined, "--limit 300") {
		t.Errorf("cliArgs %q should clamp to \"--limit 300\"", joined)
	}
}

func TestBUG4026_PadItemBacklinks_HonorsInRangeLimitAndOffset(t *testing.T) {
	joined := backlinksArgs(t, map[string]any{"ref": "TASK-5", "limit": float64(12), "offset": float64(20)})
	if !strings.Contains(joined, "--limit 12") || !strings.Contains(joined, "--offset 20") {
		t.Errorf("cliArgs %q should keep --limit 12 and --offset 20", joined)
	}
}
