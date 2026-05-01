package mcp

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
		},
	},
	Actions: map[string]ActionFn{
		// `pad item search` is the underlying CLI command — search is
		// item-level FTS. The catalog exposes it under pad_search rather
		// than pad_item.action: search because the operation is logically
		// distinct (cross-workspace, scoring) and folding it would force
		// an unwieldy union schema on pad_item.
		"query": passThrough([]string{"item", "search"}),
	},
}

const padSearchToolDescription = `Full-text search across items.

Actions:
  query  — FTS search across items. Required: workspace, query.
           Optional filters: collection, status, priority, sort, limit, offset.

Use pad_search when looking for items by content keywords or fuzzy title match.
For ref-based lookup (TASK-5, IDEA-12), use pad_item.action=get directly — it's
faster and doesn't require a query string.`
