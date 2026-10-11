package mcp

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// TASK-3585 at the MCP doors: op_key replays over the remote transport, a
// reused key comes back as its own code (not validation_failed), and the
// hints match the CLI's.
func TestTASK3585_MCPOpKey(t *testing.T) {
	f := newParentFixture(t)
	ws, ref := f.workspace.Slug, f.child.Slug
	f.dispatch(t, []string{"item", "update"}, map[string]any{"workspace": ws, "ref": ref, "content": "the fox ran\n", "replace": true})
	call := func(extra map[string]any) *CallToolResult {
		in := map[string]any{"workspace": ws, "ref": ref, "edits": []any{map[string]any{"old": "fox", "new": "fox, then a dog,"}}, "op_key": "mcp-k"}
		for k, v := range extra {
			in[k] = v
		}
		return f.dispatchResult2(t, []string{"item", "patch"}, in)
	}
	first := call(nil)
	second := call(nil)
	m1, _ := first.StructuredContent.(map[string]any)
	m2, _ := second.StructuredContent.(map[string]any)
	if first.IsError || second.IsError || m1["patch_id"] != m2["patch_id"] {
		t.Fatalf("replay: %s / %s", textOf(first), textOf(second))
	}
	got, _ := f.store.GetItem(f.child.ID)
	if got.Content != "the fox, then a dog, ran\n" {
		t.Fatalf("the retry applied again: %q", got.Content)
	}
	reused := call(map[string]any{"edits": []any{map[string]any{"old": "ran", "new": "RAN"}}})
	env, ok := reused.StructuredContent.(ErrorEnvelope)
	if !reused.IsError || !ok || env.Error.Code != ErrIdempotencyKeyReused || env.Error.Hint != IdempotencyKeyReusedHint {
		t.Fatalf("reused key: %s", textOf(reused))
	}
}

func TestTASK3585_CLIAndMCPAgreeOnHints(t *testing.T) {
	for _, p := range [][2]string{
		{cli.PatchOutcomeUnknownHint, PatchOutcomeUnknownHint},
		{cli.IdempotencyInFlightHint, IdempotencyInFlightHint},
		{cli.IdempotencyKeyReusedHint, IdempotencyKeyReusedHint},
	} {
		if p[0] != p[1] {
			t.Errorf("hints differ:\n cli %q\n mcp %q", p[0], p[1])
		}
	}
	for _, c := range []string{cli.PatchOutcomeUnknownCode, cli.IdempotencyInFlightCode, cli.IdempotencyKeyReusedCode} {
		if _, ok := allowedStructuredErrorCodes[c]; !ok {
			t.Errorf("%s is not allow-listed", c)
		}
	}
}
