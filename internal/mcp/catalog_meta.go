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
		Workspace: false, // server-wide; no workspace context needed
		Params:    nil,   // only `action` (added by buildToolFromDef)
	},
	Actions: map[string]ActionFn{
		"server-info":  actionMetaServerInfo,
		"version":      actionMetaVersion,
		"tool-surface": actionMetaToolSurface,
	},
}

const padMetaToolDescription = `Server introspection — version, capabilities, and the v0.2 tool catalog.

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

Use pad_meta when an agent needs to discover server capabilities or generate
documentation. For runtime task work, use pad_item / pad_workspace / etc.`

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
// Schema is intentionally simple — name + description per tool, plus
// per-action name + presence. Full per-action parameter schemas live
// in tools/list (the JSON Schema mcp-go emits for each tool); the
// catalog summary is a higher-level "what can this server do" view.
func actionMetaToolSurface(_ context.Context, _ map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
	type actionSummary struct {
		Name string `json:"name"`
	}
	type toolSummary struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Workspace   bool            `json:"workspace"`
		Actions     []actionSummary `json:"actions"`
	}
	tools := make([]toolSummary, 0, len(env.Catalog))
	for _, def := range env.Catalog {
		actions := make([]actionSummary, 0, len(def.Actions))
		for _, name := range sortedActionNames(def) {
			actions = append(actions, actionSummary{Name: name})
		}
		tools = append(tools, toolSummary{
			Name:        def.Name,
			Description: def.Description,
			Workspace:   def.Schema.Workspace,
			Actions:     actions,
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
