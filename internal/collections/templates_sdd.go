package collections

import "github.com/PerpetualSoftware/pad/internal/models"

// specTemplate builds the "spec" workspace template (IDEA-2527): a
// spec-driven-development variant of the software templates. Idea → Spec →
// Tasks → PRs, with the spec (not a Plan) as the parenting artifact that
// carries acceptance criteria and gates implementation via conventions.
//
// Deliberately NO Plans collection — a spec's optional "## Implementation
// plan" body section covers what a separate Plans collection would (Dave's
// decision, IDEA-2527 comment 3): cheaper than maintaining two artifacts
// that would otherwise say almost the same thing.
func specTemplate() WorkspaceTemplate {
	return WorkspaceTemplate{
		Name:        "spec",
		Category:    CategorySoftware,
		Description: "Specs, Ideas, Tasks, Docs, Conventions, Playbooks — spec-driven development",
		Icon:        "\U0001F4D0", // 📐
		Collections: []DefaultCollection{
			specsCollection(0),
			ideasCollection(1),
			tasksCollection(2),
			docsCollection(3),
			conventionsCollection(4, SpecConventionTriggers, SpecConventionScopes),
			playbooksCollection(5, SpecPlaybookTriggers, SpecPlaybookScopes),
		},
		Conventions: specStarterConventions(),
		Playbooks:   specStarterPlaybooks(),
	}
}

// specContentTemplate is the body skeleton every new Specs item starts
// from (CollectionSettings.ContentTemplate, prefilled by the web UI's
// QuickCaptureSheet/Sidebar new-item flow). Mirrors the structure settled
// in IDEA-2527 comment 2: Context / Goals+Non-goals / Specified behavior
// (MUST/SHOULD) / Acceptance criteria with stable AC-N ids / Open
// questions, plus an optional Implementation plan section that stands in
// for a separate Plans collection.
const specContentTemplate = `## Context

<Why this spec exists — the problem, the trigger, links to the idea or bug it grew from (if any).>

## Goals

<What must be true when this is implemented. Outcomes, not activities.>

## Non-goals

<What's explicitly out of scope, so scope doesn't creep silently.>

## Specified behavior

<MUST / SHOULD statements describing the behavior this spec commits to. Every MUST should map to at least one acceptance criterion below — if it doesn't, either add the criterion or downgrade the statement to a goal.>

## Acceptance criteria

- AC-1: <a statement an agent or reviewer could actually check against the diff or running behavior — if you can't say how you'd verify it, it belongs in Goals, not here>
- AC-2: ...

## Open questions

<Anything unresolved. A spec with open questions can be drafted, but approval waits on these closing.>

## Implementation plan

<Optional. If it's useful to sketch build order or approach, it lives here instead of a separate Plans collection — decompose reads this section directly. Delete this section if you don't need it; an unedited placeholder is treated as absent, same as if the heading were missing.>
`

// specsCollection returns the Specs collection: the spec template's
// parenting artifact (replaces Plans in this template's idea→spec→tasks
// chain). Prefix is explicit even though DerivePrefix("Specs") already
// yields "SPEC" — self-documenting, and it stays correct if DerivePrefix's
// pluralization rule ever changes.
func specsCollection(sortOrder int) DefaultCollection {
	return DefaultCollection{
		Name:        "Specs",
		Slug:        "specs",
		Prefix:      "SPEC",
		Icon:        "\U0001F4D0", // 📐
		Description: "What must be true — approved specs gate implementation",
		SortOrder:   sortOrder,
		Schema: models.CollectionSchema{
			Fields: []models.FieldDef{
				{
					Key:             "status",
					Label:           "Status",
					Type:            "select",
					Options:         []string{"draft", "in-review", "approved", "implemented", "superseded"},
					TerminalOptions: []string{"superseded"},
					Default:         "draft",
					Required:        true,
				},
				{
					Key:   "version",
					Label: "Version",
					Type:  "text",
				},
				{
					Key:   "area",
					Label: "Area",
					Type:  "text",
				},
			},
		},
		Settings: models.CollectionSettings{
			Layout:      "content-primary",
			DefaultView: "board",
			ListSortBy:  "updated_at",
			ListGroupBy: "status",
			QuickActions: []models.QuickAction{
				{Label: "Verify against this spec", Prompt: "/pad verify {ref} — walk the acceptance criteria against the current diff/behavior", Scope: "item", Icon: "✅"},
				{Label: "Decompose into tasks", Prompt: "/pad decompose {ref} \"{title}\" into tasks", Scope: "item", Icon: "\U0001F4DD"},
				{Label: "Explain this spec", Prompt: "/pad explain {ref} \"{title}\" — what does it commit to and why?", Scope: "item", Icon: "\U0001F4AC"},
				{Label: "Find specs pending review", Prompt: "/pad triage all specs in draft or in-review status", Scope: "collection", Icon: "\U0001F4CB"},
			},
			ContentTemplate: specContentTemplate,
		},
	}
}

