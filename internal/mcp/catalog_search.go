package mcp

import (
	"context"
	"fmt"
	"strings"
)

// padSearchTool exposes full-text search across all items. Cross-
// workspace by design — the underlying CLI command (`pad item search`)
// queries an FTS index over every workspace the user can read.
//
// Why a separate tool instead of folding into pad_item: search is the
// one operation that doesn't fit pad_item's "operate on a single item
// or a single collection" mental model. Carving it out keeps pad_item's
// action enum focused.

func init() {
	appendToCatalog(padSearchTool)
}

var padSearchTool = ToolDef{
	Name:        "pad_search",
	Description: padSearchToolDescription,
	Schema: ToolSchema{
		Workspace: true,
		Params: []ParamDef{
			{
				Name:        "query",
				Type:        "string",
				Description: "Search query string. FTS syntax supported. Required.",
			},
			{
				Name:        "collection",
				Type:        "string",
				Description: "Restrict results to items in this collection slug (e.g. \"tasks\"). Optional.",
			},
			{
				Name:        "status",
				Type:        "string",
				Description: "Restrict results to items with this status. Optional.",
			},
			{
				Name:        "priority",
				Type:        "string",
				Description: "Restrict results to items with this priority. Optional.",
			},
			{
				Name:        "sort",
				Type:        "string",
				Description: "Sort order for results. Optional.",
			},
			{
				Name:        "limit",
				Type:        "number",
				Description: "Maximum results to return. Optional.",
			},
			{
				Name:        "offset",
				Type:        "number",
				Description: "Pagination offset. Optional.",
			},
			{
				Name:        "all_workspaces",
				Type:        "bool",
				Description: "Search every workspace the caller can read; refuses an explicit workspace. Optional.",
			},
		},
	},
	Actions: map[string]ActionFn{
		// `pad item search` is the underlying CLI command — search is
		// item-level FTS. The catalog exposes it under pad_search rather
		// than pad_item.action: search because the operation is logically
		// distinct (cross-workspace, scoring) and folding it would force
		// an unwieldy union schema on pad_item.
		"query": actionSearchQuery,
	},
}

// actionSearchQuery handles pad_search.action=query. With all_workspaces=true
// it refuses an explicit workspace, clears the session default so neither
// transport leaks it, and dispatches unscoped search (--all-workspaces on
// stdio; /api/v1/search with no workspace query key on HTTP).
func actionSearchQuery(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	allWorkspaces := false
	if v, present := input["all_workspaces"]; present && v != nil {
		b, ok := v.(bool)
		if !ok {
			return errStructured("pad_search.query", fmt.Errorf("all_workspaces must be a boolean, got %T", v)), nil
		}
		allWorkspaces = b
	}
	in := copyInputMap(input)
	if allWorkspaces {
		if v, present := input["workspace"]; present && v != nil {
			s, isStr := v.(string)
			if !isStr || strings.TrimSpace(s) != "" {
				return errStructured("pad_search.query", fmt.Errorf("all_workspaces cannot be combined with workspace")), nil
			}
		}
		in["all_workspaces"] = true
		delete(in, "workspace")
		env.Workspace = NewWorkspaceState("")
	} else {
		delete(in, "all_workspaces")
	}
	return env.Dispatch(ctx, []string{"item", "search"}, in)
}

const padSearchToolDescription = `Full-text search across items.

Actions:
  query  — FTS search across items. Required: query.
           Workspace: the session's unless all_workspaces=true (every readable workspace; refuses workspace).
           Optional filters: collection, status, priority, sort, limit, offset.

For a known ref (TASK-5), use pad_item.action=get.`
