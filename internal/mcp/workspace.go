package mcp

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// SetWorkspaceToolName is the canonical name of the built-in workspace
// tool. Stable across versions — agents bind to it by name, so renaming
// breaks every prompt template that references it.
const SetWorkspaceToolName = "pad_set_workspace"

// WorkspaceState is the per-session workspace selected via the
// pad_set_workspace tool. Read by every dispatched tool, written only
// by pad_set_workspace; safe for concurrent use.
type WorkspaceState struct {
	mu        sync.RWMutex
	workspace string
}

// NewWorkspaceState returns a state pre-seeded with `initial`. Pass an
// empty string to start with no default — the first tool call will
// then need an explicit `workspace` argument.
func NewWorkspaceState(initial string) *WorkspaceState {
	return &WorkspaceState{workspace: initial}
}

// Get returns the current session workspace, or empty string when
// none has been set.
func (s *WorkspaceState) Get() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.workspace
}

// Set replaces the session workspace. Pass an empty string to clear.
func (s *WorkspaceState) Set(ws string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workspace = ws
}

// SetWorkspaceTool returns the (Tool, Handler) pair for the built-in
// pad_set_workspace tool. The handler updates state in place.
//
// Exposed as a constructor (rather than a registration shortcut) so
// tests can invoke the handler directly without spinning a full
// MCPServer.
func SetWorkspaceTool(state *WorkspaceState) (mcp.Tool, server.ToolHandlerFunc) {
	tool := mcp.NewTool(
		SetWorkspaceToolName,
		mcp.WithDescription(
			"Set the workspace used as the default --workspace for all "+
				"subsequent tool calls in this MCP session. Pass an empty "+
				"string to clear the session default.",
		),
		mcp.WithString("workspace",
			mcp.Description("Workspace slug. Empty string clears the session default."),
			mcp.Required(),
		),
	)
	handler := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ws, err := req.RequireString("workspace")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		state.Set(ws)
		out := map[string]string{"workspace": ws, "status": "ok"}
		b, _ := json.Marshal(out)
		return mcp.NewToolResultText(string(b)), nil
	}
	return tool, handler
}

// registerSetWorkspaceTool installs the built-in workspace tool on srv.
func registerSetWorkspaceTool(srv *server.MCPServer, state *WorkspaceState) {
	tool, handler := SetWorkspaceTool(state)
	srv.AddTool(tool, handler)
}
