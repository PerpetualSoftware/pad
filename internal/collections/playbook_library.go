package collections

// LibraryPlaybook holds a pre-built playbook definition that can be
// activated (created as an item) in a workspace's Playbooks collection.
type LibraryPlaybook struct {
	Title    string `json:"title"`
	Content  string `json:"content"`
	Category string `json:"category"` // workflow, planning, quality, operations
	Trigger  string `json:"trigger"`  // on-implement, on-triage, on-release, on-plan, on-review, on-deploy, manual
	Scope    string `json:"scope"`    // all, backend, frontend, etc.
}

// PlaybookCategory groups related playbooks under a named category.
type PlaybookCategory struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Playbooks   []LibraryPlaybook `json:"playbooks"`
}

// PlaybookLibrary returns all pre-defined playbook categories with their playbooks.
func PlaybookLibrary() []PlaybookCategory {
	return []PlaybookCategory{
		{
			Name:        "workflow",
			Description: "Core development workflows",
			Playbooks: []LibraryPlaybook{
				{
					Title:    "Implementation Workflow",
					Category: "workflow",
					Trigger:  "on-implement",
					Scope:    "all",
					Content: `1. Claim the task — update its status to in-progress
2. Load context — read related docs, architecture decisions, and linked items for background
3. Create an isolated workspace — use a feature branch or worktree to keep changes separate
4. Implement the changes — follow project conventions
5. Verify your work — run the project's test suite and build process
6. Self-review — check your diff for debug code, missing error handling, and unintended changes
7. Commit with a clear message — describe what changed and why, reference the task

` + "\U0001F4A1 Ask your AI agent to customize this playbook for your specific project tools and workflow.",
				},
				{
					Title:    "Code Review Process",
					Category: "workflow",
					Trigger:  "on-review",
					Scope:    "all",
					Content: `1. Read the context — review the PR description and linked task for intent
2. Understand the scope — what should this change do, and what shouldn't it touch?
3. Review for correctness — does the code do what it claims?
4. Review for robustness — error handling, edge cases, security considerations
5. Review for maintainability — clear naming, reasonable complexity, adequate comments
6. Check test coverage — are the important paths tested?
7. Provide actionable feedback — be specific, suggest alternatives, distinguish blockers from nits

` + "\U0001F4A1 Ask your AI agent to customize this playbook for your specific project tools and workflow.",
				},
			},
		},
		{
			Name:        "planning",
			Description: "Planning and triage workflows",
			Playbooks: []LibraryPlaybook{
				{
					Title:    "Phase Planning",
					Category: "planning",
					Trigger:  "on-plan",
					Scope:    "all",
					Content: `1. Review current state — check the roadmap, active phases, and recent progress
2. Define the goal — what does success look like for this phase?
3. Identify the work — list everything that needs to happen
4. Break into tasks — each task should be independently completable (one branch, one PR)
5. Estimate effort — flag tasks that seem too large and split them
6. Order by dependency — what must happen before what?
7. Set targets — define when the phase should start and end
8. Create the items — build the phase and its tasks in the project tracker

` + "\U0001F4A1 Ask your AI agent to customize this playbook for your specific project tools and workflow.",
				},
				{
					Title:    "Bug Triage",
					Category: "planning",
					Trigger:  "on-triage",
					Scope:    "all",
					Content: `1. Reproduce the issue — confirm it's real and understand the conditions
2. Assess impact — how many users are affected? Is there a workaround?
3. Determine severity — critical (broken for everyone), high (significant impact), medium (inconvenient), low (cosmetic)
4. Check for duplicates — search existing tasks for similar reports
5. Capture the details — create a task with: steps to reproduce, expected vs actual behavior, severity
6. Link related items — connect to relevant phases, architecture docs, or prior work
7. Prioritize — decide if it needs immediate attention or can be scheduled

` + "\U0001F4A1 Ask your AI agent to customize this playbook for your specific project tools and workflow.",
				},
			},
		},
		{
			Name:        "quality",
			Description: "Quality and improvement workflows",
			Playbooks: []LibraryPlaybook{
				{
					Title:    "Retrospective",
					Category: "quality",
					Trigger:  "manual",
					Scope:    "all",
					Content: `1. Gather the data — load the completed phase and all its tasks
2. What shipped — list everything that was completed
3. What was deferred — list anything that was planned but postponed
4. What went well — identify practices, tools, or decisions that helped
5. What could improve — identify friction, surprises, or mistakes
6. Action items — concrete changes for the next phase
7. Save and share — document the retrospective for future reference

` + "\U0001F4A1 Ask your AI agent to customize this playbook for your specific project tools and workflow.",
				},
				{
					Title:    "Onboarding to a Project",
					Category: "quality",
					Trigger:  "manual",
					Scope:    "all",
					Content: `1. Read the architecture — understand the tech stack, structure, and key patterns
2. Read the roadmap — understand where the project is and where it's going
3. Review active work — check current phases, in-progress tasks, and recent activity
4. Set up the environment — get the project building and running locally
5. Read the conventions — understand the team's rules and expectations
6. Pick a starter task — choose something small to build familiarity
7. Ask questions — don't guess, clarify unclear architecture or conventions

` + "\U0001F4A1 Ask your AI agent to customize this playbook for your specific project tools and workflow.",
				},
			},
		},
		{
			Name:        "operations",
			Description: "Release, deployment, and incident workflows",
			Playbooks: []LibraryPlaybook{
				{
					Title:    "Release Process",
					Category: "operations",
					Trigger:  "on-release",
					Scope:    "all",
					Content: `1. Verify completeness — confirm all planned tasks are done or explicitly deferred
2. Run the full test suite — no skipped or failing tests
3. Generate changelog — summarize what's new, fixed, and changed from completed tasks
4. Update version — bump version numbers as appropriate
5. Create the release — tag, branch, or package as your project requires
6. Verify in staging — deploy to a non-production environment first
7. Ship it — deploy to production
8. Monitor — watch for errors and regressions after release

` + "\U0001F4A1 Ask your AI agent to customize this playbook for your specific project tools and workflow.",
				},
				{
					Title:    "Deployment",
					Category: "operations",
					Trigger:  "on-deploy",
					Scope:    "all",
					Content: `1. Pre-flight check — verify CI is passing and the build is ready
2. Back up current state — ensure you can roll back if needed
3. Deploy to staging — verify in a non-production environment
4. Run smoke tests — confirm critical paths work
5. Deploy to production — follow your deployment process
6. Verify production — spot-check key functionality
7. Monitor — watch logs and metrics for 15-30 minutes
8. Communicate — let the team know what was deployed

` + "\U0001F4A1 Ask your AI agent to customize this playbook for your specific project tools and workflow.",
				},
				{
					Title:    "Incident Response",
					Category: "operations",
					Trigger:  "manual",
					Scope:    "all",
					Content: `1. Assess the situation — what's broken, who's affected, how severe?
2. Communicate — let the team know there's an active incident
3. Mitigate — can you reduce impact quickly? (rollback, feature flag, redirect)
4. Investigate — find the root cause
5. Fix — implement and verify the fix
6. Recover — restore full service and verify
7. Document — capture what happened, timeline, root cause, and fix
8. Follow up — create tasks for preventive measures

` + "\U0001F4A1 Ask your AI agent to customize this playbook for your specific project tools and workflow.",
				},
			},
		},
	}
}

// GetLibraryPlaybook finds a playbook in the library by its title.
// Returns nil if no playbook with the given title exists.
func GetLibraryPlaybook(title string) *LibraryPlaybook {
	for _, cat := range PlaybookLibrary() {
		for i := range cat.Playbooks {
			if cat.Playbooks[i].Title == title {
				return &cat.Playbooks[i]
			}
		}
	}
	return nil
}
