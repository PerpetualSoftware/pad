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
//
// A state may be `shared`: a single process-global instance that serves
// MULTIPLE users (the cloud /mcp transport, where one stateless process
// dispatches for every OAuth user). A shared state MUST NOT be used as a
// per-call resolution default — one user's pad_set_workspace would
// otherwise bleed into another user's tool call, or into another of the
// same user's concurrent sessions (BUG-1865). ResolveDefault() returns
// "" for shared states so resolution falls back to the explicit
// `workspace` argument (or the per-user maybeInjectWorkspace default) and
// never to cross-user shared memory. Get()/Set() are unchanged so the
// single-user local `pad mcp serve` transport keeps working exactly as
// before.
type WorkspaceState struct {
	mu        sync.RWMutex
	workspace string
	shared    bool
}

// NewWorkspaceState returns a state pre-seeded with `initial`. Pass an
// empty string to start with no default — the first tool call will
// then need an explicit `workspace` argument. Use this for the
// single-user local stdio transport (`pad mcp serve`), where the
// session workspace is genuinely per-session and safe to inject as a
// resolution default.
func NewWorkspaceState(initial string) *WorkspaceState {
	return &WorkspaceState{workspace: initial}
}

// NewSharedWorkspaceState returns a state for a multi-user process (the
// cloud /mcp transport). It records Set/Get like any other state, but
// ResolveDefault() always returns "" and IsShared() reports true, so the
// shared value is never injected as a per-call default — the fix for the
// cross-user workspace bleed in BUG-1865.
func NewSharedWorkspaceState() *WorkspaceState {
	return &WorkspaceState{shared: true}
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

// IsShared reports whether this state is a process-global instance
// serving multiple users. Callers use it to avoid persisting or trusting
// a session default that would leak across users (BUG-1865).
func (s *WorkspaceState) IsShared() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.shared
}

// ResolveDefault returns the workspace to inject into a dispatched call
// that didn't carry an explicit `workspace` — or "" when this state must
// not be trusted as a resolution source. A shared multi-user state
// always returns "" (see NewSharedWorkspaceState / BUG-1865); a normal
// single-user state returns its current value.
func (s *WorkspaceState) ResolveDefault() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.shared {
		return ""
	}
	return s.workspace
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
	// The tool BEHAVES differently per deployment, so the description must
	// too (BUG-1865): a single-user local server persists the session
	// default; a shared multi-user server (cloud /mcp) cannot, and telling
	// agents it does leads them to omit `workspace` and hit avoidable
	// no-workspace errors.
	desc := "Set the workspace used as the default --workspace for all " +
		"subsequent tool calls in this MCP session. Pass an empty " +
		"string to clear the session default. When the workspace " +
		"is non-empty and the server supports it, the response " +
		"includes an embedded bootstrap blob — collections, " +
		"always-on conventions, roles, playbook metadata, " +
		"dashboard, recent activity — so the agent starts the " +
		"session with full context in one call."
	if state.IsShared() {
		desc = "Load workspace context for this MCP session and return an " +
			"embedded bootstrap blob (collections, conventions, roles, " +
			"playbook metadata, dashboard) for the given workspace. NOTE: " +
			"this server serves multiple users from one process and does " +
			"NOT persist a session default — you MUST pass `workspace=<slug>` " +
			"explicitly on every subsequent tool call. The response reports " +
			"status=not_persisted as a reminder."
	}
	tool := mcp.NewTool(
		SetWorkspaceToolName,
		mcp.WithDescription(desc),
		mcp.WithString("workspace",
			mcp.Description("Workspace slug to load context for. On multi-user/remote servers this does not persist; pass workspace explicitly on each call."),
			mcp.Required(),
		),
	)
	handler := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ws, err := req.RequireString("workspace")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		// On a shared multi-user state (cloud /mcp), a persisted session
		// default would bleed across users / concurrent sessions
		// (BUG-1865). Don't persist it; report honestly so the agent
		// passes `workspace` explicitly on each call. Still embed the
		// bootstrap blob for the requested workspace (fetched with the
		// caller's own context) so the one-round-trip context load is
		// preserved.
		if state.IsShared() {
			out := map[string]any{
				"workspace": ws,
				"status":    "not_persisted",
				"note": "This MCP server serves multiple users from one process and does not " +
					"persist a session workspace. Pass `workspace=<slug>` explicitly on each tool call.",
			}
			if ws != "" && bootstrapFetcher != nil {
				if raw, berr := bootstrapFetcher.Bootstrap(ctx, ws); berr == nil && len(raw) > 0 {
					var bs any
					if json.Unmarshal(raw, &bs) == nil {
						out["bootstrap"] = bs
					}
				}
			}
			b, _ := json.Marshal(out)
			return mcp.NewToolResultText(string(b)), nil
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
