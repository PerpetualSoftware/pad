package mcp

import (
	"context"
	"fmt"
)

// padProjectTool exposes project intelligence — computed views over
// the workspace state. All actions are read-only.

func init() {
	for name, fn := range padProjectTool.Actions {
		if name == "ready" || name == "next" {
			continue
		}
		padProjectTool.Actions[name] = rejectProjectRoleParam("pad_project."+name, fn)
	}
	appendToCatalog(padProjectTool)
}

func rejectProjectRoleParam(prefix string, fn ActionFn) ActionFn {
	return func(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
		if _, present := input["role"]; present {
			return errStructured(prefix, fmt.Errorf("role is only accepted for action=ready and action=next")), nil
		}
		return fn(ctx, input, env)
	}
}

var padProjectTool = ToolDef{
	Name:        "pad_project",
	Description: padProjectToolDescription,
	Schema: ToolSchema{
		Workspace: true,
		Params: []ParamDef{
			{Name: "role", Type: "string", Description: "Agent role slug (ready/next only; reads role queue; refused elsewhere)."},
			{Name: "worker_id", Type: "string", Description: "Lease worker id for next/ready (skips foreign leases)."},
			{Name: "days", Type: "number", Description: "Lookback days: standup (default 1), changelog (default 7)."},
			{Name: "since", Type: "string", Description: "ISO date YYYY-MM-DD: changelog (excludes days) or activity."},
			{Name: "parent", Type: "string", Description: "Parent ref (e.g. PLAN-2) to scope changelog."},
			{Name: "window", Type: "string", Description: "Report window: day | week | 2wk | month (default week)."},
			{Name: "collections", Type: "string", Description: "Comma-separated collection slugs for report (default visible non-system)."},
			{Name: "actor", Type: "string", Description: "Filter activity by actor: user | agent."},
			{Name: "limit", Type: "number", Description: "Max activity entries (default 20, max 200)."},
		},
	},
	Actions: map[string]ActionFn{
		"dashboard": passThrough([]string{"project", "dashboard"}),
		"next":      passThrough([]string{"project", "next"}),
		"ready":     passThrough([]string{"project", "ready"}),
		"stale":     passThrough([]string{"project", "stale"}),
		"standup":   passThrough([]string{"project", "standup"}),
		"changelog": passThrough([]string{"project", "changelog"}),
		"report":    passThrough([]string{"project", "report"}),
		"activity":  passThrough([]string{"project", "activity"}),
		"catch-up":  passThrough([]string{"project", "catch-up"}),
	},
}

const padProjectToolDescription = `Project intelligence — computed views over workspace state.

Actions (all require workspace):
  dashboard  — Overview: collections, active items, plans, attention, blockers, suggested next.
  next       — Top recommended items (same rank as ready). Optional: role (role queue, max 3 ready).
  ready      — Ranked actionable items (blocked and parked excluded; see stale). Optional: role (role queue with now/park).
  stale      — Stalled, blocked, overdue, or orphaned items.
  standup    — Daily standup (done, in-progress, blockers). Optional: days (default 1).
  changelog  — Completed items. Optional: days (default 7), since, parent.
  report     — Throughput, net flow, status distribution. Optional: window (day|week|2wk|month), collections.
  activity   — Recent activity feed. Optional: limit (default 20), actor (user|agent), since.
  catch-up   — Personal digest since last caught up (max 14d). Read-only; never marks caught up.`
