package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
)

// padWorkspaceTool exposes workspace-level operations: discovery
// (list), membership (members, invite), storage usage, and audit log.
//
// All actions dispatch through the existing CLI/HTTP route table.
// Workspace context: this tool operates ON workspaces, so the
// `workspace` parameter targets a specific workspace for the action
// (e.g. members of which workspace) rather than picking the session
// default. Where the action is server-wide (`list`), the param is
// ignored.

func init() {
	appendToCatalog(padWorkspaceTool)
}

var padWorkspaceTool = ToolDef{
	Name:        "pad_workspace",
	Description: padWorkspaceToolDescription,
	Schema: ToolSchema{
		Workspace: true,
		Params: []ParamDef{
			{
				Name:        "email",
				Type:        "string",
				Description: "Email address to invite. Required for action=invite.",
			},
			{
				Name:        "role",
				Type:        "string",
				Description: "Role to grant on invite (owner | editor | viewer). Optional for action=invite; defaults to editor.",
				Enum:        []string{"owner", "editor", "viewer"},
			},
			{
				Name:        "action_filter",
				Type:        "string",
				Description: "Filter audit-log entries by action name (e.g. \"item.created\"). Optional for action=audit-log.",
			},
			{
				Name:        "actor",
				Type:        "string",
				Description: "Filter audit-log entries by actor (user name or \"agent\"). Optional for action=audit-log.",
			},
			{
				Name:        "days",
				Type:        "number",
				Description: "Limit audit-log entries to the last N days. Optional for action=audit-log.",
			},
			{
				Name:        "limit",
				Type:        "number",
				Description: "Maximum entries to return. Optional for action=audit-log.",
			},
		},
	},
	Actions: map[string]ActionFn{
		"list":      passThrough([]string{"workspace", "list"}),
		"members":   passThrough([]string{"workspace", "members"}),
		"invite":    passThrough([]string{"workspace", "invite"}),
		"storage":   passThrough([]string{"workspace", "storage"}),
		"audit-log": actionWorkspaceAuditLog,
	},
}

// actionWorkspaceAuditLog reshapes the input for `pad workspace
// audit-log`'s `--action` filter flag. The CLI flag name `action`
// would normally be reachable via passThrough — but the catalog's
// fan-out routing field is also named `action` (and is stripped by
// makeFanOutHandler before reaching the handler). To preserve user-
// supplied `--action` filtering without forcing every action handler
// to know about the routing-key stripping, we expose the filter as
// `action_filter` in the schema and rename here before dispatch.
//
// All other audit-log flags (actor, days, limit) flow through unchanged.
func actionWorkspaceAuditLog(ctx context.Context, input map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
	if v, ok := input["action_filter"]; ok {
		// Clone so the caller's map (and req.Params.Arguments) stays
		// untouched. Same defensive copy makeFanOutHandler does.
		out := make(map[string]any, len(input))
		for k, val := range input {
			if k == "action_filter" {
				continue
			}
			out[k] = val
		}
		// Re-inject under the CLI flag name. By this point the catalog's
		// `action` routing key has already been stripped by makeFanOutHandler,
		// so writing `action` here goes directly to the CLI's --action flag
		// via BuildCLIArgs.
		out["action"] = v
		input = out
	}
	return env.Dispatch(ctx, []string{"workspace", "audit-log"}, input)
}

const padWorkspaceToolDescription = `Workspace operations — discovery, membership, storage, and audit log.

Actions:
  list       — List workspaces visible to the current user. No params; admins see all,
               regular users see their memberships.
  members    — List members + pending invitations for a workspace.
               Required: workspace.
  invite     — Invite a user to a workspace.
               Required: workspace, email.
               Optional: role (owner | editor | viewer; default editor).
  storage    — Workspace storage usage breakdown (attachments, items, total bytes).
               Required: workspace.
  audit-log  — Workspace audit log. ADMIN ONLY — non-admin users get an auth error.
               Optional: action_filter, actor, days, limit.
               Note: the underlying endpoint is global, so this returns entries
               across all workspaces filtered by the supplied params.

Use pad_workspace when an agent needs to discover, manage membership, or audit
workspace activity. For item-level operations use pad_item.`
