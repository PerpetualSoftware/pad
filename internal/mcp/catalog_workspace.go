package mcp

import (
	"context"
)

// padWorkspaceTool exposes workspace-level operations: discovery
// (list), membership (members, invite), storage usage, audit log,
// lifecycle (create, claim), and soft-delete recovery (deleted,
// restore).
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
			// --- TASK-1521: workspace lifecycle ---
			{
				Name:        "name",
				Type:        "string",
				Description: "Human-readable workspace name. Required for action=create; optional for action=rename.",
			},
			{
				Name:        "slug",
				Type:        "string",
				Description: "Workspace slug (kebab-case, globally unique). Optional for action=create (derived from name when omitted); required for action=restore (names the soft-deleted workspace to restore).",
			},
			{
				Name:        "new_slug",
				Type:        "string",
				Description: "New canonical slug. Required for action=rename.",
			},
			{
				Name:        "template",
				Type:        "string",
				Description: "Template to seed collections from (e.g. \"startup\", \"scrum\", \"blank\"). Optional for action=create.",
			},
			{
				Name:        "code",
				Type:        "string",
				Description: "6-digit claim code generated in the workspace's \"Connect project\" modal. Required for action=claim.",
			},
		},
	},
	Actions: map[string]ActionFn{
		"list":      passThrough([]string{"workspace", "list"}),
		"members":   passThrough([]string{"workspace", "members"}),
		"invite":    passThrough([]string{"workspace", "invite"}),
		"storage":   passThrough([]string{"workspace", "storage"}),
		"audit-log": actionWorkspaceAuditLog,
		// PLAN-1519 / TASK-1521 / IDEA-1517 §1 + §4.
		"create": passThrough([]string{"workspace", "create"}),
		"claim":  passThrough([]string{"workspace", "claim"}),
		// TASK-1973: workspace soft-delete recovery (mirrors the CLI
		// `pad workspace deleted` / `pad workspace restore` from TASK-1972).
		// Both non-interactive; restore is mutating but non-destructive.
		"deleted": passThrough([]string{"workspace", "deleted"}),
		"restore": passThrough([]string{"workspace", "restore"}),
		"rename":  actionWorkspaceRename,
	},
}

func actionWorkspaceRename(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	if workspace, _ := input["workspace"].(string); workspace == "" {
		return validationFailedFromBuildErr("workspace rename", errWorkspaceRequired), nil
	}
	return env.Dispatch(ctx, []string{"workspace", "rename"}, input)
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
func actionWorkspaceAuditLog(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
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

const padWorkspaceToolDescription = `Workspace operations — discovery, membership, storage, audit log, lifecycle.

Actions:
  list       — List visible workspaces (admins see all). No params.
  members    — List members and pending invitations. Required: workspace.
  invite     — Invite user. Required: workspace, email. Optional: role (owner|editor|viewer; default editor).
  storage    — Storage usage breakdown. Required: workspace.
  audit-log  — Cross-workspace audit log (ADMIN ONLY). Optional: action_filter, actor, days, limit.
  create     — Create workspace. Required: name. Optional: slug, template (startup/scrum/blank).
               OAuth may_create_workspaces=true auto-adds it; false refuses 403 (re-auth or /settings/connected-apps).
  claim      — Redeem 6-digit claim code (web UI "Connect project", 5–10m TTL) into OAuth allow-list.
               Required: workspace, code. Errors: 401 invalid_code, 404 not_found, 412 connection_not_persisted.
  deleted    — List soft-deleted workspaces in 30-day restore window. No params.
  restore    — Un-soft-delete workspace (owner-only). Required: slug.
  rename     — Rename workspace slug in place (owner-only; old links keep working). Required: workspace, new_slug. Optional: name.`
