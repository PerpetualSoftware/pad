package mcp

// padRoleTool exposes agent-role management. Roles let users organize
// items by what kind of thinking is required (Planner / Implementer /
// Reviewer). One per (user, role) assignment on items.

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
				Description: "Display name of the role (e.g. \"Implementer\"). Required for action=create.",
			},
			{
				Name:        "slug",
				Type:        "string",
				Description: "Role slug (e.g. \"implementer\"). Required for action=delete.",
			},
			{
				Name:        "description",
				Type:        "string",
				Description: "What kind of work this role does. Optional for action=create.",
			},
			{
				Name:        "icon",
				Type:        "string",
				Description: "Emoji icon for the role (e.g. \"🔨\"). Optional for action=create.",
			},
			{
				Name:        "tools",
				Type:        "string",
				Description: "Comma-separated list of tools/integrations the role uses. Optional for action=create.",
			},
		},
	},
	Actions: map[string]ActionFn{
		"list":   passThrough([]string{"role", "list"}),
		"create": passThrough([]string{"role", "create"}),
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
  delete  — Delete a role by slug.
            Required: workspace, slug.

Roles let users organize items by what kind of work they require (Planner,
Implementer, Reviewer, Researcher, etc.). To assign an item to a role, use
pad_item.action=update with the role parameter.`
