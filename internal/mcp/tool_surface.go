package mcp

import "encoding/json"

// tool_surface.go — the cycle-free catalog→JSON serializer (PLAN-1888 /
// TASK-1891, DR-3). Builds the same tool-surface descriptor blob that
// pad_meta.action: tool-surface emits, PLUS a per-action `read_only`
// bool, directly from the package-global Catalog. No server types, no
// ActionEnv — so cmd/pad/main.go can build an http.Handler from this
// and inject it into *server.Server without internal/server having to
// import internal/mcp (which would close the import cycle —
// internal/mcp already imports internal/server via dispatch_http.go).
//
// Why a per-action read_only flag (DR-2): WebMCP's readOnlyHint is
// per-TOOL, but the fat catalog tools straddle read+write (pad_item
// mixes show/list AND create/update/delete). The browser layer
// (Phase 3) derives readOnlyHint=true only when EVERY action a tool
// exposes is a read. Emitting the per-action bool here keeps that
// derivation a lookup rather than a re-derivation of the read set in
// TypeScript. The read set lives in Go (readOnlyActions below) as the
// single source of truth.

// toolSurfaceActionSummary is one action entry in the serialized
// surface. Adds `read_only` to the existing {name} shape — additive,
// so any consumer of pad_meta.action: tool-surface that ignores unknown
// keys is unaffected (DR-7: no ToolSurfaceVersion bump).
type toolSurfaceActionSummary struct {
	Name     string `json:"name"`
	ReadOnly bool   `json:"read_only"`
}

type toolSurfaceParamSummary struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

type toolSurfaceToolSummary struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	Workspace   bool                       `json:"workspace"`
	Actions     []toolSurfaceActionSummary `json:"actions"`
	Params      []toolSurfaceParamSummary  `json:"params"`
}

// readOnlyActions is the allowlist of (tool, action) pairs that perform
// no mutation — pure reads / introspection. Co-located with the catalog
// (DR-2) and consulted by the serializer to stamp each action's
// `read_only` bool. Keyed by tool name → set of read-only action names.
//
// Anything NOT listed here is treated as a write (the safe default —
// a missing entry produces read_only=false, so a forgotten mutating
// action never silently advertises as read-only). Confirmed against the
// catalog_*.go ToolDefs:
//
//   - pad_item:       get/list/deps/starred/list-comments/backlinks/export read;
//     create/update/delete/move/restore/link/unlink/star/unstar/
//     comment/bulk-update/note/decide/import write.
//   - pad_workspace:  list/members/storage/audit-log read; invite/create/claim write.
//   - pad_collection: list read; create/update/delete write.
//   - pad_project:    all read (dashboard/next/standup/changelog/report).
//   - pad_role:       list read; create/update/delete write.
//   - pad_search:     query read.
//   - pad_playbook:   list/get read; run is side-effect-free (returns the
//     body + bound args for the agent to execute) → read.
//   - pad_library:    list/get read; activate mutates workspace state → write.
//   - pad_meta:       server-info/version/tool-surface/bootstrap all read.
var readOnlyActions = map[string]map[string]bool{
	"pad_item": {
		"get":           true,
		"list":          true,
		"deps":          true,
		"starred":       true,
		"list-comments": true,
		"backlinks":     true,
		"export":        true,
	},
	"pad_workspace": {
		"list":      true,
		"members":   true,
		"storage":   true,
		"audit-log": true,
	},
	"pad_collection": {
		"list": true,
	},
	"pad_project": {
		"dashboard": true,
		"next":      true,
		"standup":   true,
		"changelog": true,
		"report":    true,
	},
	"pad_role": {
		"list": true,
	},
	"pad_search": {
		"query": true,
	},
	"pad_playbook": {
		"list": true,
		"get":  true,
		"run":  true,
	},
	"pad_library": {
		"list": true,
		"get":  true,
	},
	"pad_meta": {
		"server-info":  true,
		"version":      true,
		"tool-surface": true,
		"bootstrap":    true,
	},
}

