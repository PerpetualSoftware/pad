package collections

// planPlaybookBody is the de-personalized body of the library `plan`
// playbook. Subsumes the pre-PLAN-1377 "Plan Creation" library entry,
// reframed as a structured invokable procedure (PLAN-1397 T4).
//
// Generic across project types — no Pad-specific assumptions, no
// software-only vocabulary. A hiring workspace can /pad plan a
// recruiting push; a research workspace can /pad plan an experiment;
// the body adapts via the conversation, not via baked-in collection
// names. The one place a workspace-specific collection name appears
// is the `collection` argument, which defaults to `plans` but accepts
// `roadmap`, `projects`, or whatever the workspace uses.
//
// The body mirrors the "Planning: 'Let's create a plan'" workflow in
// skills/pad/SKILL.md so the conversational flow and the invokable
// procedure stay in lockstep. Updates to one should propagate to the
// other (T7 docs pass calls this out).
const planPlaybookBody = `Co-design a new plan with the user — discuss the goal, pin down
scope, propose a task breakdown, and create the plan item at the end.
This is a conversation-first playbook: every step is a checkpoint, and
nothing gets written until the user confirms.

## Arguments

- ` + "`topic`" + ` (required, string OR ref) — what to plan. Accepts either:
  - **A free-text topic** (e.g. "API redesign", "OAuth migration", "Q3 hiring push") — the normal "create a new plan" path.
  - **A ref to an existing plan-like item** (e.g. ` + "`PLAN-5`" + `, ` + "`IDEA-12`" + `) — the "elaborate this existing item into a fuller plan" path, triggered by UI quick-actions like "Plan this" on a plan card. In this mode, the agent loads the existing item and works WITH the user to expand its motivation/scope/breakdown rather than starting from scratch.
- ` + "`parent`" + ` (optional, ref) — existing IDEA, roadmap entry, or higher-level plan this reifies. Only meaningful in the "create new" path; ignored when the first positional is itself a ref.
- ` + "`collection`" + ` (optional, string, default=plans) — collection to create the plan in (some workspaces use ` + "`roadmap`" + `, ` + "`projects`" + `, etc.). Only meaningful in the "create new" path.

## Dispatch — new plan vs. elaborate existing

Detect which mode the user invoked by inspecting the first positional:

- **Looks like a ref** (matches ` + "`^[A-Z]+-\\d+$`" + ` and resolves to a real item in the workspace) → **elaborate mode**. Load the item with ` + "`pad item show <ref> --format markdown`" + `, ask the user what they want to expand or clarify, and update the item via ` + "`pad item update <ref> --stdin`" + ` with the agreed-upon body.
- **Otherwise** → **create-new mode**. Run the full Conversation flow below to design and create a fresh plan.

Quick-action prompts of the form ` + "`/pad plan <REF> \"<title>\" — outline goals, deliverables, and timeline`" + ` are the elaborate-mode entry point — the trailing freeform text after the title is conversational context, not extra positional args.

## Pre-flight

Before opening the conversation, gather workspace context so suggestions
are grounded in what already exists.

1. **Confirm the target collection exists.** Run ` + "`pad collection list --format json`" + `
   and verify the ` + "`collection`" + ` argument resolves. If it doesn't, ask the user
   which collection to use (or offer to create one) — don't guess.
2. **Load the current dashboard.** Run ` + "`pad project dashboard --format json`" + ` to
   see active plans, recent activity, and what's already in flight. Use this to
   spot overlap with existing work and to time-box the proposal sensibly.
3. **If ` + "`parent`" + ` is set, load it.** Run ` + "`pad item show <parent-ref> --format markdown`" + `
   and read the full body. The new plan should be a faithful reification of
   the parent's intent.
4. **Search for related items.** Run ` + "`pad item search \"<topic>\" --format json`" + ` to
   surface ideas, docs, or prior plans that touch the same area. If a closely
   related plan already exists, ask the user whether to extend it instead of
   creating a new one.

## Conversation

Walk the user through each step. Confirm at every checkpoint — never
create the plan until step 6.

### 1. Discuss the goal

Ask: what does success look like for this plan? Pin down the outcome,
not the activity. Example: "all paying users on the new billing flow"
beats "rewrite the billing flow."

### 2. Discuss scope

Two lists:
- **In scope** — what this plan covers
- **Out of scope** — what someone might reasonably expect but isn't included

The out-of-scope list is the more useful one. It prevents scope creep
and surfaces hidden assumptions early.

### 3. Surface related items

Reference the items found in pre-flight with ` + "`[[Item Title]]`" + ` wiki-links.
If the new plan supersedes an existing idea or replaces a stalled plan,
note that explicitly — it'll show up in the audit trail later.

### 4. Propose the title

Format: short, action-oriented, name the outcome. Example: ` + "`API redesign — move auth off the v1 endpoints`" + ` is better than ` + "`API redesign`" + ` alone because the title already implies the scope.

Get the user's confirmation before going further. Iterate on the title until they're happy.

### 5. Propose the task breakdown

List titles only — one line per task, ordered by dependency where one
exists. Each task should be PR-sized (one branch, one meaningful unit of
work). If a task feels large, split it.

Ask the user to approve the list in bulk OR to mark which to keep, drop,
or merge. Don't create the tasks yet — that's ` + "`/pad decompose <new-plan-ref>`" + `'s job after the plan exists.

### 6. Create the plan

Assemble the body from the agreed pieces:

` + "```markdown" + `
## Motivation

<from step 1>

## Scope

<from step 2 — in-scope bullets>

## Out of scope

<from step 2 — out-of-scope bullets>

## Task breakdown

<from step 5 — numbered list, titles only>

## Reference

<wiki-links from step 3>
` + "```" + `

Then create it:

` + "```bash" + `
pad item create <collection> "<title>" --status planned --stdin <<EOF
<assembled body>
EOF
` + "```" + `

If ` + "`parent`" + ` was set, attach the parent via ` + "`--parent <parent-ref>`" + `.

Capture the new plan's ref from the CLI output (e.g. ` + "`PLAN-N`" + `).

### 7. Offer next steps

Two natural follow-ups:

- **Decompose now:** turn the approved breakdown into child task items. Check whether the workspace has a ` + "`decompose`" + ` playbook activated (` + "`pad playbook list --format json`" + `) — if so, invoke it with ` + "`/pad decompose <new-plan-ref>`" + `; otherwise create the tasks inline:

  ` + "```bash" + `
  pad item create task "<task title>" --parent <new-plan-ref> --priority <X>
  ` + "```" + `

  one per approved item.
- **Decompose later:** the plan stands on its own; the breakdown lives in the body as plain text. The user can decompose anytime.

Don't push — just offer and respect the answer.

## Philosophy

- **Conversation before creation.** Never create the plan until step 6, and never create any tasks before the user has approved the breakdown in step 5. In step 7, prefer delegating task creation to the ` + "`decompose`" + ` playbook when it's available; only fall back to inline ` + "`pad item create task`" + ` calls if the user opts in and no decompose playbook is activated.
- **Out-of-scope is more useful than in-scope.** What you decide not to do is the harder, more valuable list.
- **Generic across project types.** A research workspace plans an experiment; a hiring workspace plans a recruiting push; a software workspace plans a feature. The structure is the same; the vocabulary adapts via the conversation.
- **Wiki-link related items.** ` + "`[[Title]]`" + ` references build the audit trail. Drift between sibling work surfaces here.
`

