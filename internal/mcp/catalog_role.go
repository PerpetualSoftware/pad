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
				Description: "Display name of the role (e.g. \"Implementer\"). Required for action=create. Optional for action=update — provide to rename.",
			},
			{
				Name:        "slug",
				Type:        "string",
				Description: "Role slug (e.g. \"implementer\") or UUID. Required for action=update and action=delete; identifies which role to mutate. To RENAME a role, also pass new_slug.",
			},
			{
				Name:        "new_slug",
				Type:        "string",
				Description: "Replacement slug when renaming an existing role. Optional for action=update; ignored otherwise.",
			},
			{
				Name:        "description",
				Type:        "string",
				Description: "What kind of work this role does. Optional for action=create and action=update.",
			},
			{
				Name:        "icon",
				Type:        "string",
				Description: "Emoji icon for the role (e.g. \"🔨\"). Optional for action=create and action=update.",
			},
			{
				Name:        "tools",
				Type:        "string",
				Description: "Comma-separated list of tools/integrations the role uses. Optional for action=create and action=update.",
			},
			{
				Name:        "sort_order",
				Type:        "number",
				Description: "Display sort order (lower = appears first). Optional for action=update.",
			},
		},
	},
	Actions: map[string]ActionFn{
		"list":   passThrough([]string{"role", "list"}),
		"create": passThrough([]string{"role", "create"}),
		"update": passThrough([]string{"role", "update"}),
		"delete": passThrough([]string{"role", "delete"}),
	},
}

const padRoleToolDescription = `Agent role management — what kind of thinking each item requires.

Actions:
  list    — List roles in the workspace.
            Required: workspace.
  create  — Create a new role.
            Required: workspace, name.
            Optional: description, icon, tools.
  update  — Update an existing role's name, slug, description, icon, tools,
            or sort_order. Only fields you explicitly set are mutated.
            Required: workspace, slug.
            Optional: name, description, icon, tools, sort_order, new_slug
                      (the rename target; pass slug for lookup AND
                      new_slug for the replacement value).
            This is the adaptation primitive for the /pad onboard playbook
            (PLAN-1496): rewriting seeded role descriptions/icons to match
            the team's actual division of work.
  delete  — Delete a role by slug.
            Required: workspace, slug.

Roles let users organize items by what kind of work they require (Planner,
Implementer, Reviewer, Researcher, etc.). To assign an item to a role, use
pad_item.action=update with the role parameter.`
