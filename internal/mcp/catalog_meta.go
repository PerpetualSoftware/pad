package mcp

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
)

// padMetaTool is the v0.2 server-introspection tool. Three actions,
// all handled inline (no CLI dispatch) — they read in-memory state
// and return JSON.
//
// Why a tool and not just resources: pad://_meta/version is the
// canonical resource for read-only version metadata, but tools/list
// is what most agents introspect first when establishing context.
// Exposing the same data plus a full catalog dump as a tool means
// agents that haven't enumerated resources yet can still discover the
// surface in one tool call. pad_meta complements RegisterMeta — it
// doesn't replace it.
//
// The three actions:
//
//   - server-info   → {name, version} — minimal handshake-equivalent.
//   - version       → MetaPayload — same JSON the meta resource serves.
//   - tool-surface  → {tools: [...]} — full catalog for getpad.dev/docs/mcp.
//
// All inline → padMetaTool's actions never call env.Dispatch.

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

const padMetaToolDescription = `Server introspection — version, capabilities, the v0.2 tool catalog, and the agent bootstrap blob.

Actions:
  server-info   — Server name + runtime version. Lightweight; no params.
  version       — Full version metadata (pad version, cmdhelp version, tool surface
                  version, MCP protocol version). Same JSON as pad://_meta/version.
  tool-surface  — v0.2 catalog dump: every tool managed by the hand-curated catalog,
                  its actions, and its input schema. During PLAN-969's rollout the
                  cmdhelp walker also contributes to tools/list; tool-surface
                  intentionally only enumerates the catalog (the source it can
                  introspect cleanly). For the complete advertised surface, read
                  tools/list directly.
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

// actionMetaToolSurface returns the v0.2 catalog as JSON. Pairs with
// PLAN-943 TASK-957 (getpad.dev/docs/mcp) — that page can generate
// docs from this single canonical source instead of re-introspecting
// tools/list and reverse-engineering action enums.
//
// Scope: the v0.2 catalog only. Until TASK-981 retires the cmdhelp
// walker, tools/list contains both the catalog (currently pad_meta)
// AND the walker-derived ~85 verb tools — but tool-surface
// deliberately does not enumerate the walker output. The walker
// surface is opaque to the catalog (no shared ToolDef shape), and
// hand-mapping it would cost duplication for value that's about to
// disappear in TASK-981.
//
// During rollout the response includes a `rollout_status` field so
// consumers can detect "catalog is partial" and fall back to
// tools/list when they need the complete advertised surface.
//
// Schema: per-tool entries carry name + description + workspace flag +
// actions[] + params[]. The params list is synthesized to mirror what
// consumers see in tools/list — `action` (always required, enum of
// declared actions), `workspace` (when ToolDef.Schema.Workspace=true),
// and the per-tool ParamDefs. This makes the dump self-contained for
// docs generators: they don't have to reproduce buildToolFromDef's
// implicit-param logic separately.
func actionMetaToolSurface(_ context.Context, _ map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
	type actionSummary struct {
		Name string `json:"name"`
	}
	type paramSummary struct {
		Name        string   `json:"name"`
		Type        string   `json:"type"`
		Description string   `json:"description,omitempty"`
		Enum        []string `json:"enum,omitempty"`
	}
	type toolSummary struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Workspace   bool            `json:"workspace"`
		Actions     []actionSummary `json:"actions"`
		Params      []paramSummary  `json:"params"`
	}
	tools := make([]toolSummary, 0, len(env.Catalog))
	for _, def := range env.Catalog {
		actions := make([]actionSummary, 0, len(def.Actions))
		for _, name := range sortedActionNames(def) {
			actions = append(actions, actionSummary{Name: name})
		}
		// `action` is always present and always required — buildToolFromDef
		// injects it into the schema. Synthesize it here so consumers
		// reading tool-surface get the full param picture without having
		// to know about that injection. Enum mirrors sortedActionNames.
		params := []paramSummary{{
			Name:        "action",
			Type:        "string",
			Description: "The action to perform. Required.",
			Enum:        sortedActionNames(def),
		}}
		if def.Schema.Workspace {
			params = append(params, paramSummary{
				Name: "workspace",
				Type: "string",
				Description: "Workspace slug. Defaults to the session workspace " +
					"set via pad_set_workspace, then to the CWD-linked workspace " +
					"from .pad.toml.",
			})
		}
		for _, p := range def.Schema.Params {
			params = append(params, paramSummary{
				Name:        p.Name,
				Type:        p.Type,
				Description: p.Description,
				Enum:        p.Enum,
			})
		}
		tools = append(tools, toolSummary{
			Name:        def.Name,
			Description: def.Description,
			Workspace:   def.Schema.Workspace,
			Actions:     actions,
			Params:      params,
		})
	}
	// rollout_status is "complete" only after TASK-981 retires the
	// cmdhelp walker and the catalog is the sole tools/list source.
	// Before then, "in-progress" signals to consumers that the catalog
	// is a strict subset of the advertised surface and they should
	// read tools/list directly if they need everything.
	rolloutStatus := "in-progress"
	if ToolSurfaceVersion != "0.1" {
		rolloutStatus = "complete"
	}
	payload := map[string]any{
		"tool_surface_version": ToolSurfaceVersion,
		"rollout_status":       rolloutStatus,
		"tools":                tools,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return mcp.NewToolResultErrorf("pad_meta.tool-surface: marshal: %s", err.Error()), nil
	}
	return mcp.NewToolResultStructured(payload, string(body)), nil
}
