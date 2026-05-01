package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// TestPadMetaTool_RegisteredInCatalog locks the invariant that init()
// in catalog_meta.go appends padMetaTool to Catalog. If the registration
// pattern changes (e.g. someone moves to explicit registration), this
// test forces a deliberate update rather than silently dropping the tool.
func TestPadMetaTool_RegisteredInCatalog(t *testing.T) {
	found := false
	for _, def := range Catalog {
		if def.Name == "pad_meta" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("pad_meta missing from Catalog; init() in catalog_meta.go should appendToCatalog")
	}
}

// TestActionMetaServerInfo_ReturnsServerNameAndVersion verifies the
// minimal {name, version} pair. Empty PadVersion falls back to
// FallbackVersion — same invariant the handshake's serverInfo honours,
// so the agent never sees a blank version string.
func TestActionMetaServerInfo_ReturnsServerNameAndVersion(t *testing.T) {
	t.Run("explicit version", func(t *testing.T) {
		env := ActionEnv{PadVersion: "1.2.3-test"}
		res, err := actionMetaServerInfo(context.Background(), nil, env)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if res.IsError {
			t.Fatalf("unexpected error: %s", textOf(res))
		}
		got := parseStructured(t, res)
		if got["name"] != ServerName {
			t.Errorf("name = %v, want %q", got["name"], ServerName)
		}
		if got["version"] != "1.2.3-test" {
			t.Errorf("version = %v, want 1.2.3-test", got["version"])
		}
	})
	t.Run("empty version falls back", func(t *testing.T) {
		env := ActionEnv{PadVersion: ""}
		res, err := actionMetaServerInfo(context.Background(), nil, env)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		got := parseStructured(t, res)
		if got["version"] != FallbackVersion {
			t.Errorf("version = %v, want fallback %q", got["version"], FallbackVersion)
		}
	})
}

