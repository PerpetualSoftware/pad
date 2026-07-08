package mcp

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
)

// padMetaTool is the server-introspection tool (v0.4 catalog). Four actions —
// three handled inline (read in-memory state and return JSON), one
// (bootstrap) dispatches through env.Dispatch since it needs workspace
// context and the same underlying CLI handler the bootstrap resource
// uses.
//
// Why a tool and not just resources: pad://_meta/version is the
// canonical resource for read-only version metadata, but tools/list
// is what most agents introspect first when establishing context.
// Exposing the same data plus a full catalog dump as a tool means
// agents that haven't enumerated resources yet can still discover the
// surface in one tool call. pad_meta complements RegisterMeta — it
// doesn't replace it.
//
// The four actions:
//
//   - server-info   → {name, version} — minimal handshake-equivalent.
//   - version       → MetaPayload — same JSON the meta resource serves.
//   - tool-surface  → {tools: [...]} — full catalog for getpad.dev/docs/mcp.
//   - bootstrap     → AgentBootstrap blob (PLAN-1377 / TASK-1380). Equivalent
//                     to pad://workspace/{ws}/bootstrap and pad_set_workspace's
//                     embedded response.
//
// server-info / version / tool-surface are inline (no env.Dispatch);
// bootstrap shells through env.Dispatch so it reuses the canonical CLI/HTTP
// handler instead of forking workspace context loading.

func init() {
	appendToCatalog(padMetaTool)
}

var padMetaTool = ToolDef{
	Name:        "pad_meta",
	Description: padMetaToolDescription,
	Schema: ToolSchema{
		// bootstrap requires workspace context; server-info / version /
		// tool-surface do not (they read in-memory state only). Marking
		// Workspace=true here makes the workspace param available to
		// every action — the action handler decides whether to use it.
		Workspace: true,
		Params:    nil,
	},
	Actions: map[string]ActionFn{
		"server-info":  actionMetaServerInfo,
		"version":      actionMetaVersion,
		"tool-surface": actionMetaToolSurface,
		"bootstrap":    actionMetaBootstrap,
	},
}

const padMetaToolDescription = `Server introspection — version, capabilities, the hand-curated tool catalog, and the agent bootstrap blob.

Actions:
  server-info   — Server name + runtime version. Lightweight; no params.
  version       — Full version metadata (pad version, cmdhelp version, tool surface
                  version, MCP protocol version). Same JSON as pad://_meta/version.
  tool-surface  — catalog dump: every tool managed by the hand-curated
                  catalog (the ten resource × action tools — pad_set_workspace
                  is registered separately and not included), its actions, and
                  its input schema. tools/list also contains pad_set_workspace
                  and matches the catalog otherwise — the historical cmdhelp
                  walker was retired in TASK-981.
  bootstrap     — Consolidated agent context-load blob (workspace + user + collections
                  + always-on conventions + roles + playbook metadata + dashboard +
                  recent activity). On-demand refresh for agents that didn't get the
                  resource prefetch, or want a fresh snapshot mid-session after lots
                  of mutations. Equivalent to pad://workspace/{ws}/bootstrap and to
                  pad_set_workspace's embedded response.

Use pad_meta when an agent needs to discover server capabilities, regenerate context,
or build documentation. For runtime task work, use pad_item / pad_workspace / etc.`

// actionMetaServerInfo returns the minimal {name, version} pair.
// Roughly equivalent to what the MCP handshake exposes, but available
// post-handshake without a full reconnect.
func actionMetaServerInfo(_ context.Context, _ map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
	version := env.PadVersion
	if version == "" {
		version = FallbackVersion
	}
	payload := map[string]any{
		"name":    ServerName,
		"version": version,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		// Marshalling a string-valued map can't realistically fail;
		// surface as an error result rather than panicking.
		return mcp.NewToolResultErrorf("pad_meta.server-info: marshal: %s", err.Error()), nil
	}
	return mcp.NewToolResultStructured(payload, string(body)), nil
}

// actionMetaVersion returns the same MetaPayload that pad://_meta/version
// serves as a resource. Two surfaces, one source of truth (BuildMetaPayload).
func actionMetaVersion(_ context.Context, _ map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
	payload := BuildMetaPayload(env.PadVersion)
	body, err := json.Marshal(payload)
	if err != nil {
		return mcp.NewToolResultErrorf("pad_meta.version: marshal: %s", err.Error()), nil
	}
	return mcp.NewToolResultStructured(payload, string(body)), nil
}

// actionMetaBootstrap dispatches to `pad bootstrap` so MCP callers get
// the same AgentBootstrap blob the HTTP endpoint, CLI, and resource all
// return. The dispatcher resolves the session workspace via the standard
// precedence chain (explicit arg → pad_set_workspace → CWD-linked).
//
// This is intentionally a passThrough — keep the canonical builder
// (Server.BuildAgentBootstrap) the single source of truth for shape,
// validation, and visibility filtering. The MCP surface contributes
// only the discovery affordance, not a divergent implementation.
func actionMetaBootstrap(ctx context.Context, input map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
	return env.Dispatch(ctx, []string{"bootstrap"}, input)
}

// actionMetaToolSurface returns the catalog as JSON. Pairs with
// PLAN-943 TASK-957 (getpad.dev/docs/mcp) — that page can generate
// docs from this single canonical source instead of re-introspecting
// tools/list and reverse-engineering action enums.
//
// Scope: the catalog only — the eight hand-curated resource × action
// tools (pad_item, pad_workspace, pad_collection, pad_project, pad_role,
// pad_search, pad_playbook, pad_meta). pad_set_workspace is registered
// separately and NOT enumerated here — `actionMetaToolSurface` loops
// env.Catalog only. Consumers building from tool-surface should account
// for pad_set_workspace as a known extra (always present in tools/list).
// The historical cmdhelp leaf walker was retired in TASK-981.
//
// The response carries a `rollout_status` field that callers should
// treat as opaque metadata — populated for backwards compatibility
// with consumers that branched on it during the v0.1→v0.2 cmdhelp
// retirement.
//
// Schema: per-tool entries carry name + description + workspace flag +
// actions[] + params[]. The params list is synthesized to mirror what
// consumers see in tools/list — `action` (always required, enum of
// declared actions), `workspace` (when ToolDef.Schema.Workspace=true),
// and the per-tool ParamDefs. This makes the dump self-contained for
// docs generators: they don't have to reproduce buildToolFromDef's
// implicit-param logic separately.
func actionMetaToolSurface(_ context.Context, _ map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
	// Shared serializer (TASK-1891): buildToolSurfacePayload is the
	// single source of truth for the tool-surface shape, consumed by
	// both this MCP action and the cycle-free ToolSurfaceJSON() that
	// backs GET /api/v1/mcp/tool-surface. env.Catalog is the same slice
	// as the package-global Catalog at registration time; passing it
	// explicitly keeps the action reading the env it was wired with.
	// Each action now carries a `read_only` bool — additive metadata,
	// so consumers that ignore unknown keys are unaffected.
	payload := buildToolSurfacePayload(env.Catalog)
	body, err := json.Marshal(payload)
	if err != nil {
		return mcp.NewToolResultErrorf("pad_meta.tool-surface: marshal: %s", err.Error()), nil
	}
	return mcp.NewToolResultStructured(payload, string(body)), nil
}
