package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
)

// fakeDispatcher records the dispatch invocation for assertion.
type fakeDispatcher struct {
	gotPath []string
	gotArgs []string
	out     string
}

func (f *fakeDispatcher) Dispatch(ctx context.Context, cmdPath, args []string) (*mcp.CallToolResult, error) {
	f.gotPath = append([]string(nil), cmdPath...)
	f.gotArgs = append([]string(nil), args...)
	if f.out == "" {
		f.out = "ok"
	}
	return mcp.NewToolResultText(f.out), nil
}

// fixtureDoc mirrors a slice of pad's real tree: groups, leaves,
// excluded entries, and a multi-word path for snake-case naming.
func fixtureDoc() *cmdhelp.Document {
	return &cmdhelp.Document{
		CmdhelpVersion: "0.1",
		Binary:         "pad",
		Commands: map[string]cmdhelp.Command{
			"item":                  {Summary: "Item commands"},
			"item create":           {Summary: "Create item"},
			"item list":             {Summary: "List items"},
			"db":                    {Summary: "DB ops"},
			"db backup":             {Summary: "Backup DB"},
			"mcp":                   {Summary: "MCP"},
			"mcp serve":             {Summary: "MCP server"},
			"completion":            {Summary: "Generate shell completions"},
			"completion bash":       {Summary: "bash completion"},
			"completion zsh":        {Summary: "zsh completion"},
			"workspace":             {Summary: "ws commands"},
			"workspace context":     {Summary: "ws context group"},
			"workspace context set": {Summary: "set context"},
		},
	}
}

