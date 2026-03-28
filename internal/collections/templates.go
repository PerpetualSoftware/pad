package collections

import "github.com/xarmian/pad/internal/models"

// WorkspaceTemplate is a named set of collection definitions used to
// initialize a new workspace.
type WorkspaceTemplate struct {
	Name        string
	Description string
	Collections []DefaultCollection
	SeedItems   []SeedItem // Optional sample items to create after collections
}

// SeedItem defines a sample item to seed into a workspace.
type SeedItem struct {
	CollectionSlug string // Which collection to add this to
	Title          string
	Content        string
	Fields         string // JSON string of field values
}

// docsCollection returns the standard Docs collection shared across templates.
func docsCollection(sortOrder int) DefaultCollection {
	return DefaultCollection{
		Name:        "Docs",
		Slug:        "docs",
		Icon:        "\U0001F4C4",
		Description: "Documentation, notes, and reference material",
		SortOrder:   sortOrder,
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
		},
	}
}

// conventionsCollection returns the standard Conventions collection shared across templates.
func conventionsCollection(sortOrder int) DefaultCollection {
	return DefaultCollection{
		Name:        "Conventions",
		Slug:        "conventions",
		Icon:        "\U0001F4CF",
		Description: "Project rules and conventions that guide agent behavior",
		SortOrder:   sortOrder,
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
	}
}

// playbooksCollection returns the standard Playbooks collection shared across templates.
func playbooksCollection(sortOrder int) DefaultCollection {
	return DefaultCollection{
		Name:        "Playbooks",
		Slug:        "playbooks",
		Icon:        "\U0001F4D8",
		Description: "Multi-step workflows that agents follow for specific actions",
		SortOrder:   sortOrder,
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
	}
}

