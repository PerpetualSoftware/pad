package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
)

// TestRegisterCatalog_RegistersAllCatalogTools is the smoke test —
// every ToolDef in Catalog gets registered exactly once.
func TestRegisterCatalog_RegistersAllCatalogTools(t *testing.T) {
	srv := server.NewMCPServer("t", "1", server.WithToolCapabilities(true))
	count, err := RegisterCatalog(srv, CatalogOptions{
		Doc:        fixtureDoc(),
		Workspace:  NewWorkspaceState(""),
		Dispatcher: &fakeDispatcher{},
		PadVersion: "test",
	})
	if err != nil {
		t.Fatalf("RegisterCatalog: %v", err)
	}
	if count != len(Catalog) {
		t.Errorf("registered %d tools, want %d (one per Catalog entry)", count, len(Catalog))
	}
	if count == 0 {
		t.Errorf("Catalog is empty — at least pad_meta should be present after init()")
	}
}

// TestRegisterCatalog_RequiresOptions verifies the validation surface.
// Each missing required field produces a clear error rather than a
// nil-deref later in the action handler.
func TestRegisterCatalog_RequiresOptions(t *testing.T) {
	cases := []struct {
		name string
		opts CatalogOptions
		want string
	}{
		{"missing Doc", CatalogOptions{Workspace: NewWorkspaceState(""), Dispatcher: &fakeDispatcher{}}, "Doc"},
		{"missing Workspace", CatalogOptions{Doc: fixtureDoc(), Dispatcher: &fakeDispatcher{}}, "Workspace"},
		{"missing Dispatcher", CatalogOptions{Doc: fixtureDoc(), Workspace: NewWorkspaceState("")}, "Dispatcher"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := server.NewMCPServer("t", "1", server.WithToolCapabilities(true))
			_, err := RegisterCatalog(srv, tc.opts)
			if err == nil {
				t.Fatalf("expected error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

// TestBuildToolFromDef_ActionRequiredAndEnumerated verifies that every
// tool's input schema enforces `action` as required and enumerates the
// declared action names. This is the schema invariant the fan-out
// handler relies on.
func TestBuildToolFromDef_ActionRequiredAndEnumerated(t *testing.T) {
	def := ToolDef{
		Name:        "pad_test",
		Description: "test tool",
		Schema:      ToolSchema{Workspace: true},
		Actions: map[string]ActionFn{
			"alpha":   passThrough([]string{"item", "list"}),
			"bravo":   passThrough([]string{"item", "show"}),
			"charlie": passThrough([]string{"item", "create"}),
		},
	}
	tool := buildToolFromDef(def)
	if tool.Name != "pad_test" {
		t.Errorf("tool.Name = %q, want pad_test", tool.Name)
	}

	// The schema is exposed via JSON marshalling; round-trip through
	// the wire shape so the test asserts what mcp-go actually emits.
	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal input schema: %v", err)
	}
	var schema struct {
		Properties map[string]struct {
			Type string   `json:"type"`
			Enum []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal input schema: %v", err)
	}
	action, ok := schema.Properties["action"]
	if !ok {
		t.Fatalf("input schema missing 'action' property; got %s", raw)
	}
	if action.Type != "string" {
		t.Errorf("action.type = %q, want string", action.Type)
	}
	wantEnum := []string{"alpha", "bravo", "charlie"}
	if !equalStrings(action.Enum, wantEnum) {
		t.Errorf("action.enum = %v, want %v (lexically sorted)", action.Enum, wantEnum)
	}
	if !contains(schema.Required, "action") {
		t.Errorf("'action' missing from required[]; got %v", schema.Required)
	}
	if _, ok := schema.Properties["workspace"]; !ok {
		t.Errorf("Schema.Workspace=true but workspace property absent; got %s", raw)
	}
}

// TestMakeFanOutHandler_DispatchesPerAction verifies the handler reads
// `action` from the input, looks up the right ActionFn, and forwards.
func TestMakeFanOutHandler_DispatchesPerAction(t *testing.T) {
	called := ""
	def := ToolDef{
		Name:   "pad_test",
		Schema: ToolSchema{},
		Actions: map[string]ActionFn{
			"foo": func(_ context.Context, _ map[string]any, _ ActionEnv) (*mcp.CallToolResult, error) {
				called = "foo"
				return mcp.NewToolResultText("foo-result"), nil
			},
			"bar": func(_ context.Context, _ map[string]any, _ ActionEnv) (*mcp.CallToolResult, error) {
				called = "bar"
				return mcp.NewToolResultText("bar-result"), nil
			},
		},
	}
	env := ActionEnv{Workspace: NewWorkspaceState("")}
	handler := makeFanOutHandler(def, env)

	res, err := handler(context.Background(), callToolRequest(map[string]any{"action": "bar"}))
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if res.IsError {
		t.Errorf("expected success, got error result: %s", textOf(res))
	}
	if called != "bar" {
		t.Errorf("expected 'bar' handler called, got %q", called)
	}
	if got := textOf(res); got != "bar-result" {
		t.Errorf("result text = %q, want bar-result", got)
	}
}

// TestMakeFanOutHandler_MissingAction returns a structured error
// listing valid actions so the agent can self-correct without a
// human round-trip. Wire-level guarantee: IsError = true, message
// contains every valid action name.
func TestMakeFanOutHandler_MissingAction(t *testing.T) {
	def := ToolDef{
		Name: "pad_test",
		Actions: map[string]ActionFn{
			"alpha": passThrough([]string{"item", "list"}),
			"beta":  passThrough([]string{"item", "show"}),
		},
	}
	handler := makeFanOutHandler(def, ActionEnv{Workspace: NewWorkspaceState("")})
	res, err := handler(context.Background(), callToolRequest(map[string]any{}))
	if err != nil {
		t.Fatalf("handler returned protocol error (wanted IsError result): %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true; got success result %q", textOf(res))
	}
	msg := textOf(res)
	if !strings.Contains(msg, "missing required field 'action'") {
		t.Errorf("error message %q does not explain the missing field", msg)
	}
	if !strings.Contains(msg, "alpha") || !strings.Contains(msg, "beta") {
		t.Errorf("error message %q should list valid actions (alpha, beta)", msg)
	}
}

// TestMakeFanOutHandler_StripsActionFromInput proves the routing
// `action` key never reaches the action handler — important because
// some downstream CLI commands (workspace audit-log) declare their
// own `--action` flag, which would collide with the catalog's
// routing key and silently emit the catalog action name as the flag
// value. Strip-at-the-boundary keeps action handlers ergonomic.
func TestMakeFanOutHandler_StripsActionFromInput(t *testing.T) {
	var seen map[string]any
	def := ToolDef{
		Name: "pad_test",
		Actions: map[string]ActionFn{
			"audit-log": func(_ context.Context, input map[string]any, _ ActionEnv) (*mcp.CallToolResult, error) {
				seen = input
				return mcp.NewToolResultText("ok"), nil
			},
		},
	}
	handler := makeFanOutHandler(def, ActionEnv{Workspace: NewWorkspaceState("")})
	_, err := handler(context.Background(), callToolRequest(map[string]any{
		"action":     "audit-log",
		"days":       float64(7),
		"actor_name": "dave",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if _, has := seen["action"]; has {
		t.Errorf("action key leaked into action handler input: %v", seen)
	}
	if seen["days"] != float64(7) {
		t.Errorf("days lost: got %v", seen["days"])
	}
	if seen["actor_name"] != "dave" {
		t.Errorf("actor_name lost: got %v", seen["actor_name"])
	}
}

// TestMakeFanOutHandler_UnknownAction is the symmetric case: action is
// set but not in the handler table. Same recovery pattern — list the
// valid actions so the agent can retry.
func TestMakeFanOutHandler_UnknownAction(t *testing.T) {
	def := ToolDef{
		Name: "pad_test",
		Actions: map[string]ActionFn{
			"foo": passThrough([]string{"item", "list"}),
		},
	}
	handler := makeFanOutHandler(def, ActionEnv{Workspace: NewWorkspaceState("")})
	res, err := handler(context.Background(), callToolRequest(map[string]any{"action": "nope"}))
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true for unknown action")
	}
	msg := textOf(res)
	if !strings.Contains(msg, `unknown action "nope"`) {
		t.Errorf("error message %q does not name the bad action", msg)
	}
	if !strings.Contains(msg, "foo") {
		t.Errorf("error message %q should list the one valid action", msg)
	}
}

// TestPassThrough_ForwardsToDispatcher exercises the helper end-to-end
// against a fake dispatcher. The dispatched cmdPath + cliArgs prove
// that the input flowed through BuildCLIArgs unchanged.
func TestPassThrough_ForwardsToDispatcher(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc: &cmdhelp.Document{
			CmdhelpVersion: "0.1",
			Binary:         "pad",
			Commands: map[string]cmdhelp.Command{
				"item list": {
					Summary: "List items",
					Flags: map[string]cmdhelp.Flag{
						"workspace": {Type: "string"},
					},
				},
			},
		},
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}
	handler := passThrough([]string{"item", "list"})
	res, err := handler(context.Background(), nil, env)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", textOf(res))
	}
	if !equalStrings(disp.gotPath, []string{"item", "list"}) {
		t.Errorf("dispatcher saw cmdPath %v, want [item list]", disp.gotPath)
	}
	// session workspace should be injected by BuildCLIArgs
	wantArgs := []string{"--workspace", "docapp", "--format", "json"}
	if !equalStrings(disp.gotArgs, wantArgs) {
		t.Errorf("dispatcher saw cliArgs %v, want %v", disp.gotArgs, wantArgs)
	}
}

// TestActionEnv_Dispatch_UnknownCmdPath catches catalog/cmdhelp drift.
// If a future commit adds an action whose cmdPath isn't in cmdhelp,
// this surfaces as a "registry bug" error result rather than a panic.
func TestActionEnv_Dispatch_UnknownCmdPath(t *testing.T) {
	env := ActionEnv{
		Doc:        &cmdhelp.Document{Binary: "pad", Commands: map[string]cmdhelp.Command{}},
		Workspace:  NewWorkspaceState(""),
		Dispatcher: &fakeDispatcher{},
	}
	res, err := env.Dispatch(context.Background(), []string{"item", "ghost"}, nil)
	if err != nil {
		t.Fatalf("Dispatch returned protocol error (wanted IsError result): %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true for unknown cmdPath")
	}
	if !strings.Contains(textOf(res), `unknown cmdPath "item ghost"`) {
		t.Errorf("error message %q should name the missing cmdPath", textOf(res))
	}
}

// ── test helpers ──

// callToolRequest builds an mcp.CallToolRequest with the given input
// arguments — the public constructor isn't exposed by mcp-go so we
// reach for the same field-level construction the library's own
// tests use.
func callToolRequest(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}

// textOf returns the text of the first content block, or empty string.
// Robust to mcp-go's content-type variations (TextContent, ErrorContent).
func textOf(res *mcp.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