// isReadOnlyAction reports whether (toolName, action) performs no
// mutation. Defaults to false (write) for any pair not in the
// allowlist — the conservative direction.
func isReadOnlyAction(toolName, action string) bool {
	actions, ok := readOnlyActions[toolName]
	if !ok {
		return false
	}
	return actions[action]
}

// buildToolSurfaceTools projects a catalog slice into the serialized
// per-tool summaries. Shared by ToolSurfaceJSON (the REST/browser path)
// and actionMetaToolSurface (the MCP path) so the two surfaces can't
// drift. The synthesized param list mirrors buildToolFromDef's
// implicit-param logic (action enum first, optional workspace, then the
// declared ParamDefs) so the dump is self-contained for consumers.
func buildToolSurfaceTools(catalog []ToolDef) []toolSurfaceToolSummary {
	tools := make([]toolSurfaceToolSummary, 0, len(catalog))
	for _, def := range catalog {
		actionNames := sortedActionNames(def)
		actions := make([]toolSurfaceActionSummary, 0, len(actionNames))
		for _, name := range actionNames {
			actions = append(actions, toolSurfaceActionSummary{
				Name:     name,
				ReadOnly: isReadOnlyAction(def.Name, name),
			})
		}
		// `action` is always present and always required — buildToolFromDef
		// injects it into the schema. Synthesize it here so consumers get
		// the full param picture. Enum mirrors sortedActionNames.
		params := []toolSurfaceParamSummary{{
			Name:        "action",
			Type:        "string",
			Description: "The action to perform. Required.",
			Enum:        actionNames,
		}}
		if def.Schema.Workspace {
			params = append(params, toolSurfaceParamSummary{
				Name: "workspace",
				Type: "string",
				Description: "Workspace slug. An explicit value always wins. A " +
					"single-user local server else uses the pad_set_workspace " +
					"session default, then the .pad.toml workspace; a multi-user/" +
					"remote server requires an explicit workspace per call.",
			})
		}
		for _, p := range def.Schema.Params {
			params = append(params, toolSurfaceParamSummary{
				Name:        p.Name,
				Type:        p.Type,
				Description: p.Description,
				Enum:        p.Enum,
			})
		}
		tools = append(tools, toolSurfaceToolSummary{
			Name:        def.Name,
			Description: def.Description,
			Workspace:   def.Schema.Workspace,
			Actions:     actions,
			Params:      params,
		})
	}
	return tools
}

// buildToolSurfacePayload assembles the full tool-surface document
// (version + rollout status + tools) from a catalog slice. The payload
// shape is identical to what actionMetaToolSurface emitted pre-TASK-1891
// except every action now carries `read_only`.
func buildToolSurfacePayload(catalog []ToolDef) map[string]any {
	// rollout_status mirrors the historical actionMetaToolSurface logic:
	// "complete" once the cmdhelp leaf walker retired (any version past
	// the initial 0.1). Kept for backwards compatibility with consumers
	// that branched on it during the v0.1→v0.2 retirement.
	rolloutStatus := "in-progress"
	if ToolSurfaceVersion != "0.1" {
		rolloutStatus = "complete"
	}
	return map[string]any{
		"tool_surface_version": ToolSurfaceVersion,
		"rollout_status":       rolloutStatus,
		"tools":                buildToolSurfaceTools(catalog),
	}
}

// ToolSurfaceJSON serializes the package-global Catalog into the
// tool-surface descriptor JSON — the same blob pad_meta.action:
// tool-surface emits, plus a per-action `read_only` bool. Cycle-free:
// reads only the in-package Catalog + readOnlyActions, no server types.
//
// cmd/pad/main.go builds an http.Handler from this and injects it into
// *server.Server (the SetMCPTransport pattern) so the
// GET /api/v1/mcp/tool-surface endpoint can serve it without
// internal/server importing internal/mcp.
//
// Scope is the nine env.Catalog tools (pad_item, pad_workspace,
// pad_collection, pad_project, pad_role, pad_search, pad_playbook,
// pad_meta, pad_library). pad_set_workspace is registered separately
// (registry.go) and is NOT in Catalog, so it's naturally excluded.
func ToolSurfaceJSON() ([]byte, error) {
	return json.Marshal(buildToolSurfacePayload(Catalog))
}
