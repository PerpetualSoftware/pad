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
				Description: "ISO date (YYYY-MM-DD) — include changes on or after this date. Optional for action=changelog. Mutually exclusive with `days`.",
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
		},
	},
	Actions: map[string]ActionFn{
		"dashboard": passThrough([]string{"project", "dashboard"}),
		"next":      passThrough([]string{"project", "next"}),
		"standup":   passThrough([]string{"project", "standup"}),
		"changelog": passThrough([]string{"project", "changelog"}),
		"report":    passThrough([]string{"project", "report"}),
	},
}

const padProjectToolDescription = `Project intelligence — computed views over workspace state.

Actions:
  dashboard  — Project overview: collections, active items, plans, attention,
               blockers, suggested next.
               Required: workspace.
  next       — Recommended next item to work on (uses dashboard's suggestion logic).
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

Use pad_project when an agent needs to summarize progress, plan next work, or
generate retro / standup / changelog reports.`
