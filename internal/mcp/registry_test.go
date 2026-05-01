package mcp

import (
	"context"
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
	handler := makeDispatchHandler("item create", doc.Commands["item create"], disp, state)
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
	handler := makeDispatchHandler("item show", cmdInfo, disp, state)

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
