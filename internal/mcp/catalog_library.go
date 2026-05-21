package mcp

// padLibraryTool exposes the global convention + playbook library to MCP
// callers. Read-mostly; the one mutation (activate) creates a workspace
// item from a library entry by title.
//
// PLAN-1560 / TASK-1563. The HTTP endpoints (TASK-1561) and CLI surface
// (TASK-1562) already exist; this file just makes them discoverable as
// a first-class MCP tool. Closes IDEA-1514 — pure-MCP agents (notably
// the /pad onboard playbook from PLAN-1496) can now walk the library
// without shelling out.
//
// Why Workspace=true even though list + get are global:
//
//   - activate creates an item in the workspace's conventions/playbooks
//     collection — it needs a workspace.
//   - Mixing workspace-bound + workspace-free actions in one tool is the
//     pad_meta precedent: declare Workspace=true so the catalog builder
//     injects the workspace param into the schema, and let each action
//     decide whether to use it. Bonus: activate gets automatic
//     pad_set_workspace session-default resolution for free.
//   - The alternative (Workspace=false + an explicit workspace ParamDef)
//     would force callers to re-pass workspace on every activate call,
//     skipping the session-default chain.

func init() {
	appendToCatalog(padLibraryTool)
}

var padLibraryTool = ToolDef{
	Name:        "pad_library",
	Description: padLibraryToolDescription,
	Schema: ToolSchema{
		Workspace: true,
		Params: []ParamDef{
			{
				Name:        "type",
				Type:        "string",
				Description: "Filter list results by kind. Only used by action=list. Omit to list both.",
				Enum:        []string{"conventions", "playbooks"},
			},
			{
				Name:        "category",
				Type:        "string",
				Description: "Server-side category filter (case-sensitive exact match against LibraryCategory.Name). Only used by action=list.",
			},
			{
				Name:        "title",
				Type:        "string",
				Description: "Library entry title to address. Required for action=get and action=activate. Exact match; conventions are checked first, then playbooks.",
			},
			{
				Name:        "full",
				Type:        "bool",
				Description: "When true, action=list returns full playbook bodies instead of the default summary. Conventions always carry full bodies (they're short). Use sparingly — agent context blows up fast. Default: false (summary mode).",
			},
		},
	},
	Actions: map[string]ActionFn{
		"list":     passThrough([]string{"library", "list"}),
		"get":      passThrough([]string{"library", "get"}),
		"activate": passThrough([]string{"library", "activate"}),
	},
}

const padLibraryToolDescription = `Convention + playbook library — global catalog of pre-built entries that workspaces activate into their own conventions/playbooks collections.

Actions:
  list     — Browse the library. By default playbooks come back as metadata + a short
             summary (first non-heading paragraph, ~240 chars); conventions always carry
             full bodies. Use action=get for the full body of one playbook.
             Optional: type (conventions|playbooks), category, full.
  get      — Full body of one entry by exact title. Conventions are checked first,
             then playbooks — same precedence activate uses, so a title resolves to the
             same kind in both surfaces.
             Required: title.
  activate — Create a workspace item from a library entry by title. Conventions land
             in the conventions collection, playbooks land in the playbooks collection,
             with all fields (trigger, scope, surfaces, enforcement, invocation_slug,
             arguments) carried through from the library definition.
             Required: workspace, title.

The library itself is workspace-agnostic — list/get don't need workspace context. The
workspace param is still accepted (and honored by activate) so a session pinned via
pad_set_workspace doesn't need to re-pass it on every call.

Use pad_library when an agent needs to discover available conventions/playbooks (notably
during the /pad onboard interview) or activate a curated entry into a workspace.`