// templates holds all registered workspace templates.
var templates = []WorkspaceTemplate{
	{
		Name:        "startup",
		Description: "Tasks, Ideas, Phases, Docs, Conventions, Playbooks",
		Collections: Defaults(),
	},
	{
		Name:        "scrum",
		Description: "Backlog, Sprints, Bugs, Docs, Conventions, Playbooks",
		Collections: []DefaultCollection{
			{
				Name:        "Backlog",
				Slug:        "backlog",
				Icon:        "\U0001F4CB",
				Description: "Product backlog items for sprint planning",
				SortOrder:   0,
				Schema: models.CollectionSchema{
					Fields: []models.FieldDef{
						{
							Key:      "status",
							Label:    "Status",
							Type:     "select",
							Options:  []string{"new", "ready", "in_sprint", "done"},
							Default:  "new",
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
							Key:   "points",
							Label: "Points",
							Type:  "number",
						},
						{
							Key:   "sprint",
							Label: "Sprint",
							Type:  "text",
						},
					},
				},
				Settings: models.CollectionSettings{
					Layout:       "fields-primary",
					DefaultView:  "board",
					BoardGroupBy: "status",
					ListSortBy:   "priority",
				},
			},
			{
				Name:        "Sprints",
				Slug:        "sprints",
				Icon:        "\U0001F3C3",
				Description: "Sprint cycles with goals and timelines",
				SortOrder:   1,
				Schema: models.CollectionSchema{
					Fields: []models.FieldDef{
						{
							Key:      "status",
							Label:    "Status",
							Type:     "select",
							Options:  []string{"planning", "active", "completed"},
							Default:  "planning",
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
							Key:   "goal",
							Label: "Goal",
							Type:  "text",
						},
					},
				},
				Settings: models.CollectionSettings{
					Layout:       "balanced",
					DefaultView:  "board",
					BoardGroupBy: "status",
					ListSortBy:   "start_date",
				},
			},
			{
				Name:        "Bugs",
				Slug:        "bugs",
				Icon:        "\U0001F41B",
				Description: "Track and triage bugs and defects",
				SortOrder:   2,
				Schema: models.CollectionSchema{
					Fields: []models.FieldDef{
						{
							Key:      "status",
							Label:    "Status",
							Type:     "select",
							Options:  []string{"new", "triaged", "fixing", "resolved", "wontfix"},
							Default:  "new",
							Required: true,
						},
						{
							Key:     "severity",
							Label:   "Severity",
							Type:    "select",
							Options: []string{"low", "medium", "high", "critical"},
							Default: "medium",
						},
						{
							Key:   "component",
							Label: "Component",
							Type:  "text",
						},
					},
				},
				Settings: models.CollectionSettings{
					Layout:       "fields-primary",
					DefaultView:  "board",
					BoardGroupBy: "status",
					ListSortBy:   "severity",
				},
			},
			docsCollection(3),
			conventionsCollection(4),
			playbooksCollection(5),
		},
	},
	{
		Name:        "product",
		Description: "Features, Feedback, Roadmap Items, Docs, Conventions, Playbooks",
		Collections: []DefaultCollection{
			{
				Name:        "Features",
				Slug:        "features",
				Icon:        "\u2728",
				Description: "Track feature development from proposal to launch",
				SortOrder:   0,
				Schema: models.CollectionSchema{
					Fields: []models.FieldDef{
						{
							Key:      "status",
							Label:    "Status",
							Type:     "select",
							Options:  []string{"proposed", "researching", "planned", "building", "shipped"},
							Default:  "proposed",
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
							Key:   "owner",
							Label: "Owner",
							Type:  "text",
						},
					},
				},
				Settings: models.CollectionSettings{
					Layout:       "fields-primary",
					DefaultView:  "board",
					BoardGroupBy: "status",
					ListSortBy:   "priority",
				},
			},
			{
				Name:        "Feedback",
				Slug:        "feedback",
				Icon:        "\U0001F4AC",
				Description: "Collect and prioritize user feedback",
				SortOrder:   1,
				Schema: models.CollectionSchema{
					Fields: []models.FieldDef{
						{
							Key:      "status",
							Label:    "Status",
							Type:     "select",
							Options:  []string{"new", "reviewed", "planned", "shipped"},
							Default:  "new",
							Required: true,
						},
						{
							Key:   "source",
							Label: "Source",
							Type:  "text",
						},
						{
							Key:   "customer",
							Label: "Customer",
							Type:  "text",
						},
						{
							Key:     "impact",
							Label:   "Impact",
							Type:    "select",
							Options: []string{"low", "medium", "high"},
							Default: "medium",
						},
					},
				},
				Settings: models.CollectionSettings{
					Layout:      "balanced",
					DefaultView: "list",
					ListSortBy:  "created_at",
					ListGroupBy: "status",
				},
			},
			{
				Name:        "Roadmap Items",
				Slug:        "roadmap-items",
				Icon:        "\U0001F5FA\uFE0F",
				Description: "Plan and communicate product direction",
				SortOrder:   2,
				Schema: models.CollectionSchema{
					Fields: []models.FieldDef{
						{
							Key:      "status",
							Label:    "Status",
							Type:     "select",
							Options:  []string{"planned", "in_progress", "completed"},
							Default:  "planned",
							Required: true,
						},
						{
							Key:   "quarter",
							Label: "Quarter",
							Type:  "text",
						},
						{
							Key:   "team",
							Label: "Team",
							Type:  "text",
						},
					},
				},
				Settings: models.CollectionSettings{
					Layout:       "balanced",
					DefaultView:  "board",
					BoardGroupBy: "status",
					ListSortBy:   "quarter",
				},
			},
			docsCollection(3),
			conventionsCollection(4),
			playbooksCollection(5),
		},
	},
	{
		Name:        "demo",
		Description: "Fully populated workspace — see every feature in 30 seconds",
		Collections: Defaults(),
		SeedItems:   demoSeedItems(),
	},
}

