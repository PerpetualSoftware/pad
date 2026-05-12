package mcp

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// BootstrapFetcher resolves the agent bootstrap blob for a given
// workspace. Used by pad_set_workspace to embed the blob in its response
// so an MCP host calling pad_set_workspace receives full session context
// in a single round-trip — no follow-up pad_meta.action=bootstrap or
// resource read needed.
//
// Mirrors ResourceFetcher in spirit (both shell out via the CLI), but
// kept narrower so the workspace handler doesn't drag in the full
// resource layer.
type BootstrapFetcher interface {
	Bootstrap(ctx context.Context, workspace string) ([]byte, error)
}

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
//
// bootstrapFetcher is optional. When non-nil, the response embeds the
// AgentBootstrap JSON under `bootstrap`, so a single set-workspace call
// hands the agent a fully-loaded session. When nil (e.g. early in
// pad-cloud's HTTP dispatch where no CLI is available yet), the
// response shape stays {workspace, status} and clients can fetch
// bootstrap separately via pad_meta.action=bootstrap or the resource.
func SetWorkspaceTool(state *WorkspaceState, bootstrapFetcher BootstrapFetcher) (mcp.Tool, server.ToolHandlerFunc) {
	tool := mcp.NewTool(
		SetWorkspaceToolName,
		mcp.WithDescription(
			"Set the workspace used as the default --workspace for all "+
				"subsequent tool calls in this MCP session. Pass an empty "+
				"string to clear the session default. When the workspace "+
				"is non-empty and the server supports it, the response "+
				"includes an embedded bootstrap blob — collections, "+
				"always-on conventions, roles, playbook metadata, "+
				"dashboard, recent activity — so the agent starts the "+
				"session with full context in one call.",
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
		out := map[string]any{"workspace": ws, "status": "ok"}
		// Only attempt bootstrap embedding for a non-empty workspace.
		// Clearing the session default (ws="") never needs context.
		if ws != "" && bootstrapFetcher != nil {
			if raw, berr := bootstrapFetcher.Bootstrap(ctx, ws); berr == nil && len(raw) > 0 {
				// Decode + re-attach so the result is a structured
				// object inside the JSON, not a stringified blob the
				// agent has to parse a second time.
				var bs any
				if json.Unmarshal(raw, &bs) == nil {
					out["bootstrap"] = bs
				}
			}
			// If bootstrap fails, fall through silently — the workspace
			// switch already succeeded, and the agent can fetch context
			// via pad_meta.action=bootstrap on its own. We deliberately
			// don't fail the whole call on a bootstrap glitch.
		}
		b, _ := json.Marshal(out)
		return mcp.NewToolResultText(string(b)), nil
	}
	return tool, handler
}

// registerSetWorkspaceTool installs the built-in workspace tool on srv.
func registerSetWorkspaceTool(srv *server.MCPServer, state *WorkspaceState, fetcher BootstrapFetcher) {
	tool, handler := SetWorkspaceTool(state, fetcher)
	srv.AddTool(tool, handler)
}
