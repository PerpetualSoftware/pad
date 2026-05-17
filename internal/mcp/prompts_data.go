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

You are helping the user create and decompose a Plan in their pad workspace.

1. **Load context.** Run ` + "`pad project dashboard --format json`" + ` and ` + "`pad item list plans --all --format json`" + `. Understand current state — what plans exist, what's active, what's completed.
2. **Propose an outline.** Present a plan title plus a 1-line summary. Ask for feedback before writing anything.
3. **Create the plan** when approved:
   ` + "```bash" + `
   pad item create plan "Plan N: Title" --status draft --content "<plan content>"
   ` + "```" + `
4. **Decompose into tasks.** For each actionable unit, propose a Task; create it linked to the plan:
   ` + "```bash" + `
   pad item create task "Task description" --parent PLAN-3 --priority medium
   ` + "```" + `
5. **Suggest role assignments** if the workspace has agent roles ("This looks like Implementer work — assign to Implementer?").
6. **Size each task for a single meaningful unit of work.** Software workspaces typically size tasks to one branch / one PR; other domains size them to one deliverable, one interview loop, one draft section, etc. Check the workspace's conventions for domain-specific sizing rules.
7. **Always ask before creating each item.** Don't bulk-create without approval.
`

const promptIdeateBody = `# Pad: Ideate workflow

You are helping the user brainstorm an idea in their pad workspace.

1. **Load context.** Run ` + "`pad project dashboard --format json`" + ` and ` + "`pad item list --format json --limit 20`" + `.
2. **Search for related items:** ` + "`pad item search \"X\" --format json`" + `.
3. **Discuss systematically.** Ask clarifying questions, explore trade-offs, reference existing items with [[Title]] links to keep cross-links intact.
4. **Offer to save** at natural checkpoints. Examples:
   - "Want me to save this as an Idea?" → ` + "`pad item create idea \"X\" --content \"...\"`" + `
   - "Should I create a Doc for this architecture decision?" → ` + "`pad item create doc \"X\" --category decision --content \"...\"`" + `
5. **Never save without asking.** Always show what you'll create and get confirmation first.
`

const promptRetroBody = `# Pad: Retrospective workflow

You are helping the user run a retrospective on a completed Plan.

1. **Load the plan:** ` + "`pad item show PLAN-N --format markdown`" + `.
2. **Load the tasks:** ` + "`pad item list tasks --all --format json`" + ` (filter to the plan).
3. **Generate the retro:**
   - What shipped: completed tasks + impact.
   - What was deferred: tasks not done + why.
   - Lessons learned: themes from the run.
4. **Offer to save:** ` + "`pad item create doc \"Plan N Retrospective\" --category retro --content \"...\"`" + `.
5. **Offer to close the plan:** ` + "`pad item update PLAN-N --status completed`" + `.

Always confirm before creating or mutating items.
`

const promptOnboardBody = `# Pad: Onboard workflow

The canonical workspace-onboarding interview lives in the ` + "`/pad onboard`" + ` invokable library playbook (PLAN-1496 / TASK-1499). Every new workspace auto-seeds it as ` + "`status=active`" + ` (TASK-1500), so it should be directly invokable.

To run it:

1. Confirm the playbook is activated. ` + "`pad playbook list --format json`" + ` and look for ` + "`invocation_slug=onboard`" + ` with ` + "`status=active`" + `. If it's missing, activate from the library: ` + "`pad library activate playbook \"Onboard a workspace\"`" + ` (web UI: ` + "`/{ws}/library?tab=playbooks`" + `).
2. Load the body. ` + "`pad playbook show onboard --format markdown`" + `.
3. Follow the body's instructions. It teaches you the surface-agnostic interview: discover the domain, propose collections, adapt seeded conventions/playbooks to the project's actual tooling, suggest roles, seed a first item. The body is the source of truth — this prompt is just the dispatcher.

The pre-PLAN-1496 step-by-step workflow that used to live here (codebase-scan / suggest-conventions / draft-doc / propose-plan / suggest-roles) was retired in TASK-1505. All of it is now embedded in the playbook body, surface-agnostic so MCP-only agents can follow it too.
`
