package collections

// LibraryConvention holds a pre-built convention definition that can be
// activated (created as an item) in a workspace's Conventions collection.
type LibraryConvention struct {
	Title       string   `json:"title"`
	Content     string   `json:"content"`
	Category    string   `json:"category"`    // git, quality, pm, docs, build
	Trigger     string   `json:"trigger"`     // always, on-task-start, on-task-complete, on-implement, on-commit, on-pr-create, on-plan-complete, on-plan
	Surfaces    []string `json:"surfaces"`    // all, backend, frontend, mobile, docs, devops
	Enforcement string   `json:"enforcement"` // must, should, nice-to-have
	Commands    []string `json:"commands,omitempty"`
}

// LibraryCategory groups related conventions under a named category.
type LibraryCategory struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Conventions []LibraryConvention `json:"conventions"`
}

// ConventionLibrary returns all pre-defined convention categories with their conventions.
func ConventionLibrary() []LibraryCategory {
	return []LibraryCategory{
		{
			Name:        "git",
			Description: "Git workflow conventions",
			Conventions: []LibraryConvention{
				{
					Title:       "Commit after task completion",
					Content:     "Create a git commit with a descriptive message after completing each discrete unit of work. Reference the task slug or item number in the commit message.",
					Category:    "git",
					Trigger:     "on-task-complete",
					Surfaces:    []string{"all"},
					Enforcement: "should",
					Commands:    []string{"git commit -m \"feat(scope): summary\""},
				},
				{
					Title:       "Work in worktrees per task",
					Content:     "Create a git worktree for each task to isolate work. Branch name should match the task slug. Run: git worktree add ../worktree-<slug> -b <slug>",
					Category:    "git",
					Trigger:     "on-task-start",
					Surfaces:    []string{"all"},
					Enforcement: "should",
					Commands:    []string{"git worktree add ../worktree-<slug> -b <slug>"},
				},
				{
					Title:       "Create PR on task completion",
					Content:     "Create a pull request using `gh pr create` when finishing a task. Include the task title in the PR title and link the task in the PR description.",
					Category:    "git",
					Trigger:     "on-task-complete",
					Surfaces:    []string{"all"},
					Enforcement: "should",
					Commands:    []string{"gh pr create"},
				},
				{
					Title:       "Conventional commit format",
					Content:     "Use conventional commit format for all commit messages: feat:, fix:, docs:, refactor:, test:, chore:. Include scope when relevant, e.g. feat(api): add user endpoint",
					Category:    "git",
					Trigger:     "on-commit",
					Surfaces:    []string{"all"},
					Enforcement: "should",
				},
				{
					Title:       "Never push directly to main",
					Content:     "Never commit or push directly to the main/master branch. Always use feature branches and merge via pull request.",
					Category:    "git",
					Trigger:     "on-commit",
					Surfaces:    []string{"all"},
					Enforcement: "must",
				},
			},
		},
		{
			Name:        "quality",
			Description: "Code quality conventions",
			Conventions: []LibraryConvention{
				{
					Title:       "Run tests before completing tasks",
					Content:     "Run the project's test suite before marking any task as done. If tests fail, fix them before completing the task.",
					Category:    "quality",
					Trigger:     "on-task-complete",
					Surfaces:    []string{"all"},
					Enforcement: "must",
					Commands:    []string{"go test ./...", "npm run build", "make install"},
				},
				{
					Title:       "Run linter before committing",
					Content:     "Run the project's linter/formatter before committing code to ensure consistent code style.",
					Category:    "quality",
					Trigger:     "on-commit",
					Surfaces:    []string{"all"},
					Enforcement: "should",
				},
				{
					Title:       "Add tests for new code",
					Content:     "When adding new functions, endpoints, or components, add corresponding test coverage. Aim for testing the happy path and key error cases.",
					Category:    "quality",
					Trigger:     "on-implement",
					Surfaces:    []string{"all"},
					Enforcement: "should",
				},
				{
					Title:       "Review your own changes before PR",
					Content:     "Before creating a PR, review your own diff. Check for: debug code left behind, missing error handling, unclear variable names, and unintended changes.",
					Category:    "quality",
					Trigger:     "on-pr-create",
					Surfaces:    []string{"all"},
					Enforcement: "should",
				},
			},
		},
		{
			Name:        "pm",
			Description: "Project management conventions",
			Conventions: []LibraryConvention{
				{
					Title:       "Update task status when starting work",
					Content:     "When starting work on a task, update its status to in-progress: `pad item update <ref> --status in-progress`",
					Category:    "pm",
					Trigger:     "on-task-start",
					Surfaces:    []string{"all"},
					Enforcement: "must",
					Commands:    []string{"pad item update <ref> --status in-progress"},
				},
				{
					Title:       "Summarize completed work",
					Content:     "When completing a task, add a brief summary of what was done and any decisions made to the task's content body.",
					Category:    "pm",
					Trigger:     "on-task-complete",
					Surfaces:    []string{"all"},
					Enforcement: "should",
				},
				{
					Title:       "Retrospective on plan completion",
					Content:     "When all tasks in a plan are done, suggest running a retrospective before marking the plan complete. Capture: what shipped, what was deferred, and lessons learned.",
					Category:    "pm",
					Trigger:     "on-plan-complete",
					Surfaces:    []string{"all"},
					Enforcement: "nice-to-have",
				},
				{
					Title:       "Link related items",
					Content:     "When working on a task that relates to other items (ideas, docs, other tasks), add [[wiki-links]] in the content to create connections.",
					Category:    "pm",
					Trigger:     "on-implement",
					Surfaces:    []string{"all"},
					Enforcement: "nice-to-have",
				},
			},
		},
		{
			Name:        "docs",
			Description: "Documentation conventions",
			Conventions: []LibraryConvention{
				{
					Title:       "Update docs on API changes",
					Content:     "When modifying API endpoints (adding, changing, or removing), update the corresponding API documentation to reflect the changes.",
					Category:    "docs",
					Trigger:     "on-implement",
					Surfaces:    []string{"backend", "docs"},
					Enforcement: "should",
				},
				{
					Title:       "Document architecture decisions",
					Content:     "When making a significant architectural choice, create a Doc item with category 'decision' explaining the rationale, alternatives considered, and trade-offs.",
					Category:    "docs",
					Trigger:     "on-implement",
					Surfaces:    []string{"all"},
					Enforcement: "nice-to-have",
				},
			},
		},
		{
			Name:        "build",
			Description: "Build and deploy conventions",
			Conventions: []LibraryConvention{
				{
					Title:       "Rebuild after code changes",
					Content:     "After modifying source code, run the project's build command to verify everything compiles and builds successfully.",
					Category:    "build",
					Trigger:     "on-implement",
					Surfaces:    []string{"all"},
					Enforcement: "must",
				},
				{
					Title:       "Verify locally before PR",
					Content:     "Before creating a PR, verify the changes work locally: build succeeds, tests pass, and the feature works as expected.",
					Category:    "build",
					Trigger:     "on-pr-create",
					Surfaces:    []string{"all"},
					Enforcement: "must",
					Commands:    []string{"go build ./...", "go test ./...", "npm run build", "make install"},
				},
			},
		},
	}
}

// GetLibraryConvention finds a convention in the library by its title.
// Returns nil if no convention with the given title exists.
func GetLibraryConvention(title string) *LibraryConvention {
	for _, cat := range ConventionLibrary() {
		for i := range cat.Conventions {
			if cat.Conventions[i].Title == title {
				return &cat.Conventions[i]
			}
		}
	}
	return nil
}
