package mcp

// padCollectionTool exposes collection management. Four actions:
// list (read-only), create (admin-mutating), update (admin-mutating),
// delete (admin-mutating).
//
// `update` (TASK-1510) and `delete` (TASK-1511) are the adaptation
// primitives for the `/pad onboard` playbook (PLAN-1496): the agent
// rewrites OR removes seeded collections during onboarding so the
// workspace shape matches the project's actual vocabulary instead of
// the template defaults. Server-side handlers at handlers_collections.go
// require the workspace owner role; viewers/editors can't mutate.
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
				Name:        "slug",
				Type:        "string",
				Description: "Collection slug (e.g. \"tasks\", \"conventions\"). Required for action=update and action=delete; identifies which collection to mutate.",
			},
			{
				Name:        "name",
				Type:        "string",
				Description: "Display name of the collection. Required for action=create. Optional for action=update — provide to rename.",
			},
			{
				Name:        "fields",
				Type:        "string",
				Description: "Compact field DSL: \"key:type[:options]; ...\". Optional for action=create. Example: \"status:select:open,done; priority:select:high,medium,low\". Use `schema` instead when you need terminal_options, custom defaults, computed fields, suffixes, or relation collections — the DSL cannot express those. Mutually exclusive with `schema`.",
			},
			{
				Name:        "schema",
				Type:        "object",
				Description: "Structured CollectionSchema: {\"fields\":[{\"key\":\"...\",\"label\":\"...\",\"type\":\"...\",\"options\":[...],\"terminal_options\":[...],\"default\":\"...\",\"required\":bool,\"computed\":bool,\"suffix\":\"...\",\"collection\":\"...\"}]}. Optional for action=create. Use instead of `fields` when you need terminal_options or any FieldDef property the DSL cannot express. Missing `label` values are auto-filled from `key` using Title Case (e.g. due_date → Due Date). Mutually exclusive with `fields`.",
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
			{
				Name:        "prefix",
				Type:        "string",
				Description: "Issue-ID prefix (e.g. \"TASK\", \"BUG\"). Optional for action=update — provide to change how new items in this collection are numbered.",
			},
			{
				Name:        "sort_order",
				Type:        "number",
				Description: "Display sort order (lower = appears first). Optional for action=update.",
			},
		},
	},
	Actions: map[string]ActionFn{
		"list":   passThrough([]string{"collection", "list"}),
		"create": passThrough([]string{"collection", "create"}),
		"update": passThrough([]string{"collection", "update"}),
		"delete": passThrough([]string{"collection", "delete"}),
	},
}

const padCollectionToolDescription = `Collection management — list, create, update, and delete collection types.

Actions:
  list    — List collections in the workspace with their schemas + counts.
            Required: workspace.
  create  — Create a new collection.
            Required: workspace, name.
            Optional: fields OR schema (mutually exclusive), icon, description,
                      layout, default_view, board_group_by.
            DSL example:    fields="status:select:open,done; priority:select:high,medium,low"
            Schema example: schema={"fields":[{"key":"status","type":"select","options":["new","done"],"terminal_options":["done"]}]}
            Prefer schema when terminal_options matters (driving dashboard
            active/complete counts) or when fields need defaults, computed
            flags, suffixes, or relation collections — the DSL cannot express
            those.
  update  — Update an existing collection's name, icon, description, prefix,
            schema, or sort order. Workspace owner only.
            Required: workspace, slug.
            Optional: name, icon, description, prefix, fields OR schema
                      (mutually exclusive), sort_order. Only fields you
                      explicitly set are mutated; everything else is left
                      untouched.
            This is the adaptation primitive for the onboarding playbook
            (/pad onboard) — rewrite seeded collections to match the
            project's actual vocabulary instead of template defaults.
  delete  — Soft-delete a collection. Owner-only. No restore endpoint
            exists — recovery requires a database backup. Required:
            workspace, slug.
            Constraints:
              - Cannot delete a default (template-seeded) collection.
                Adapt those via update instead (rename, reshape schema).
              - Items in the collection are NOT cascaded — they remain
                in the database with the soft-deleted collection_id.
                The web UI hides them; raw API queries still surface
                them.

Schema for an individual collection is included in the list response — read it
from there rather than calling list again. v0.2 does not expose a dedicated
read-by-slug action; if you need that, file a feature request.

Use pad_collection when an agent needs to discover collection types or set up
a new one. For item-level CRUD inside a collection, use pad_item.`
