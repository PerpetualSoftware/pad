package collections

// decomposePlaybookBody is the de-personalized body of the library
// `decompose` playbook (PLAN-1397 T5). Takes a plan (or, since IDEA-2527,
// a spec) and turns its implicit work into explicit child task items,
// with the user's approval at every step.
//
// Pairs naturally with the `plan` playbook (PLAN-1397 T4) and, in the
// `spec` template, with the `spec` playbook (IDEA-2527): both create a
// parent item with a breakdown of work — a "## Task breakdown" section for
// plans, a "## Implementation plan" section and/or "## Acceptance
// criteria" for specs — and `/pad decompose <ref>` reads whichever is
// present and turns it into actual TASK items linked back via `--parent`.
//
// Shared across every software-category template (startup, scrum,
// product, spec) via softwareStarterPlaybookTitles / GetLibraryPlaybook
// title lookup — the plan-or-spec generalization below is additive
// wording only, so it stays coherent for templates with no Specs
// collection (the spec-specific bullets simply never match their bodies).
//
// Mirrors the "Decomposition: 'Break plan X into tasks'" workflow in
// skills/pad/SKILL.md so the conversational and invokable forms stay
// in lockstep. Updates to one should propagate to the other (T7 docs
// pass covers this).
const decomposePlaybookBody = `Turn a plan or spec into a set of child task items. The agent reads the
source item's body, proposes a task list, gets the user's approval, and
creates the tasks linked back to the source via ` + "`--parent`" + `.

This is the natural follow-up to the ` + "`plan`" + ` or ` + "`spec`" + `
playbook: those create the parent item with a breakdown (a plan's task
breakdown, or a spec's implementation plan / acceptance criteria) in the
body; this playbook turns that breakdown into actionable items the team
can claim, work, and ship.

## Arguments

- ` + "`target`" + ` (required, ref) — the plan or spec to decompose. Must resolve to an item in the plans-like or specs-like collection.
- ` + "`dry-run`" + ` (flag, default=false) — propose the task list but don't create anything. Use this to iterate on the proposed breakdown without committing.
- ` + "`collection`" + ` (optional, string, default=tasks) — collection to create child items in. Some workspaces use ` + "`bugs`" + `, ` + "`work-items`" + `, or domain-specific equivalents.

## Pre-flight

1. **Resolve the target.** Run ` + "`pad item show <target> --format markdown`" + ` and read the full body. If the ref doesn't resolve or isn't a plan-like or spec-like item, stop and report — don't guess.
2. **Verify the child collection exists.** Run ` + "`pad collection list --format json`" + ` and confirm the ` + "`collection`" + ` argument resolves. If it doesn't, ask the user which collection to use.
3. **Load existing children.** Run ` + "`pad item list <collection> --parent <target> --all --format json`" + ` to see what's already linked. The source item may have been partially decomposed before — don't duplicate.

## Conversation

### 1. Analyze the source body

Read the source item's content and identify actionable work. Look for:

- An explicit ` + "`## Task breakdown`" + `, ` + "`## Tasks`" + `, or ` + "`## Task ordering`" + ` section (the plan shape) — these are the easy wins; each bullet/line is a task candidate.
- An explicit ` + "`## Implementation plan`" + ` section (the spec shape) with real content — same treatment; each bullet/line is a task candidate. Treat a section that's still just the unedited placeholder text (angle-bracket instructions, no actual content) as if the heading were absent — don't propose a candidate task that's literally the placeholder.
- No implementation plan section on a spec, or only the placeholder: fall back to ` + "`## Acceptance criteria`" + ` — one task per AC-N is a reasonable default breakdown when nothing more specific is given. Note in the proposal which ACs each task covers.
- Checklist-style bullets in the body (` + "`- [ ] foo`" + ` or ` + "`1. foo`" + `).
- Scope bullets that imply work (e.g. "Add OAuth provider config" in a scope list is a task).
- Dependency markers in the body (e.g. "T1 is foundational", "depends on …", "after the migration") — capture these for step 5.

### 2. Reconcile with existing children

Compare your candidate list against the existing children loaded in pre-flight. For each existing child whose title matches a candidate, drop the candidate (the work is already tracked). For each candidate without a matching existing child, keep it.

If the existing children fully cover the source item, tell the user it's already decomposed and offer to:
- Run a status check (` + "`pad item list <collection> --parent <target> --all`" + `)
- Identify any drift between the source body and the children
- Stop without creating anything

### 3. Propose the task list

Present the candidates as a numbered list. For each, include:

- **Title** — short, action-oriented (one branch / one PR / one deliverable)
- **Priority** — inferred from the source body where possible (foundational tasks → high, content/polish → medium/low)
- **Dependency hint** — if step 1 surfaced ordering, note it (e.g. "T2 depends on T1")

Sample:

` + "```" + `
Proposed tasks for <target-ref>:
  1. [high]   Foundational: <T1 title>
  2. [medium] <T2 title>           (depends on #1)
  3. [medium] <T3 title>           (depends on #1)
  4. [medium] <T4 title>
  5. [low]    Closer: <T5 title>   (depends on #2, #3, #4)
` + "```" + `

### 4. Confirm

Ask the user to approve in bulk OR mark which to keep, drop, merge, or rename. If the user wants to iterate, regenerate the list and confirm again.

**If ` + "`dry-run`" + ` is true, stop here.** Report the proposed list and exit without creating any items.

### 5. Create approved tasks

For each approved task, run:

` + "```bash" + `
pad item create <collection> "<title>" --parent <target> --priority <X>
` + "```" + `

Capture each created item's ref (printed by the CLI) — you'll need them in step 6.

### 6. Wire dependencies

For each ordering pair the user approved in step 3 (e.g. "T2 depends on T1"):

` + "```bash" + `
pad item block <T1-ref> <T2-ref>
` + "```" + `

This records the dependency so downstream queries (` + "`pad item deps`" + `, ` + "`pad project next`" + `) honor the order.

### 7. Report

Summarize what was created:

` + "```" + `
Decomposed <target-ref> into 5 tasks:
  - TASK-X — Foundational: …
  - TASK-Y — …
  - …

Dependencies wired:
  - TASK-Y blocks TASK-Z
  - …

Next: run the ship playbook on <target-ref> to start working through them (shortcut: ` + "`/pad ship <target-ref>`" + ` in Claude Code), or open TASK-X to dive into the first task.
` + "```" + `

## Philosophy

- **Reflect, don't invent.** The tasks should come from what the source item actually says, not from what the agent thinks work like this usually needs. If the source item is underspecified, surface that — don't paper over it with generic tasks.
- **One unit of meaningful work per task.** What "meaningful" means depends on the workspace (a branch/PR for code; an interview round for hiring; a draft section for content). Check the workspace's conventions for sizing rules.
- **Confirm before creating.** Especially in bulk operations — once 8 tasks are in the workspace, cleaning them up is a chore.
- **Dependencies make ` + "`pad project next`" + ` smarter.** Wiring them up at decomposition time means the team gets accurate "what to work on now" guidance without manual triage later.
- **Dry-run is cheap.** When the source item is large or ambiguous, run dry-run first to iterate on the breakdown before committing.
`

