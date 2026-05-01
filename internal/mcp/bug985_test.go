package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
)

// Tests covering BUG-985 (Pad MCP v0.2 Issue Report). Three regressions
// reported by Claude Desktop after v0.1.0-rc.2:
//
//   - Bug 1: explicit `workspace` parameter silently dropped because
//     the flag is registered as a persistent root flag in cobra and
//     therefore never appears in cmdhelp's per-command flag list,
//     so BuildCLIArgs's flag-iteration loop can't see it.
//   - Bug 2: session default appeared to fail for some tools — same
//     root cause; the post-loop session-fallback works but the
//     explicit-input branch was missing entirely.
//   - Bug 3: list responses returned top-level JSON arrays as
//     structuredContent, which MCP host validators reject ("expected
//     record"). Required wrapping in `{items: [...]}`.

// TestBuildCLIArgs_ExplicitWorkspaceWhenNotInCmdInfoFlags is the
// regression test for BUG-985 bug 1. The cmdhelp document for `item
// list` does NOT include `workspace` in its Flags map (because cobra
// registers --workspace persistently on the root command, not per
// leaf), so the per-flag iteration in BuildCLIArgs has no entry to
// look up. Without the explicit-input check added in this PR, the
// `workspace` value passed in the MCP input is silently dropped and
// the resulting subprocess hits the CWD-fallback path — which is why
// agents got `no_workspace` errors despite passing `workspace=docapp`.
func TestBuildCLIArgs_ExplicitWorkspaceWhenNotInCmdInfoFlags(t *testing.T) {
	// Mirror the real cmdhelp shape: `item list` has 10 flags, none of
	// which is `workspace`.
	cmd := cmdhelp.Command{
		Args: []cmdhelp.Arg{{Name: "collection"}},
		Flags: map[string]cmdhelp.Flag{
			"all":      {Type: "bool"},
			"assign":   {Type: "string"},
			"field":    {Type: "[]string", Repeatable: true},
			"group-by": {Type: "string"},
			"limit":    {Type: "int"},
			"parent":   {Type: "string"},
			"priority": {Type: "string"},
			"role":     {Type: "string"},
			"sort":     {Type: "string"},
			"status":   {Type: "string"},
		},
	}

	t.Run("explicit workspace emitted even when not in cmdInfo.Flags", func(t *testing.T) {
		got, err := BuildCLIArgs(cmd,
			map[string]any{"workspace": "docapp"},
			"", // no session — explicit must still emit
			nil,
		)
		if err != nil {
			t.Fatalf("BuildCLIArgs: %v", err)
		}
		if !containsPair(got, "--workspace", "docapp") {
			t.Errorf("expected --workspace docapp in cliArgs; got %v", got)
		}
	})

	t.Run("explicit workspace overrides session", func(t *testing.T) {
		got, err := BuildCLIArgs(cmd,
			map[string]any{"workspace": "explicit-ws"},
			"session-ws",
			nil,
		)
		if err != nil {
			t.Fatalf("BuildCLIArgs: %v", err)
		}
		// Exactly one --workspace, with the explicit value.
		count, value := countAndValue(got, "--workspace")
		if count != 1 {
			t.Errorf("expected exactly 1 --workspace, got %d in %v", count, got)
		}
		if value != "explicit-ws" {
			t.Errorf("--workspace value = %q, want explicit-ws (explicit must beat session)", value)
		}
	})

	t.Run("session falls through when no explicit", func(t *testing.T) {
		got, err := BuildCLIArgs(cmd, nil, "session-ws", nil)
		if err != nil {
			t.Fatalf("BuildCLIArgs: %v", err)
		}
		if !containsPair(got, "--workspace", "session-ws") {
			t.Errorf("expected session fallback --workspace session-ws; got %v", got)
		}
	})

	t.Run("no explicit, no session — no --workspace emitted", func(t *testing.T) {
		got, err := BuildCLIArgs(cmd, nil, "", nil)
		if err != nil {
			t.Fatalf("BuildCLIArgs: %v", err)
		}
		if count, _ := countAndValue(got, "--workspace"); count != 0 {
			t.Errorf("expected no --workspace flag; got %v", got)
		}
	})
}

