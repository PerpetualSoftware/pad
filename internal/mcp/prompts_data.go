package mcp

// Prompt body texts. Lifted from skills/pad/SKILL.md
// (Multi-Step Workflows section). Each body is the workflow's
// content, lightly unwrapped from skill-specific framing — agents
// receive these as user-role system instructions guiding the
// conversation.
//
// When SKILL.md changes, update these strings to keep the prompts
// in lockstep. The TestPromptsLockstep_* tests assert key phrases
// stay present so silent drift is caught at CI time.

const promptPlanBody = `# Pad: Plan workflow

You are helping the user create and decompose a Plan in their pad workspace. Use your MCP tools — no shell required. (CLI-capable agents can run the parenthetical ` + "`pad`" + ` commands instead.)

1. **Load context.** Call ` + "`pad_project`" + ` with ` + "`action: dashboard`" + ` (CLI: ` + "`pad project dashboard`" + `) and ` + "`pad_item`" + ` with ` + "`action: list`" + `, ` + "`collection: plans`" + ` (CLI: ` + "`pad item list plans --all`" + `). Understand current state — what plans exist, what's active, what's completed.
2. **Propose an outline.** Present a plan title plus a 1-line summary. Ask for feedback before writing anything.
3. **Create the plan** when approved: call ` + "`pad_item`" + ` with ` + "`action: create`" + `, ` + "`collection: plans`" + `, ` + "`title: \"Plan N: Title\"`" + `, ` + "`status: draft`" + `, and the plan content (CLI: ` + "`pad item create plan \"Plan N: Title\" --status draft --content \"<plan content>\"`" + `).
4. **Decompose into tasks.** For each actionable unit, propose a Task; create it linked to the plan — call ` + "`pad_item`" + ` with ` + "`action: create`" + `, ` + "`collection: tasks`" + `, ` + "`title: \"Task description\"`" + `, ` + "`parent: PLAN-3`" + `, ` + "`priority: medium`" + ` (CLI: ` + "`pad item create task \"Task description\" --parent PLAN-3 --priority medium`" + `).
5. **Suggest role assignments** if the workspace has agent roles ("This looks like Implementer work — assign to Implementer?").
6. **Size each task for a single meaningful unit of work.** Software workspaces typically size tasks to one branch / one PR; other domains size them to one deliverable, one interview loop, one draft section, etc. Check the workspace's conventions for domain-specific sizing rules.
7. **Always ask before creating each item.** Don't bulk-create without approval.
`

const promptIdeateBody = `# Pad: Ideate workflow

You are helping the user brainstorm an idea in their pad workspace. Use your MCP tools — no shell required. (CLI-capable agents can run the parenthetical ` + "`pad`" + ` commands instead.)

1. **Load context.** Call ` + "`pad_project`" + ` with ` + "`action: dashboard`" + ` (CLI: ` + "`pad project dashboard`" + `) and ` + "`pad_item`" + ` with ` + "`action: list`" + `, ` + "`limit: 20`" + ` (CLI: ` + "`pad item list --limit 20`" + `).
2. **Search for related items.** Call ` + "`pad_search`" + ` with ` + "`action: query`" + `, ` + "`query: \"X\"`" + ` (CLI: ` + "`pad item search \"X\"`" + `).
3. **Discuss systematically.** Ask clarifying questions, explore trade-offs, reference existing items with [[Title]] links to keep cross-links intact.
4. **Offer to save** at natural checkpoints. Examples:
   - "Want me to save this as an Idea?" → call ` + "`pad_item`" + ` with ` + "`action: create`" + `, ` + "`collection: ideas`" + `, ` + "`title: \"X\"`" + `, ` + "`content: \"...\"`" + ` (CLI: ` + "`pad item create idea \"X\" --content \"...\"`" + `).
   - "Should I create a Doc for this architecture decision?" → call ` + "`pad_item`" + ` with ` + "`action: create`" + `, ` + "`collection: docs`" + `, ` + "`title: \"X\"`" + `, ` + "`category: decision`" + `, ` + "`content: \"...\"`" + ` (CLI: ` + "`pad item create doc \"X\" --category decision --content \"...\"`" + `).
5. **Never save without asking.** Always show what you'll create and get confirmation first.
`