// decomposePlaybookArguments mirrors the body's `## Arguments`
// section. Structured form is the queryable contract; markdown is
// the human-readable mirror. Keep them in sync.
var decomposePlaybookArguments = []map[string]any{
	{
		"name":        "target",
		"type":        "ref",
		"required":    true,
		"description": "The plan or spec to decompose. Must resolve to a plan-like or spec-like item.",
	},
	{
		"name":        "dry-run",
		"type":        "flag",
		"default":     false,
		"description": "Propose the task list but don't create anything. Use to iterate before committing.",
	},
	{
		"name":        "collection",
		"type":        "string",
		"default":     "tasks",
		"description": "Collection to create child items in. Defaults to `tasks`; some workspaces use `bugs`, `work-items`, etc.",
	},
}

// DecomposePlaybook returns the library entry for the generic `decompose`
// playbook. Title is "Decompose a plan into tasks" to read well in the
// library card and pair naturally with "Plan a new initiative" (T4).
func DecomposePlaybook() LibraryPlaybook {
	return LibraryPlaybook{
		Title:          "Decompose a plan into tasks",
		Category:       "agent-workflows",
		Trigger:        "manual",
		Scope:          "all",
		InvocationSlug: "decompose",
		Arguments:      decomposePlaybookArguments,
		Content:        decomposePlaybookBody,
	}
}
