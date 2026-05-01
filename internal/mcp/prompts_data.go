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

You are helping the user set up a fresh pad workspace, or scan an existing codebase for context.

1. **Check workspace state.** ` + "`pad project dashboard --format json`" + `. If the workspace already has items, ask whether they want to add more or start fresh sections.

2. **Check for a workspace-specific onboarding playbook.** Some templates ship their own:
   ` + "```bash" + `
   pad item list playbooks --field status=active --format json
   ` + "```" + `
   Look for a playbook whose title starts with "Onboarding" (or is explicitly about onboarding for this workspace type). If one exists, **follow its steps in order** — it's the template's opinion about how to get this kind of workspace set up.

3. **If the playbook is software-flavored or absent, do a codebase scan.** Skip this step for non-code workspaces:
   - ` + "`README.md`" + ` / ` + "`README`" + ` — project overview, setup instructions
   - ` + "`CLAUDE.md`" + ` — existing AI/agent instructions
   - Build config: ` + "`Makefile`, `package.json`, `go.mod`, `Cargo.toml`, `pyproject.toml`, `pom.xml`" + `
   - CI config: ` + "`.github/workflows/`, `.gitlab-ci.yml`, `.circleci/`" + `
   - Directory structure
   - Detect language, build system, test runner, linter — use the actual commands the project uses when suggesting conventions.

4. **Suggest conventions.** Present relevant conventions from the library as a checklist and ask which to activate. For code workspaces, customize with the actual commands found (e.g., "Run ` + "`make test`" + `" instead of "Run the test suite"). For non-code workspaces, lean on the template's starter pack.

5. **Draft a seed doc.** Summarize whatever's appropriate for the workspace type — an architecture doc for a codebase, a process doc for hiring, a research-agenda doc for a research workspace. Offer to save as a Doc item.

6. **Propose an initial plan.** For codebases, base it on recent ` + "`git log`" + ` and open TODOs. For other workspace types, base it on the first thing the user wants to track. Ask before creating.

7. **Suggest agent roles.** If no roles exist yet, suggest roles appropriate for the workspace type. Dev: Planner, Implementer, Reviewer. Hiring: Recruiter, Hiring Manager, Interviewer. Research: Researcher, Reviewer. Don't auto-create — ask first.

8. **Always confirm before creating each item.** Show what will be created, get approval, then create.
`