// planPlaybookArguments mirrors the body's `## Arguments` section
// (PLAN-1377). The structured form is the queryable contract; the
// markdown is the human-readable mirror. Keep them in sync.
//
// `topic` is typed `string` in the structured form because the
// strict CLI parser doesn't currently distinguish "string OR ref" —
// the body documents the dual-purpose semantics, and the agent's NL
// dispatcher routes ref-form invocations to elaborate-mode.
var planPlaybookArguments = []map[string]any{
	{
		"name":        "topic",
		"type":        "string",
		"required":    true,
		"description": "What to plan. Accepts either a free-text topic (e.g. \"API redesign\") for create-new mode, OR a ref like PLAN-5 / IDEA-12 to elaborate an existing item (the entry point for UI quick-actions like \"Plan this\").",
	},
	{
		"name":        "parent",
		"type":        "ref",
		"description": "Existing IDEA, roadmap entry, or higher-level plan this reifies. Only meaningful in create-new mode.",
	},
	{
		"name":        "collection",
		"type":        "string",
		"default":     "plans",
		"description": "Collection to create the plan in. Defaults to `plans`; some workspaces use `roadmap`, `projects`, etc. Only meaningful in create-new mode.",
	},
}

// PlanPlaybook returns the library entry for the generic `plan` playbook.
// Title is "Plan a new initiative" to read well in the library card and
// avoid colliding with collection-name vocabulary (the seeded `ship`
// uses "Ship tasks" — same naming style).
func PlanPlaybook() LibraryPlaybook {
	return LibraryPlaybook{
		Title:          "Plan a new initiative",
		Category:       "workflow",
		Trigger:        "manual",
		Scope:          "all",
		InvocationSlug: "plan",
		Arguments:      planPlaybookArguments,
		Content:        planPlaybookBody,
	}
}