func demoSeedItems() []SeedItem {
	return []SeedItem{
		// Phase
		{
			CollectionSlug: "phases",
			Title:          "MVP Launch",
			Content: `# MVP Launch

Ship the core product with enough polish for early adopters.

## Goals
- Core CRUD working end-to-end (CLI + web UI)
- Clean onboarding experience
- Agent integration via /pad skill
- Public repo with README and install instructions

## Success Criteria
- A new user can install, init, and create their first task in under 2 minutes
- The /pad skill works reliably for common workflows
`,
			Fields: `{"status":"active","start_date":"2026-03-01"}`,
		},

		// Tasks — various statuses to make the board look alive
		{
			CollectionSlug: "tasks",
			Title:          "Set up CI/CD pipeline",
			Content:        "GitHub Actions for test, build, and release automation. GoReleaser for cross-platform binaries.",
			Fields:         `{"status":"done","priority":"high","effort":"m"}`,
		},
		{
			CollectionSlug: "tasks",
			Title:          "Design the landing page",
			Content:        "Hero section, feature highlights, install instructions, and a demo GIF. Keep it clean and developer-focused.",
			Fields:         `{"status":"in-progress","priority":"high","effort":"m"}`,
		},
		{
			CollectionSlug: "tasks",
			Title:          "Add Homebrew formula",
			Content:        "Create a homebrew-tap repo so users can `brew install pad`. GoReleaser can auto-generate the formula.",
			Fields:         `{"status":"done","priority":"medium","effort":"s"}`,
		},
		{
			CollectionSlug: "tasks",
			Title:          "Write API documentation",
			Content: `Document all REST API endpoints with request/response examples.

## Endpoints to cover
- Workspaces CRUD
- Collections CRUD
- Items CRUD (create, list, show, update, delete)
- Search
- Dashboard & next
- SSE events`,
			Fields: `{"status":"open","priority":"medium","effort":"l"}`,
		},
		{
			CollectionSlug: "tasks",
			Title:          "Add dark/light theme toggle",
			Content:        "The web UI is dark-only right now. Add a toggle in the header that respects system preference and saves to localStorage.",
			Fields:         `{"status":"open","priority":"low","effort":"s"}`,
		},
		{
			CollectionSlug: "tasks",
			Title:          "Fix search ranking for short queries",
			Content:        "Single-word searches like \"auth\" return too many low-relevance results. Investigate FTS5 ranking options and boost title matches.",
			Fields:         `{"status":"open","priority":"medium","effort":"m"}`,
		},

		// Ideas
		{
			CollectionSlug: "ideas",
			Title:          "Real-time collaboration",
			Content:        "WebSocket-based presence and conflict resolution for multi-user editing. Would need auth first. Could use CRDTs or OT.",
			Fields:         `{"status":"new","impact":"high","category":"infrastructure"}`,
		},
		{
			CollectionSlug: "ideas",
			Title:          "Keyboard shortcuts cheat sheet",
			Content:        "A `?` hotkey that shows all available keyboard shortcuts in a modal. Common in developer tools (GitHub, Linear, etc.).",
			Fields:         `{"status":"exploring","impact":"medium","category":"ux"}`,
		},

		// Doc with wiki-links
		{
			CollectionSlug: "docs",
			Title:          "Architecture Overview",
			Content: `# Architecture Overview

Pad is a single Go binary with an embedded SvelteKit web UI and SQLite storage.

## Components

- **CLI** — Cobra commands that talk to the API via HTTP
- **REST API** — chi router serving JSON endpoints at /api/v1/
- **Web UI** — SvelteKit 2 + Svelte 5, compiled to static files and embedded via go:embed
- **Store** — SQLite with FTS5 for full-text search, automatic migrations
- **Agent Skill** — /pad skill for Claude Code, Cursor, Copilot, and more — uses the CLI under the hood

## Key Design Decisions

1. **Single binary** — no external dependencies, easy to install and distribute
2. **SQLite** — no database server to manage, data lives in a single file
3. **Embedded web UI** — no separate frontend deployment, the binary serves everything
4. **Local-first** — your data stays on your machine, no accounts needed

## Related

See [[MVP Launch]] for the current phase and [[Write API documentation]] for the API docs effort.
`,
			Fields: `{"status":"published","category":"architecture"}`,
		},

		// Convention
		{
			CollectionSlug: "conventions",
			Title:          "Run tests before completing tasks",
			Content:        "Always run `make test` and verify tests pass before marking a task as done. If tests fail, fix them before completing the task.",
			Fields:         `{"status":"active","trigger":"on-task-complete","scope":"all","priority":"must"}`,
		},
		{
			CollectionSlug: "conventions",
			Title:          "Use conventional commit format",
			Content:        "Commit messages must follow the conventional commits format: type(scope): description. Types: feat, fix, docs, refactor, test, chore.",
			Fields:         `{"status":"active","trigger":"on-commit","scope":"all","priority":"should"}`,
		},

		// Playbook
		{
			CollectionSlug: "playbooks",
			Title:          "Implementation Workflow",
			Content: `1. Read the task description and any linked items
2. Create a feature branch from main
3. Implement the change in small, focused commits
4. Run tests: ` + "`make test`" + `
5. Run the build: ` + "`make build`" + `
6. Self-review the diff before creating a PR
7. Update the task status to done`,
			Fields: `{"status":"active","trigger":"on-implement","scope":"all"}`,
		},
	}
}

// GetTemplate returns the workspace template with the given name, or nil if
// no template with that name exists.
func GetTemplate(name string) *WorkspaceTemplate {
	for i := range templates {
		if templates[i].Name == name {
			return &templates[i]
		}
	}
	return nil
}

// ListTemplates returns all available workspace templates.
func ListTemplates() []WorkspaceTemplate {
	result := make([]WorkspaceTemplate, len(templates))
	copy(result, templates)
	return result
}
