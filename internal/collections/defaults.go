package collections

import "github.com/PerpetualSoftware/pad/internal/models"

// DefaultCollection holds the definition for a default collection that gets
// created when a workspace is initialized.
type DefaultCollection struct {
	Name        string
	Slug        string
	Prefix      string // Optional explicit issue-ID prefix; empty means derive from Name
	Icon        string
	Description string
	Schema      models.CollectionSchema
	Settings    models.CollectionSettings
	// Traits declares the collection's kernel traits (SPEC-5). Templates
	// declare them so a NEW workspace gets them at seed time; existing
	// workspaces are backfilled by migration 080 / pg 058. Between the two,
	// no kernel behavior is inferred from a collection's slug any more.
	// TASK-2657.
	Traits    models.CollectionTraits
	SortOrder int
	IsSystem  bool // System collections (conventions, playbooks) are always visible to members
}

// Defaults returns the six default collections for a new workspace.
//
// Tasks and Ideas are extracted into tasksCollection/ideasCollection
// (templates.go, alongside docsCollection) so templates that want them
// without a Plans collection — e.g. `spec` (IDEA-2527), where
// implementation-plan material lives in the spec body instead — can compose
// the same seeded schema directly. Seeded output here is unchanged.
func Defaults() []DefaultCollection {
	return []DefaultCollection{
		tasksCollection(0),
		ideasCollection(1),
		{
			Name:        "Plans",
			Slug:        "plans",
			Icon:        "🗺️",
			Description: "Plan and track project plans and milestones",
			SortOrder:   2,
			Schema: models.CollectionSchema{
				Fields: []models.FieldDef{
					{
						Key:             "status",
						Label:           "Status",
						Type:            "select",
						Options:         []string{"planned", "active", "completed", "paused"},
						TerminalOptions: []string{"completed"},
						Default:         "planned",
						Required:        true,
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
				DefaultView: "board",
				ListSortBy:  "sort_order",
				QuickActions: []models.QuickAction{
					{Label: "Plan this", Prompt: "/pad plan {ref} \"{title}\" — outline goals, deliverables, and timeline", Scope: "item", Icon: "📐"},
					{Label: "Break into tasks", Prompt: "/pad break {ref} \"{title}\" into PR-sized tasks", Scope: "item", Icon: "📝"},
					{Label: "Run a retro", Prompt: "/pad run a retrospective on {ref} \"{title}\"", Scope: "item", Icon: "🔄"},
					{Label: "Compare progress", Prompt: "/pad compare progress across all plans", Scope: "collection", Icon: "📊"},
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
						Key:             "status",
						Label:           "Status",
						Type:            "select",
						Options:         []string{"draft", "published", "archived"},
						TerminalOptions: []string{"archived"},
						Default:         "draft",
						Required:        true,
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
				DefaultView: "board",
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
		conventionsCollection(4, SoftwareConventionTriggers, SoftwareConventionScopes),
		playbooksCollection(5, SoftwarePlaybookTriggers, SoftwarePlaybookScopes),
	}
}
