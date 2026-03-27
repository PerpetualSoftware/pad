package collections

import "github.com/xarmian/pad/internal/models"

// DefaultCollection holds the definition for a default collection that gets
// created when a workspace is initialized.
type DefaultCollection struct {
	Name        string
	Slug        string
	Icon        string
	Description string
	Schema      models.CollectionSchema
	Settings    models.CollectionSettings
	SortOrder   int
}

// Defaults returns the six default collections for a new workspace.
func Defaults() []DefaultCollection {
	return []DefaultCollection{
		{
			Name:        "Tasks",
			Slug:        "tasks",
			Icon:        "✓",
			Description: "Track work items, bugs, and to-dos",
			SortOrder:   0,
			Schema: models.CollectionSchema{
				Fields: []models.FieldDef{
					{
						Key:      "status",
						Label:    "Status",
						Type:     "select",
						Options:  []string{"open", "in-progress", "done", "cancelled"},
						Default:  "open",
						Required: true,
					},
					{
						Key:     "priority",
						Label:   "Priority",
						Type:    "select",
						Options: []string{"low", "medium", "high", "critical"},
						Default: "medium",
					},
					{
						Key:   "assignee",
						Label: "Assignee",
						Type:  "text",
					},
					{
						Key:   "due_date",
						Label: "Due Date",
						Type:  "date",
					},
					{
						Key:     "effort",
						Label:   "Effort",
						Type:    "select",
						Options: []string{"xs", "s", "m", "l", "xl"},
					},
					{
						Key:        "phase",
						Label:      "Phase",
						Type:       "relation",
						Collection: "phases",
					},
				},
			},
			Settings: models.CollectionSettings{
				Layout:       "fields-primary",
				DefaultView:  "board",
				BoardGroupBy: "status",
				ListSortBy:   "priority",
				QuickActions: []models.QuickAction{
					{Label: "Implement this", Prompt: "/pad implement {ref} \"{title}\" (status: {status}, priority: {priority})", Scope: "item", Icon: "🔨"},
					{Label: "Write tests", Prompt: "/pad write tests for {ref} \"{title}\"", Scope: "item", Icon: "🧪"},
					{Label: "Explain this task", Prompt: "/pad explain {ref} \"{title}\" — what does it involve and why is it needed?", Scope: "item", Icon: "💬"},
					{Label: "Triage open tasks", Prompt: "/pad triage all open tasks and suggest priorities", Scope: "collection", Icon: "📋"},
					{Label: "Status report", Prompt: "/pad summarize progress on all tasks", Scope: "collection", Icon: "📊"},
				},
			},
		},
		{
			Name:        "Ideas",
			Slug:        "ideas",
			Icon:        "💡",
			Description: "Capture ideas, feature requests, and inspiration",
			SortOrder:   1,
			Schema: models.CollectionSchema{
				Fields: []models.FieldDef{
					{
						Key:      "status",
						Label:    "Status",
						Type:     "select",
						Options:  []string{"new", "exploring", "planned", "implemented", "rejected"},
						Default:  "new",
						Required: true,
					},
					{
						Key:     "impact",
						Label:   "Impact",
						Type:    "select",
						Options: []string{"low", "medium", "high"},
					},
					{
						Key:   "category",
						Label: "Category",
						Type:  "text",
					},
				},
			},
			Settings: models.CollectionSettings{
				Layout:      "balanced",
				DefaultView: "list",
				ListSortBy:  "created_at",
				ListGroupBy: "status",
				QuickActions: []models.QuickAction{
					{Label: "Explore this idea", Prompt: "/pad explore {ref} \"{title}\" — research feasibility, trade-offs, and implementation approaches", Scope: "item", Icon: "🔍"},
					{Label: "Break into tasks", Prompt: "/pad break down {ref} \"{title}\" into actionable tasks", Scope: "item", Icon: "📝"},
					{Label: "Research this", Prompt: "/pad research {ref} \"{title}\" and summarize findings", Scope: "item", Icon: "📚"},
					{Label: "Review all new ideas", Prompt: "/pad triage all new ideas and suggest which to pursue", Scope: "collection", Icon: "💡"},
				},
			},
		},
		{
			Name:        "Phases",
			Slug:        "phases",
			Icon:        "🏗️",
			Description: "Plan and track project phases and milestones",
			SortOrder:   2,
			Schema: models.CollectionSchema{
				Fields: []models.FieldDef{
					{
						Key:      "status",
						Label:    "Status",
						Type:     "select",
						Options:  []string{"planned", "active", "completed", "paused"},
						Default:  "planned",
						Required: true,
					},
					{
						Key:   "start_date",
						Label: "Start Date",
						Type:  "date",
					},
					{
						Key:   "end_date",
						Label: "End Date",
						Type:  "date",
					},
					{
						Key:      "progress",
						Label:    "Progress",
						Type:     "number",
						Suffix:   "%",
						Computed: true,
					},
				},
			},
			Settings: models.CollectionSettings{
				Layout:      "content-primary",
				DefaultView: "list",
				ListSortBy:  "sort_order",
				QuickActions: []models.QuickAction{
					{Label: "Plan this phase", Prompt: "/pad plan {ref} \"{title}\" — outline goals, deliverables, and timeline", Scope: "item", Icon: "📐"},
					{Label: "Break into tasks", Prompt: "/pad break {ref} \"{title}\" into PR-sized tasks", Scope: "item", Icon: "📝"},
					{Label: "Run a retro", Prompt: "/pad run a retrospective on {ref} \"{title}\"", Scope: "item", Icon: "🔄"},
					{Label: "Compare progress", Prompt: "/pad compare progress across all phases", Scope: "collection", Icon: "📊"},
				},
			},
		},
		{
			Name:        "Docs",
			Slug:        "docs",
			Icon:        "📄",
			Description: "Documentation, notes, and reference material",
			SortOrder:   3,
			Schema: models.CollectionSchema{
				Fields: []models.FieldDef{
					{
						Key:      "status",
						Label:    "Status",
						Type:     "select",
						Options:  []string{"draft", "published", "archived"},
						Default:  "draft",
						Required: true,
					},
					{
						Key:   "category",
						Label: "Category",
						Type:  "text",
					},
				},
			},
			Settings: models.CollectionSettings{
				Layout:      "content-primary",
				DefaultView: "list",
				ListSortBy:  "updated_at",
				ListGroupBy: "category",
				QuickActions: []models.QuickAction{
					{Label: "Review this doc", Prompt: "/pad review {ref} \"{title}\" for accuracy and completeness", Scope: "item", Icon: "👀"},
					{Label: "Update this doc", Prompt: "/pad update {ref} \"{title}\" to reflect the current state of the codebase", Scope: "item", Icon: "✏️"},
					{Label: "Summarize this", Prompt: "/pad summarize {ref} \"{title}\"", Scope: "item", Icon: "📋"},
					{Label: "Find outdated docs", Prompt: "/pad review all docs and identify which are outdated", Scope: "collection", Icon: "🔍"},
				},
			},
		},
		{
			Name:        "Conventions",
			Slug:        "conventions",
			Icon:        "📏",
			Description: "Project rules and conventions that guide agent behavior",
			SortOrder:   4,
			Schema: models.CollectionSchema{
				Fields: []models.FieldDef{
					{
						Key:      "status",
						Label:    "Status",
						Type:     "select",
						Options:  []string{"active", "draft", "disabled"},
						Default:  "active",
						Required: true,
					},
					{
						Key:     "trigger",
						Label:   "When",
						Type:    "select",
						Options: []string{"always", "on-task-start", "on-task-complete", "on-implement", "on-commit", "on-pr-create", "on-phase-start", "on-phase-complete", "on-plan"},
					},
					{
						Key:     "scope",
						Label:   "Scope",
						Type:    "select",
						Options: []string{"all", "backend", "frontend", "mobile", "docs", "devops"},
					},
					{
						Key:     "priority",
						Label:   "Priority",
						Type:    "select",
						Options: []string{"must", "should", "nice-to-have"},
					},
				},
			},
			Settings: models.CollectionSettings{
				Layout:      "balanced",
				DefaultView: "list",
				ListSortBy:  "trigger",
				ListGroupBy: "trigger",
			},
		},
		{
			Name:        "Playbooks",
			Slug:        "playbooks",
			Icon:        "📘",
			Description: "Multi-step workflows that agents follow for specific actions",
			SortOrder:   5,
			Schema: models.CollectionSchema{
				Fields: []models.FieldDef{
					{
						Key:      "status",
						Label:    "Status",
						Type:     "select",
						Options:  []string{"active", "draft", "deprecated"},
						Default:  "draft",
						Required: true,
					},
					{
						Key:     "trigger",
						Label:   "When",
						Type:    "select",
						Options: []string{"on-implement", "on-triage", "on-release", "on-plan", "on-review", "on-deploy", "manual"},
					},
					{
						Key:     "scope",
						Label:   "Scope",
						Type:    "select",
						Options: []string{"all", "backend", "frontend", "mobile", "devops"},
					},
				},
			},
			Settings: models.CollectionSettings{
				Layout:      "content-primary",
				DefaultView: "list",
				ListSortBy:  "updated_at",
				ListGroupBy: "trigger",
			},
		},
	}
}
