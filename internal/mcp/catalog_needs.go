package mcp

import "context"

// padNeedsTool is the caller's own needs-you inbox (TASK-3920) and its "not
// mine" triage (TASK-4643). Detail lives in docs/mcp.md "Needs you"; the
// descriptions stay short for the prompt budget.
func init() { appendToCatalog(padNeedsTool) }

var padNeedsTool = ToolDef{
	Name:        "pad_needs",
	Description: padNeedsToolDescription,
	Schema: ToolSchema{
		Workspace: true,
		Params: []ParamDef{
			{Name: "ref", Type: "string", Description: "Item ref (TASK-5)."},
			{Name: "view", Type: "string", Description: "list only.", Enum: []string{"active", "hidden"}},
			{Name: "to_role", Type: "string", Description: "Role slug; clears the person."},
			{Name: "to_user", Type: "string", Description: "Member name or email."},
		},
	},
	Actions: map[string]ActionFn{
		"list":     actionNeedsList,
		"not-mine": passThrough([]string{"needs", "not-mine"}),
		"restore":  passThrough([]string{"needs", "restore"}),
	},
}

// actionNeedsList narrows the cross-workspace inbox only to an explicit
// workspace: the session default names one workspace, the inbox spans all.
func actionNeedsList(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	if ws, _ := input["workspace"].(string); ws == "" {
		env.Workspace = NewWorkspaceState("")
	}
	return env.Dispatch(ctx, []string{"needs", "list"}, input)
}

const padNeedsToolDescription = `Your needs-you inbox across workspaces.

Actions:
  list — Items waiting on you. Optional: view, workspace (only an explicit one narrows).
  not-mine — Hide ref until it comes back to you (mention, waiting status, assignment, reminder).
    Required: ref. Optional: to_role OR to_user reassigns it first (seq-guarded; a refusal writes nothing).
  restore — Clear your not-mine and snooze. Required: ref.`