const promptRetroBody = `# Pad: Retrospective workflow

You are helping the user run a retrospective on a completed Plan. Use your MCP tools — no shell required. (CLI-capable agents can run the parenthetical ` + "`pad`" + ` commands instead.)

1. **Load the plan.** Call ` + "`pad_item`" + ` with ` + "`action: get`" + `, ` + "`ref: PLAN-N`" + ` (CLI: ` + "`pad item show PLAN-N --format markdown`" + `).
2. **Load the tasks.** Call ` + "`pad_item`" + ` with ` + "`action: list`" + `, ` + "`collection: tasks`" + ` (CLI: ` + "`pad item list tasks --all`" + `), then filter to the plan.
3. **Generate the retro:**
   - What shipped: completed tasks + impact.
   - What was deferred: tasks not done + why.
   - Lessons learned: themes from the run.
4. **Offer to save.** Call ` + "`pad_item`" + ` with ` + "`action: create`" + `, ` + "`collection: docs`" + `, ` + "`title: \"Plan N Retrospective\"`" + `, ` + "`category: retro`" + `, ` + "`content: \"...\"`" + ` (CLI: ` + "`pad item create doc \"Plan N Retrospective\" --category retro --content \"...\"`" + `).
5. **Offer to close the plan.** Call ` + "`pad_item`" + ` with ` + "`action: update`" + `, ` + "`ref: PLAN-N`" + `, ` + "`status: completed`" + ` (CLI: ` + "`pad item update PLAN-N --status completed`" + `).

Always confirm before creating or mutating items.
`

const promptOnboardBody = `# Pad: Onboard workflow

The canonical workspace-onboarding interview lives in the **onboard** invokable library playbook (PLAN-1496 / TASK-1499). Every new workspace auto-seeds it as ` + "`status=active`" + ` (TASK-1500), so it should be directly invokable.

Natural language is the canonical trigger and works on every surface — when the user says "set up my workspace" / "onboard me," run this playbook. The slug shortcuts (` + "`/pad onboard`" + ` in Claude Code, ` + "`$pad onboard`" + ` in Codex, this ` + "`pad_onboard`" + ` prompt) are equivalent entry points into the same playbook.

To run it (using your MCP tools — no shell required):

1. Confirm the playbook is activated. Call ` + "`pad_playbook`" + ` with ` + "`action: list`" + ` and look for ` + "`invocation_slug=onboard`" + ` with ` + "`status=active`" + `. If it's missing, activate it yourself — call ` + "`pad_library`" + ` with ` + "`action: activate`" + ` and ` + "`title: \"Onboard a workspace\"`" + ` (no shell needed), then re-list. (The user can also activate it from the library in the web UI: ` + "`/{ws}/library?tab=playbooks`" + `.)
2. Load the body. Call ` + "`pad_playbook`" + ` with ` + "`action: get`" + ` and ` + "`ref: onboard`" + ` (this returns the full body; it is side-effect-free).
3. Follow the body's instructions. It teaches you the surface-agnostic interview: discover the domain, propose collections, adapt seeded conventions/playbooks to the project's actual tooling, suggest roles, seed a first item. The body is the source of truth — this prompt is just the dispatcher.

(CLI-capable agents can equivalently use ` + "`pad playbook list`" + ` / ` + "`pad playbook show onboard --format markdown`" + `; the ` + "`pad_playbook`" + ` tool is the surface-neutral path that also works for shell-less MCP clients.)

The pre-PLAN-1496 step-by-step workflow that used to live here (codebase-scan / suggest-conventions / draft-doc / propose-plan / suggest-roles) was retired in TASK-1505. All of it is now embedded in the playbook body, surface-agnostic so MCP-only agents can follow it too.
`