// specStarterConventions is the curated convention seed pack for spec
// workspaces (IDEA-2527 comment 2, four conventions). The third entry
// merges two closely-related rules from the design record —
// approved-spec-edits-require-re-review and supersede-don't-mutate — into
// one on-spec-change convention, since both describe the same discipline
// (an approved spec is not silently edited in place).
func specStarterConventions() []SeedConvention {
	return []SeedConvention{
		{
			Title:   "No implementation without an approved spec",
			Content: "Before implementing, load the governing SPEC-N and confirm its status is `approved` (or `implemented`, for follow-up work against an already-shipped spec). If the relevant behavior isn't covered by an approved spec yet, stop and draft one first (`/pad spec`) — don't implement against a draft or in-review spec, its acceptance criteria can still change out from under you.",
			Fields:  `{"status":"active","trigger":"on-implement","scope":"all","priority":"must"}`,
		},
		{
			Title:   "PRs cite the spec and which criteria they satisfy",
			Content: "Every PR body that implements spec-governed behavior must cite the SPEC-N ref and list which AC-N acceptance criteria it satisfies (e.g. \"Implements SPEC-4, satisfies AC-1, AC-2\"). This is what makes `/pad verify` fast — the reviewer walks the cited criteria against the diff instead of re-deriving what the PR is supposed to do.",
			Fields:  `{"status":"active","trigger":"on-pr-create","scope":"all","priority":"must"}`,
		},
		{
			Title:   "Approved specs are superseded, not mutated",
			Content: "Once a spec's status is `approved` (or later), don't edit its Context/Goals/Specified behavior/Acceptance criteria in place — that silently invalidates whatever was reviewed and whatever work already cites it. If the spec needs to change: (1) minor clarifications that don't change meaning are fine as edits with a comment explaining what and why; (2) anything that changes what must be true requires either re-review (bump status back to `in-review`, get it re-approved) or a new spec that supersedes this one (`status: superseded` on the old, a fresh SPEC-N referencing it) — supersede, don't mutate.",
			Fields:  `{"status":"active","trigger":"on-spec-change","scope":"all","priority":"must"}`,
		},
		{
			Title:   "Spec-code drift is a bug — in one of them",
			Content: "When a spec says X and the code does Y, that's a bug — either the code needs to change to match the spec, or the spec was wrong and needs to change (see: approved specs are superseded, not mutated). Don't let drift sit unresolved; surface it as soon as it's noticed rather than working around it silently.",
			Fields:  `{"status":"active","trigger":"always","scope":"all","priority":"should"}`,
		},
	}
}

// specStarterPlaybooks is the curated playbook seed pack for spec
// workspaces: the three SDD-specific playbooks — spec (templates_sdd_spec.go),
// verify (templates_sdd_verify.go), extract-specs
// (templates_sdd_extract.go) — plus two invokable workflows reused from
// elsewhere: decompose, generalized to accept SPEC refs (IDEA-2527,
// playbook_library_decompose.go) and ship, unchanged
// (templates_startup_ship.go).
func specStarterPlaybooks() []SeedPlaybook {
	plays := []SeedPlaybook{
		SpecPlaybook(),
		VerifyPlaybook(),
		ExtractSpecsPlaybook(),
		ShipPlaybook(),
	}
	if decompose := GetLibraryPlaybook("Decompose a plan into tasks"); decompose != nil {
		plays = append(plays, seedPlaybookFromLibrary(*decompose))
	}
	return plays
}
