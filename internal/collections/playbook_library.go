package collections

// LibraryPlaybook holds a pre-built playbook definition that can be
// activated (created as an item) in a workspace's Playbooks collection.
//
// InvocationSlug and Arguments are PLAN-1377's invocation surface. When
// set, the library entry becomes user-callable as `/pad <slug>`. Leave
// both unset for trigger-only checklist playbooks (legacy shape).
type LibraryPlaybook struct {
	Title          string           `json:"title"`
	Content        string           `json:"content"`
	Category       string           `json:"category"`                  // workflow, planning, quality, operations
	Trigger        string           `json:"trigger"`                   // on-implement, on-triage, on-release, on-plan, on-review, on-deploy, manual
	Scope          string           `json:"scope"`                     // all, backend, frontend, etc.
	InvocationSlug string           `json:"invocation_slug,omitempty"` // kebab-case slug for `/pad <slug>` routing
	Arguments      []map[string]any `json:"arguments,omitempty"`       // argument spec mirroring the body's `## Arguments` section
}

// PlaybookCategory groups related playbooks under a named category.
type PlaybookCategory struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Playbooks   []LibraryPlaybook `json:"playbooks"`
}

// PlaybookLibrary returns all pre-defined playbook categories with their
// playbooks. As of PLAN-1397's invokable-first overhaul (TASK-1403), the
// library teaches the PLAN-1377 invocation model from the first card:
// every active entry declares an `InvocationSlug` so the library doubles
// as the discovery surface for `/pad <slug>` workflows.
//
// The pre-PLAN-1377 trigger-only checklist entries (9 in total) live in
// playbook_library_archive.go and are no longer surfaced. Per-entry
// "convert to invokable" / "promote to convention" / "retire" decisions
// are tracked in IDEA-1396.
func PlaybookLibrary() []PlaybookCategory {
	return []PlaybookCategory{
		{
			Name:        "agent-workflows",
			Description: "Invokable agent workflows — `/pad <slug>` procedures with structured arguments",
			Playbooks: []LibraryPlaybook{
				// `ship` is the headline invokable example from PLAN-1377.
				// Library entry and startup-template seed share the same
				// body + argument spec (shipPlaybookBody /
				// shipPlaybookArguments) so updates live in one place.
				// Title matches ShipPlaybook()'s SeedPlaybook so the
				// library UI's activePlaybookTitles set (matched by
				// title) renders this as "Active" in `startup`
				// workspaces where it's already seeded.
				{
					Title:          "Ship tasks",
					Category:       "agent-workflows",
					Trigger:        "manual",
					Scope:          "all",
					InvocationSlug: "ship",
					Arguments:      shipPlaybookArguments,
					Content:        shipPlaybookBody,
				},
				// `plan` is the conversation-first companion to `ship`.
				// Body + arguments live in playbook_library_plan.go so
				// the library file stays focused on the registry.
				PlanPlaybook(),
				// `decompose` is the natural follow-up to `plan`:
				// takes a plan and creates the child task items its
				// body implies. Body + arguments live in
				// playbook_library_decompose.go.
				DecomposePlaybook(),
				// `onboard` is the workspace bootstrap interview.
				// Body + arguments live in playbook_library_onboard.go.
				// PLAN-1496 / TASK-1499.
				OnboardPlaybook(),
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
