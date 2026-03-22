package collections

// LibraryConvention holds a pre-built convention definition that can be
// activated (created as an item) in a workspace's Conventions collection.
type LibraryConvention struct {
	Title    string `json:"title"`
	Content  string `json:"content"`
	Category string `json:"category"` // git, quality, pm, docs, build
	Trigger  string `json:"trigger"`  // always, on-task-start, on-task-complete, on-implement, on-commit, on-pr-create, on-phase-complete, on-plan
	Scope    string `json:"scope"`    // all, backend, frontend, mobile, docs, devops
	Priority string `json:"priority"` // must, should, nice-to-have
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
					Title:    "Commit after task completion",
					Content:  "Create a git commit with a descriptive message after completing each discrete unit of work. Reference the task slug or item number in the commit message.",
					Category: "git",
					Trigger:  "on-task-complete",
					Scope:    "all",
					Priority: "should",
				},
				{
					Title:    "Work in worktrees per task",
					Content:  "Create a git worktree for each task to isolate work. Branch name should match the task slug. Run: git worktree add ../worktree-<slug> -b <slug>",
					Category: "git",
					Trigger:  "on-task-start",
					Scope:    "all",
					Priority: "should",
				},
				{
					Title:    "Create PR on task completion",
					Content:  "Create a pull request using `gh pr create` when finishing a task. Include the task title in the PR title and link the task in the PR description.",
					Category: "git",
					Trigger:  "on-task-complete",
					Scope:    "all",
					Priority: "should",
				},
				{
					Title:    "Conventional commit format",
					Content:  "Use conventional commit format for all commit messages: feat:, fix:, docs:, refactor:, test:, chore:. Include scope when relevant, e.g. feat(api): add user endpoint",
					Category: "git",
					Trigger:  "on-commit",
					Scope:    "all",
					Priority: "should",
				},
				{
					Title:    "Never push directly to main",
					Content:  "Never commit or push directly to the main/master branch. Always use feature branches and merge via pull request.",
					Category: "git",
					Trigger:  "on-commit",
					Scope:    "all",
					Priority: "must",
				},
			},
		},
		{
			Name:        "quality",
			Description: "Code quality conventions",
			Conventions: []LibraryConvention{
				{
					Title:    "Run tests before completing tasks",
					Content:  "Run the project's test suite before marking any task as done. If tests fail, fix them before completing the task.",
					Category: "quality",
					Trigger:  "on-task-complete",
					Scope:    "all",
					Priority: "must",
				},
				{
					Title:    "Run linter before committing",
					Content:  "Run the project's linter/formatter before committing code to ensure consistent code style.",
					Category: "quality",
					Trigger:  "on-commit",
					Scope:    "all",
					Priority: "should",
				},
				{
					Title:    "Add tests for new code",
					Content:  "When adding new functions, endpoints, or components, add corresponding test coverage. Aim for testing the happy path and key error cases.",
					Category: "quality",
					Trigger:  "on-implement",
					Scope:    "all",
					Priority: "should",
				},
				{
					Title:    "Review your own changes before PR",
					Content:  "Before creating a PR, review your own diff. Check for: debug code left behind, missing error handling, unclear variable names, and unintended changes.",
					Category: "quality",
					Trigger:  "on-pr-create",
					Scope:    "all",
					Priority: "should",
				},
			},
		},
		{
			Name:        "pm",
			Description: "Project management conventions",
			Conventions: []LibraryConvention{
				{
					Title:    "Update task status when starting work",
					Content:  "When starting work on a task, update its status to in-progress: `pad update <slug> --status in-progress`",
					Category: "pm",
					Trigger:  "on-task-start",
					Scope:    "all",
					Priority: "must",
				},
				{
					Title:    "Summarize completed work",
					Content:  "When completing a task, add a brief summary of what was done and any decisions made to the task's content body.",
					Category: "pm",
					Trigger:  "on-task-complete",
					Scope:    "all",
					Priority: "should",
				},
				{
					Title:    "Retrospective on phase completion",
					Content:  "When all tasks in a phase are done, suggest running a retrospective before marking the phase complete. Capture: what shipped, what was deferred, and lessons learned.",
					Category: "pm",
					Trigger:  "on-phase-complete",
					Scope:    "all",
					Priority: "nice-to-have",
				},
				{
					Title:    "Link related items",
					Content:  "When working on a task that relates to other items (ideas, docs, other tasks), add [[wiki-links]] in the content to create connections.",
					Category: "pm",
					Trigger:  "on-implement",
					Scope:    "all",
					Priority: "nice-to-have",
				},
			},
		},
		{
			Name:        "docs",
			Description: "Documentation conventions",
			Conventions: []LibraryConvention{
				{
					Title:    "Update docs on API changes",
					Content:  "When modifying API endpoints (adding, changing, or removing), update the corresponding API documentation to reflect the changes.",
					Category: "docs",
					Trigger:  "on-implement",
					Scope:    "backend",
					Priority: "should",
				},
				{
					Title:    "Document architecture decisions",
					Content:  "When making a significant architectural choice, create a Doc item with category 'decision' explaining the rationale, alternatives considered, and trade-offs.",
					Category: "docs",
					Trigger:  "on-implement",
					Scope:    "all",
					Priority: "nice-to-have",
				},
			},
		},
		{
			Name:        "build",
			Description: "Build and deploy conventions",
			Conventions: []LibraryConvention{
				{
					Title:    "Rebuild after code changes",
					Content:  "After modifying source code, run the project's build command to verify everything compiles and builds successfully.",
					Category: "build",
					Trigger:  "on-implement",
					Scope:    "all",
					Priority: "must",
				},
				{
					Title:    "Verify locally before PR",
					Content:  "Before creating a PR, verify the changes work locally: build succeeds, tests pass, and the feature works as expected.",
					Category: "build",
					Trigger:  "on-pr-create",
					Scope:    "all",
					Priority: "must",
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