// TestActionMetaVersion_MatchesMetaPayload verifies pad_meta.action:
// version returns the exact same shape as pad://_meta/version. One
// source of truth (BuildMetaPayload) keeps the two surfaces in sync;
// this test is the regression net.
func TestActionMetaVersion_MatchesMetaPayload(t *testing.T) {
	const padVersion = "0.42.0-meta-test"
	env := ActionEnv{PadVersion: padVersion}
	res, err := actionMetaVersion(context.Background(), nil, env)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", textOf(res))
	}
	var got MetaPayload
	if err := json.Unmarshal([]byte(textOf(res)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := BuildMetaPayload(padVersion)
	if got != want {
		t.Errorf("version payload mismatch\n got: %+v\nwant: %+v", got, want)
	}
	// Direct check on the new field — guards against a future build
	// where MetaPayload accidentally drops ToolSurfaceVersion.
	if got.ToolSurfaceVersion != ToolSurfaceVersion {
		t.Errorf("ToolSurfaceVersion = %q, want %q", got.ToolSurfaceVersion, ToolSurfaceVersion)
	}
}

// TestActionMetaToolSurface_DumpsCatalog verifies the action returns a
// summary of every tool currently in Catalog. The shape is the
// canonical source for getpad.dev/docs/mcp generation (PLAN-943
// TASK-957) — locking it now so the docs page can pin against this
// schema.
func TestActionMetaToolSurface_DumpsCatalog(t *testing.T) {
	env := ActionEnv{
		PadVersion: "test",
		Catalog:    Catalog, // production catalog
	}
	res, err := actionMetaToolSurface(context.Background(), nil, env)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", textOf(res))
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(textOf(res)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["tool_surface_version"] != ToolSurfaceVersion {
		t.Errorf("tool_surface_version = %v, want %q", got["tool_surface_version"], ToolSurfaceVersion)
	}
	// rollout_status signals to consumers that the catalog is a strict
	// subset of tools/list during PLAN-969's parallel-rollout phase.
	// "in-progress" while ToolSurfaceVersion stays at 0.1; flips to
	// "complete" when TASK-981 bumps the constant.
	wantStatus := "in-progress"
	if ToolSurfaceVersion != "0.1" {
		wantStatus = "complete"
	}
	if got["rollout_status"] != wantStatus {
		t.Errorf("rollout_status = %v, want %q (ToolSurfaceVersion=%q)",
			got["rollout_status"], wantStatus, ToolSurfaceVersion)
	}
	tools, ok := got["tools"].([]any)
	if !ok {
		t.Fatalf("tools is %T, want []any", got["tools"])
	}
	if len(tools) != len(Catalog) {
		t.Errorf("tools length = %d, want %d (one per Catalog entry)", len(tools), len(Catalog))
	}
	// Cross-check every tool entry mirrors a real Catalog entry.
	byName := map[string]map[string]any{}
	for _, t := range tools {
		if m, ok := t.(map[string]any); ok {
			byName[m["name"].(string)] = m
		}
	}
	for _, def := range Catalog {
		entry, ok := byName[def.Name]
		if !ok {
			t.Errorf("tool %q absent from tool-surface dump", def.Name)
			continue
		}
		if entry["description"] != def.Description {
			t.Errorf("%s.description mismatch", def.Name)
		}
		if entry["workspace"] != def.Schema.Workspace {
			t.Errorf("%s.workspace = %v, want %v", def.Name, entry["workspace"], def.Schema.Workspace)
		}
		actions, ok := entry["actions"].([]any)
		if !ok {
			t.Errorf("%s.actions is %T, want []any", def.Name, entry["actions"])
			continue
		}
		if len(actions) != len(def.Actions) {
			t.Errorf("%s.actions length = %d, want %d", def.Name, len(actions), len(def.Actions))
		}

		// params must always include an `action` entry (synthesized) so
		// the dump is self-contained for docs generation. workspace is
		// added when def.Schema.Workspace=true.
		params, ok := entry["params"].([]any)
		if !ok {
			t.Errorf("%s.params is %T, want []any", def.Name, entry["params"])
			continue
		}
		wantLen := 1 + len(def.Schema.Params)
		if def.Schema.Workspace {
			wantLen++
		}
		if len(params) != wantLen {
			t.Errorf("%s.params length = %d, want %d (1 action + %d schema + %d workspace)",
				def.Name, len(params), wantLen, len(def.Schema.Params), boolToInt(def.Schema.Workspace))
		}
		// First param is always `action` with full enum.
		if len(params) > 0 {
			first, _ := params[0].(map[string]any)
			if first["name"] != "action" {
				t.Errorf("%s.params[0].name = %v, want \"action\"", def.Name, first["name"])
			}
			enum, _ := first["enum"].([]any)
			if len(enum) != len(def.Actions) {
				t.Errorf("%s.params[0].enum length = %d, want %d (one per action)",
					def.Name, len(enum), len(def.Actions))
			}
		}
	}
}

// boolToInt is the trivial converter used in test failure messages
// where formatting "n params + 0 workspace" reads cleaner than a
// branching message.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestPadMetaTool_RoundTripThroughFanOut exercises the full path:
// register → call via the fan-out handler → assert the result. Same
// code path Claude Desktop hits on tools/call.
func TestPadMetaTool_RoundTripThroughFanOut(t *testing.T) {
	env := ActionEnv{PadVersion: "round-trip", Catalog: Catalog}
	handler := makeFanOutHandler(padMetaTool, env)

	cases := []struct {
		action       string
		wantContains string
	}{
		{"server-info", `"name":"pad-mcp"`},
		{"version", `"tool_surface_version":"` + ToolSurfaceVersion + `"`},
		{"tool-surface", `"tool_surface_version":"` + ToolSurfaceVersion + `"`},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			res, err := handler(context.Background(), callToolRequest(map[string]any{
				"action": tc.action,
			}))
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if res.IsError {
				t.Fatalf("expected success, got error: %s", textOf(res))
			}
			if got := textOf(res); !strings.Contains(got, tc.wantContains) {
				t.Errorf("result %q does not contain %q", got, tc.wantContains)
			}
		})
	}
}

// TestPadMetaTool_NoWorkspaceInSchema confirms the schema for a server-
// wide tool (Schema.Workspace=false) does NOT include a workspace
// property. Server-introspection actions don't need a workspace —
// adding one would mislead agents into thinking they should pass it.
func TestPadMetaTool_NoWorkspaceInSchema(t *testing.T) {
	tool := buildToolFromDef(padMetaTool)
	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"workspace"`) {
		t.Errorf("pad_meta schema unexpectedly includes 'workspace' property: %s", raw)
	}
}

// parseStructured pulls the JSON body out of a structured tool result
// and unmarshals it. mcp-go's NewToolResultStructured serializes the
// Go value as text alongside the structured envelope; we read the
// text form because that's what wire-level clients see.
func parseStructured(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	body := textOf(res)
	if body == "" {
		t.Fatalf("expected non-empty structured result body")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("unmarshal structured body: %v\nbody=%s", err, body)
	}
	return out
}

// _ keeps the server import alive in this file even when only
// catalog-level helpers are used — prevents a stray unused-import
// warning if mcp-go's API changes mid-refactor.
var _ = server.NewMCPServer
