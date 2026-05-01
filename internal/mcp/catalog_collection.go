package mcp

// padCollectionTool exposes collection management. Two actions in
// v0.2: list (read-only) and create (admin-mutating).
//
// `schema` was discussed in DOC-978 but dropped from v0.2 — the CLI
// has no `collection schema` command, and consumers can read each
// collection's schema field from the `list` response. If dogfooding
// (TASK-975) shows demand for a dedicated read-by-slug action, add
// it as a follow-up rather than synthesizing one inside the catalog.

func init() {
	appendToCatalog(padCollectionTool)
}

var padCollectionTool = ToolDef{
	Name:        "pad_collection",
	Description: padCollectionToolDescription,
	Schema: ToolSchema{
		Workspace: true,
		Params: []ParamDef{
			{
				Name:        "name",
				Type:        "string",
				Description: "Display name of the collection. Required for action=create.",
			},
			{
				Name:        "fields",
				Type:        "string",
				Description: "Field DSL: \"key:type[:options]; ...\". Optional for action=create. Example: \"status:select:open,done; priority:select:high,medium,low\".",
			},
			{
				Name:        "icon",
				Type:        "string",
				Description: "Emoji icon for the collection (e.g. \"🐛\"). Optional for action=create.",
			},
			{
				Name:        "description",
				Type:        "string",
				Description: "Plain-text description of the collection's purpose. Optional for action=create.",
			},
			{
				Name:        "layout",
				Type:        "string",
				Description: "UI layout hint. Optional for action=create.",
				Enum:        []string{"fields-primary", "content-primary", "balanced"},
			},
			{
				Name:        "default_view",
				Type:        "string",
				Description: "Default view mode. Optional for action=create.",
				Enum:        []string{"list", "board", "table"},
			},
			{
				Name:        "board_group_by",
				Type:        "string",
				Description: "Field key to group by when in board view. Optional for action=create. Defaults to \"status\".",
			},
		},
	},
	Actions: map[string]ActionFn{
		"list":   passThrough([]string{"collection", "list"}),
		"create": passThrough([]string{"collection", "create"}),
	},
}

const padCollectionToolDescription = `Collection management — list and create collection types.

Actions:
  list    — List collections in the workspace with their schemas + counts.
            Required: workspace.
  create  — Create a new collection.
            Required: workspace, name.
            Optional: fields, icon, description, layout, default_view, board_group_by.
            Field DSL example: "status:select:open,done; priority:select:high,medium,low".

Schema for an individual collection is included in the list response — read it
from there rather than calling list again. v0.2 does not expose a dedicated
read-by-slug action; if you need that, file a feature request.

Use pad_collection when an agent needs to discover collection types or set up
a new one. For item-level CRUD inside a collection, use pad_item.`
