package mcp

// padProjectTool exposes project intelligence — computed views over
// the workspace state. All actions are read-only.

func init() {
	appendToCatalog(padProjectTool)
}

var padProjectTool = ToolDef{
	Name:        "pad_project",
	Description: padProjectToolDescription,
	Schema: ToolSchema{
		Workspace: true,
		Params: []ParamDef{
			{
				Name:        "days",
				Type:        "number",
				Description: "Lookback window in days. Optional for action=standup (default 1) and action=changelog (default 7).",
			},
			{
				Name:        "since",
				Type:        "string",
				Description: "ISO date (YYYY-MM-DD) — include entries on or after this date. Optional for action=changelog (mutually exclusive with `days`) and action=activity (applied server-side).",
			},
			{
				Name:        "parent",
				Type:        "string",
				Description: "Parent item ref (e.g. PLAN-2). Optional for action=changelog — scope the changelog to items under this parent.",
			},
			{
				Name:        "window",
				Type:        "string",
				Description: "Report window for action=report: day | week | 2wk | month (default week).",
			},
			{
				Name:        "collections",
				Type:        "string",
				Description: "Comma-separated collection slugs to include for action=report (default: all visible).",
			},
			{
				Name:        "actor",
				Type:        "string",
				Description: "Filter by actor category — `user` or `agent`. Optional for action=activity.",
			},
			{
				Name:        "limit",
				Type:        "number",
				Description: "Maximum number of entries to return. Optional for action=activity (default 20).",
			},
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
	},
}

const padProjectToolDescription = `Project intelligence — computed views over workspace state.

Actions:
  dashboard  — Project overview: collections, active items, plans, attention,
               blockers, suggested next.
               Required: workspace.
  next       — Recommended next item to work on (uses dashboard's suggestion logic).
               Required: workspace.
  ready      — Actionable backlog: items Pad considers ready to work on now.
               The query-oriented counterpart to next (reuses the same
               suggested-next logic, returns the full list).
               Required: workspace.
  stale      — Items needing attention — stalled, blocked, overdue, or
               otherwise falling out of the active workflow.
               Required: workspace.
  standup    — Daily standup report (recent done, in-progress, blockers).
               Required: workspace.
               Optional: days (default 1).
  changelog  — Changelog of completed items.
               Required: workspace.
               Optional: days (default 7), since (ISO date), parent (item ref to scope).
               Use parent=PLAN-N to generate a release-notes view for one plan.
  report     — Windowed project report: created-vs-completed throughput, net
               flow, completed-by-collection, current status distribution.
               Required: workspace.
               Optional: window (day|week|2wk|month, default week),
               collections (comma-separated slugs, default all visible).
  activity   — Recent workspace activity feed (non-streaming): what agents and
               users changed, with item refs, titles, and field-level change
               details. Use to catch up on what other agents did since you last
               worked.
               Required: workspace.
               Optional: limit (default 20), actor (user | agent),
               since (ISO date).

Use pad_project when an agent needs to summarize progress, plan next work,
catch up on recent activity, or generate retro / standup / changelog reports.`