// TestPackageJSONResult_WrapsTopLevelArray is the regression test for
// BUG-985 bug 3. Top-level arrays must be wrapped in `{items: [...]}`
// so MCP host validators (Claude Desktop) accept the structuredContent
// shape. Without the wrap they reject with "expected record".
func TestPackageJSONResult_WrapsTopLevelArray(t *testing.T) {
	t.Run("array wrapped under items key", func(t *testing.T) {
		body := `[{"slug":"docapp"},{"slug":"pad-web"}]`
		res := packageJSONResult(body)
		if res.IsError {
			t.Fatalf("unexpected IsError")
		}
		// structuredContent must be a record (map), not an array.
		m, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("structuredContent = %T, want map[string]any", res.StructuredContent)
		}
		items, ok := m["items"].([]any)
		if !ok {
			t.Fatalf("items missing or wrong type: %#v", m)
		}
		if len(items) != 2 {
			t.Errorf("items length = %d, want 2", len(items))
		}
		// Text fallback preserves the ORIGINAL JSON (not the wrapped
		// shape) so clients that don't parse structuredContent keep
		// seeing the raw array. Important: anything pinning against
		// the text body shouldn't break from this fix.
		if textOf(res) != body {
			t.Errorf("text fallback = %q, want original body %q", textOf(res), body)
		}
	})

	t.Run("object passes through unchanged", func(t *testing.T) {
		body := `{"summary":{"total_items":42}}`
		res := packageJSONResult(body)
		m, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("structuredContent = %T, want map[string]any", res.StructuredContent)
		}
		// Top-level keys preserved as-is.
		if _, ok := m["summary"]; !ok {
			t.Errorf("summary key lost; structuredContent=%#v", m)
		}
		if _, hasItemsWrap := m["items"]; hasItemsWrap {
			t.Errorf("object body should NOT be wrapped under items; got %#v", m)
		}
	})

	t.Run("non-JSON falls back to text", func(t *testing.T) {
		body := "hello world"
		res := packageJSONResult(body)
		if res.StructuredContent != nil {
			t.Errorf("non-JSON should not produce structuredContent; got %#v", res.StructuredContent)
		}
		if textOf(res) != body {
			t.Errorf("text fallback = %q, want %q", textOf(res), body)
		}
	})

	t.Run("empty array still wrapped", func(t *testing.T) {
		body := `[]`
		res := packageJSONResult(body)
		m, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("empty array should still produce a wrapped record; got %T", res.StructuredContent)
		}
		items, _ := m["items"].([]any)
		if len(items) != 0 {
			t.Errorf("expected empty items; got %v", items)
		}
	})

	t.Run("malformed JSON falls back to text", func(t *testing.T) {
		body := `[invalid`
		res := packageJSONResult(body)
		if res.StructuredContent != nil {
			t.Errorf("malformed JSON should fall back to text; got %#v", res.StructuredContent)
		}
		if textOf(res) != body {
			t.Errorf("text fallback = %q, want %q", textOf(res), body)
		}
	})

	t.Run("envelope round-trips through JSON", func(t *testing.T) {
		// Belt-and-braces: marshal the wrapped envelope and confirm it
		// produces a valid JSON object so Claude Desktop's JSON-Schema
		// "expected: record" validator passes.
		body := `[{"slug":"docapp"}]`
		res := packageJSONResult(body)
		out, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.HasPrefix(strings.TrimSpace(string(out)), "{") {
			t.Errorf("structuredContent JSON must start with '{' to satisfy MCP record validator; got %s", out)
		}
	})
}

// containsPair returns true when args contains an adjacent (flag, value)
// pair. Used to assert "--workspace docapp" appears in BuildCLIArgs's
// output without depending on overall argument order.
func containsPair(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// countAndValue returns the number of times flag appears followed by a
// value, plus the LAST such value. Used to assert "exactly one
// --workspace and it has the right value" without ordering assumptions.
func countAndValue(args []string, flag string) (count int, lastValue string) {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			count++
			lastValue = args[i+1]
		}
	}
	return count, lastValue
}