func TestRegister_RegistersLeaves_AppliesDefaultExcludes(t *testing.T) {
	srv := server.NewMCPServer("t", "1", server.WithToolCapabilities(true))
	count, err := Register(srv, RegistryOptions{
		Doc:        fixtureDoc(),
		Workspace:  NewWorkspaceState(""),
		Dispatcher: &fakeDispatcher{},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Leaves: item create, item list, db backup, mcp serve,
	// completion bash, completion zsh, workspace context set.
	// DefaultExcludes drops: db backup, mcp serve, completion (group → all
	// children). So registered leaves = item create, item list,
	// workspace context set = 3, plus pad_set_workspace = 4.
	if count != 4 {
		t.Errorf("expected 4 tools (3 leaves + pad_set_workspace), got %d", count)
	}
}

func TestRegister_NoExcludesRegistersEverything(t *testing.T) {
	srv := server.NewMCPServer("t", "1", server.WithToolCapabilities(true))
	count, err := Register(srv, RegistryOptions{
		Doc:             fixtureDoc(),
		Workspace:       NewWorkspaceState(""),
		Dispatcher:      &fakeDispatcher{},
		ExcludeCommands: []string{}, // empty = nothing excluded
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// All 7 leaves + pad_set_workspace = 8.
	if count != 8 {
		t.Errorf("expected 8 tools, got %d", count)
	}
}

func TestRegister_ExcludePrefixSuppressesAllChildren(t *testing.T) {
	srv := server.NewMCPServer("t", "1", server.WithToolCapabilities(true))
	count, err := Register(srv, RegistryOptions{
		Doc:             fixtureDoc(),
		Workspace:       NewWorkspaceState(""),
		Dispatcher:      &fakeDispatcher{},
		ExcludeCommands: []string{"completion", "db"},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Suppressed: completion bash/zsh, db backup. Leaves left: item create,
	// item list, mcp serve, workspace context set = 4 + pad_set_workspace.
	if count != 5 {
		t.Errorf("expected 5 tools, got %d", count)
	}
}

func TestRegister_ValidatesRequiredOptions(t *testing.T) {
	srv := server.NewMCPServer("t", "1", server.WithToolCapabilities(true))
	state := NewWorkspaceState("")
	disp := &fakeDispatcher{}
	doc := fixtureDoc()

	cases := []struct {
		name string
		opts RegistryOptions
	}{
		{"missing Doc", RegistryOptions{Workspace: state, Dispatcher: disp}},
		{"missing Workspace", RegistryOptions{Doc: doc, Dispatcher: disp}},
		{"missing Dispatcher", RegistryOptions{Doc: doc, Workspace: state}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Register(srv, c.opts); err == nil {
				t.Errorf("expected error for %s, got nil", c.name)
			}
		})
	}
}

func TestToolNameFromPath_SnakeCase(t *testing.T) {
	cases := map[string]string{
		"item create":           "item_create",
		"workspace context set": "workspace_context_set",
		"db":                    "db",
		"":                      "",
	}
	for in, want := range cases {
		if got := ToolNameFromPath(in); got != want {
			t.Errorf("ToolNameFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegister_DispatchHandler_RoutesAndInjectsWorkspace(t *testing.T) {
	disp := &fakeDispatcher{out: `{"ok": true}`}
	state := NewWorkspaceState("docapp")
	doc := &cmdhelp.Document{
		CmdhelpVersion: "0.1",
		Binary:         "pad",
		Commands: map[string]cmdhelp.Command{
			"item create": {
				Summary: "Create item",
				Args: []cmdhelp.Arg{
					{Name: "collection", Type: "string", Required: true},
					{Name: "title", Type: "string", Required: true},
				},
				Flags: map[string]cmdhelp.Flag{
					"priority": {Type: "string"},
				},
			},
		},
	}
	srv := server.NewMCPServer("t", "1", server.WithToolCapabilities(true))
	if _, err := Register(srv, RegistryOptions{
		Doc:             doc,
		Workspace:       state,
		Dispatcher:      disp,
		ExcludeCommands: []string{},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Drive the handler by computing it the same way Register does
	// — this isolates the routing logic from server internals.
	handler := makeDispatchHandler("item create", doc.Commands["item create"], disp, state, nil)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"collection": "tasks",
		"title":      "Fix OAuth",
		"priority":   "high",
	}
	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Errorf("handler returned IsError; expected success")
	}
	if !equalSlice(disp.gotPath, []string{"item", "create"}) {
		t.Errorf("dispatch path = %v, want [item create]", disp.gotPath)
	}
	wantArgs := []string{"tasks", "Fix OAuth", "--priority", "high", "--workspace", "docapp", "--format", "json"}
	if !equalSlice(disp.gotArgs, wantArgs) {
		t.Errorf("dispatch args = %v, want %v", disp.gotArgs, wantArgs)
	}
}

func TestRegister_DispatchHandler_ReportsValidationErrors(t *testing.T) {
	// Missing required arg should NOT shell out — handler returns
	// IsError result with the validation message, dispatcher untouched.
	disp := &fakeDispatcher{}
	state := NewWorkspaceState("")
	cmdInfo := cmdhelp.Command{
		Args: []cmdhelp.Arg{{Name: "ref", Type: "string", Required: true}},
	}
	handler := makeDispatchHandler("item show", cmdInfo, disp, state, nil)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError result for missing required arg")
	}
	if disp.gotPath != nil {
		t.Errorf("dispatcher should NOT be called on validation failure; got path=%v", disp.gotPath)
	}
}

func TestBuildTool_HyphenatedFlagsSurfaceAsSnakeCase(t *testing.T) {
	// TASK-964: kebab-case flag names (`--due-date`) become snake_case
	// schema properties (`due_date`) so agents can emit standard
	// JSON without having to special-case hyphenated keys.
	cmd := cmdhelp.Command{
		Summary: "Update item",
		Args: []cmdhelp.Arg{
			{Name: "ref", Type: "string", Required: true}, // already underscore-free
		},
		Flags: map[string]cmdhelp.Flag{
			"due-date":   {Type: "string"},
			"blocked-by": {Type: "string"},
			"workspace":  {Type: "string"}, // single-word: round-trip unchanged
		},
	}
	tool := buildTool("item update", cmd)

	// Hyphens must NOT appear as schema property names.
	for prop := range tool.InputSchema.Properties {
		if strings.Contains(prop, "-") {
			t.Errorf("schema property %q contains hyphen — agents see kebab-case where they expect snake_case", prop)
		}
	}
	// The translated forms must be present.
	wantPresent := []string{"ref", "due_date", "blocked_by", "workspace"}
	for _, want := range wantPresent {
		if _, ok := tool.InputSchema.Properties[want]; !ok {
			t.Errorf("schema missing %q; got %v", want, tool.InputSchema.Properties)
		}
	}
	// And the kebab forms must NOT be present (no double-publishing).
	wantAbsent := []string{"due-date", "blocked-by"}
	for _, gone := range wantAbsent {
		if _, ok := tool.InputSchema.Properties[gone]; ok {
			t.Errorf("schema still contains kebab-case %q alongside snake_case form", gone)
		}
	}
}

func TestMCPPropertyName_RoundTrip(t *testing.T) {
	// Lock the translation rule so changes to it stay obvious.
	cases := map[string]string{
		"workspace":     "workspace",     // single-word
		"due-date":      "due_date",      // single hyphen
		"blocked-by":    "blocked_by",    // single hyphen
		"x-y-z":         "x_y_z",         // multiple hyphens
		"already_snake": "already_snake", // pre-snaked (idempotent)
		"":              "",              // empty
	}
	for in, want := range cases {
		if got := MCPPropertyName(in); got != want {
			t.Errorf("MCPPropertyName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildTool_OmitsStdinFromInputSchema(t *testing.T) {
	// `--stdin` instructs pad to read content from os.Stdin. The MCP
	// transport doesn't pipe agent stdin to the subprocess, so the
	// flag has no usable semantics over MCP — agents should use
	// --content instead. Hide it from the tool schema so agents
	// don't get tempted, and keep the defensive drop in
	// BuildCLIArgs as a belt-and-braces guard.
	cmd := cmdhelp.Command{
		Summary: "Create item",
		Flags: map[string]cmdhelp.Flag{
			"stdin":   {Type: "bool"},
			"content": {Type: "string"},
		},
	}
	tool := buildTool("item create", cmd)
	if _, ok := tool.InputSchema.Properties["stdin"]; ok {
		t.Errorf("stdin flag should NOT be in tool input schema; properties: %v",
			tool.InputSchema.Properties)
	}
	if _, ok := tool.InputSchema.Properties["content"]; !ok {
		t.Errorf("content flag should be in tool input schema; properties: %v",
			tool.InputSchema.Properties)
	}
}

func TestRegister_DispatchHandler_ForwardsRootFlags(t *testing.T) {
	// Per Codex review: --url is a root persistent flag; if pad mcp
	// serve is invoked as `pad --url X mcp serve`, X must reach
	// every dispatched subprocess. Otherwise tool calls run against
	// the wrong server endpoint.
	disp := &fakeDispatcher{}
	state := NewWorkspaceState("")
	cmdInfo := cmdhelp.Command{
		Summary: "show item",
		Args:    []cmdhelp.Arg{{Name: "ref", Type: "string", Required: true}},
	}
	handler := makeDispatchHandler("item show", cmdInfo, disp, state,
		map[string]string{"url": "https://api.example.com"})

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"ref": "TASK-5"}
	if _, err := handler(context.Background(), req); err != nil {
		t.Fatalf("handler: %v", err)
	}
	wantArgs := []string{"TASK-5", "--url", "https://api.example.com", "--format", "json"}
	if !equalSlice(disp.gotArgs, wantArgs) {
		t.Errorf("dispatch args = %v, want %v", disp.gotArgs, wantArgs)
	}
}

// equalSlice is a small helper for stable []string comparison; tests
// across this package use it.
func equalSlice(a, b []string) bool {
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
