package mcp

// padCollectionTool exposes collection management. Six actions:
// list and list-archived (read-only), create, update, delete (archive) and
// restore (admin-mutating). restore and list-archived are TASK-2189: an
// agent's archive is recoverable by an agent.
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
				Description: "Collection slug (e.g. tasks, conventions). Required: update/delete/restore. Restore: archived slug from list-archived or id.",
			},
			{
				Name:        "name",
				Type:        "string",
				Description: "Collection display name. Required: create; update renames.",
			},
			{
				Name:        "fields",
				Type:        "string",
				Description: "Field DSL: \"key:type[:options]; ...\", e.g. \"status:select:open,done; priority:select:high,medium,low\". For create. relation part 3 is required TARGET COLLECTION, not options: \"reviewer:relation:people\"; no target refuses. For terminal_options, defaults, computed fields or suffixes use schema; DSL cannot express them. Mutually exclusive with schema.",
			},
			{
				Name:        "schema",
				Type:        "object",
				Description: "CollectionSchema: {\"fields\":[{\"key\":\"...\",\"label\":\"...\",\"type\":\"...\",\"options\":[...],\"terminal_options\":[...],\"default\":\"...\",\"required\":bool,\"computed\":bool,\"suffix\":\"...\",\"collection\":\"...\"}]}. For create. Use for terminal_options or other FieldDef properties absent from DSL. Missing label: Title Case of key (due_date → Due Date). Mutually exclusive with fields.",
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
				Description: "Issue prefix (e.g. TASK, BUG). update changes numbering of new items.",
			},
			{
				Name:        "sort_order",
				Type:        "number",
				Description: "Display sort order (lower = appears first). Optional for action=update.",
			},
			{
				Name:        "tracks_work",
				Type:        "boolean",
				Description: "false: reference, not work.",
			},
		},
	},
	Actions: map[string]ActionFn{
		"list":   passThrough([]string{"collection", "list"}),
		"create": passThrough([]string{"collection", "create"}),
		"update": passThrough([]string{"collection", "update"}),
		"delete": passThrough([]string{"collection", "delete"}),
		// TASK-2189: an agent's archive is recoverable by an agent.
		"list-archived": passThrough([]string{"collection", "archived"}),
		"restore":       passThrough([]string{"collection", "restore"}),
	},
}

const padCollectionToolDescription = `Manage collection types.

Actions:
  list — Schemas + counts. Required: workspace. Read schemas here; no read-by-slug action.
  create — Required: workspace, name. Optional: fields OR schema (mutually exclusive),
    icon, description, layout, default_view, board_group_by.
    DSL: fields="status:select:open,done; priority:select:high,medium,low".
    Relations require a target collection slug in part 3, not options:
    fields="reviewer:relation:people"; no target refuses (cannot accept values).
    Schema: {"fields":[{"key":"status","type":"select","options":["new","done"],"terminal_options":["done"]}]}.
    Use schema for terminal_options (dashboard active/complete counts), defaults,
    computed flags or suffixes; the DSL cannot express them.
  update — Owner-only. Required: workspace, slug. Optional: name, icon, description,
    prefix, fields OR schema (mutually exclusive), sort_order. Only supplied fields change.
    Adapt seeded collections to project vocabulary with /pad onboard.
    Removed fields/options keep item values; warnings.orphaned counts affected items.
  delete — Owner-only archive; collection and items leave every view. Required: workspace, slug.
    Items stay stored, hidden until restore. Default (template-seeded) collections
    cannot be deleted: rename/reshape them via update. Reversible with restore.
  list-archived — Owner-only. Required: workspace. Returns slug, id, prefix, item_count, archived_at.
  restore — Owner-only; restores collection and items. Required: workspace, slug
    (archived slug or id from list-archived).`
