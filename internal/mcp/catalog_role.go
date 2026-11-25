package mcp

// padRoleTool exposes agent-role management. Roles let users organize
// items by what kind of thinking is required (Planner / Implementer /
// Reviewer). One per (user, role) assignment on items.
//
// `update` (TASK-1512) is the role-side adaptation primitive for the
// `/pad onboard` playbook (PLAN-1496): the agent rewrites seeded role
// descriptions and icons during onboarding so they match how the
// user's team actually splits work. Deleting and re-creating loses
// the agent_role_id references on items already assigned to the role,
// so update is the only safe way to adapt.

func init() {
	appendToCatalog(padRoleTool)
}

var padRoleTool = ToolDef{
	Name:        "pad_role",
	Description: padRoleToolDescription,
	Schema: ToolSchema{
		Workspace: true,
		Params: []ParamDef{
			{
				Name:        "name",
				Type:        "string",
				Description: "Display name (e.g. \"Implementer\"). Required: create; optional: update.",
			},
			{
				Name:        "slug",
				Type:        "string",
				Description: "Role slug or UUID. Required: update, delete, park, unpark. Rename via new_slug.",
			},
			{
				Name:        "new_slug",
				Type:        "string",
				Description: "New slug when renaming. Optional: update.",
			},
			{
				Name:        "description",
				Type:        "string",
				Description: "Role description. Optional: create, update.",
			},
			{
				Name:        "icon",
				Type:        "string",
				Description: "Emoji icon. Optional: create, update.",
			},
			{
				Name:        "tools",
				Type:        "string",
				Description: "Comma-separated tools. Optional: create, update.",
			},
			{
				Name:        "sort_order",
				Type:        "number",
				Description: "Sort order (lower first). Optional: update.",
			},
			{
				Name:        "before",
				Type:        "string",
				Description: "Queue item ref/ID to park before (park only; excludes after; omit both for top).",
			},
			{
				Name:        "after",
				Type:        "string",
				Description: "Queue item ref/ID to park after (park only; excludes before).",
			},
			{
				Name:        "expected_park_seq",
				Type:        "number",
				Description: "Optimistic-concurrency token; stale returns park_conflict. Optional: park, unpark.",
			},
		},
	},
	Actions: map[string]ActionFn{
		"list":   passThrough([]string{"role", "list"}),
		"create": passThrough([]string{"role", "create"}),
		"update": passThrough([]string{"role", "update"}),
		"delete": passThrough([]string{"role", "delete"}),
		"park":   passThrough([]string{"role", "park"}),
		"unpark": passThrough([]string{"role", "unpark"}),
	},
}

const padRoleToolDescription = `Agent role management.

Actions:
  list    — List workspace roles. Required: workspace.
  create  — Create a role. Required: workspace, name. Optional: description, icon, tools.
  update  — Update supplied fields only. Required: workspace, slug.
            Optional: name, description, icon, tools, sort_order, new_slug.
  delete  — Delete a role. Required: workspace, slug.
  park    — Set/move park line. Required: workspace, slug.
            Optional: before, after (mutually exclusive; omit both for top), expected_park_seq.
  unpark  — Clear park line (idempotent). Required: workspace, slug. Optional: expected_park_seq.

Assign an item a role with pad_item.action=update role=<slug>.`
