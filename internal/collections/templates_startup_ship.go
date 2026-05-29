package collections

import "encoding/json"

// shipPlaybookBody is the de-personalized body of the seeded `ship` playbook.
// It's derived from the personal /ship-tasks slash command (PLAN-1377 / TASK-1386):
// a generic, agent-invocable procedure for shipping a list of Pad tasks
// autonomously — branch → implement → test → commit → PR → review loop → merge.
//
// Customization markers in the body (e.g. "swap for your review tool") keep
// the playbook usable on day one in any new workspace without forcing the
// user to wire two playbooks together before they can ship anything.
//
// Updates to the playbook body live here; the corresponding argument spec
// in shipPlaybookArguments stays in sync with the `## Arguments` section.
const shipPlaybookBody = `Autonomously work a list of tasks in order. For each task: claim it,
implement on a feature branch, test, commit, open a PR, run an automated
review loop until clean, merge, mark done, move on.

This playbook exists because running the workflow by hand across many
PRs is mechanical — the agent should just do it.

## Arguments

- ` + "`target`" + ` (required, PLAN-ref | TASK-ref | comma-separated list) — what to ship
- ` + "`stop-after-each`" + ` (flag, default=false) — pause for confirmation between merges
- ` + "`merge-strategy`" + ` (enum: squash|merge|rebase, default=squash) — how PRs get merged
- ` + "`limit`" + ` (number, optional) — ship at most N tasks from the list; unset = no limit
- ` + "`no-install`" + ` (flag, default=false) — skip the post-merge install step

## Argument parsing

` + "`target`" + ` accepts either a single ref or a comma-separated list of refs:

- **PLAN-ref** like ` + "`PLAN-9`" + ` — expand into its child tasks, ordered by ` + "`sort_order`" + `
  then ` + "`created_at`" + `. Only include children with status in {open, todo, in-progress};
  skip anything already done or cancelled.
- **Task refs** like ` + "`TASK-10,TASK-11,TASK-12`" + ` — comma-separated when binding via the
  CLI / MCP. The agent's natural-language parser also accepts space-separated
  refs (` + "`/pad ship TASK-10 TASK-11 TASK-12`" + `) and collapses them into the same
  comma-separated form before binding. Order is preserved as given.
- **Empty target** — ask the user which plan or tasks to ship.

## Pre-flight checks

Before starting the first task, verify the environment is ready. If any
check fails, report what's wrong and stop — don't guess.

1. **Task tracker available.** ` + "`pad auth whoami --format json`" + ` succeeds.
2. **Clean working tree on main.** ` + "`git status`" + ` clean, ` + "`git branch --show-current`" + ` = main.
3. **Up to date with remote.** ` + "`git fetch origin && git rev-list HEAD..origin/main --count`" + ` = 0.
4. **GitHub CLI authed.** ` + "`gh auth status`" + ` (only required if the project uses GitHub).
5. **Review CLI available.** ` + "`codex --version`" + ` (or whichever review tool the project uses —
   swap for ` + "`gemini`" + `, ` + "`claude`" + `, etc. The loop shape is what matters).
6. **Tests pass on baseline.** Run the project's test command. If the baseline is already
   red, stop — don't compound problems.

Report the full plan before starting (parent plan if any, ordered task list,
merge strategy, whether install runs after).

## Per-task loop

For each task in order:

### 1. Load the task

` + "`pad item show <TASK-REF> --format markdown`" + ` — read the full content, not just
the title. Check the parent plan's content for additional context. Load linked
items if wiki-links are present.

### 2. Load conventions

Pull all relevant conventions from the workspace so you follow them:

` + "```" + `bash
pad item list conventions --field trigger=always --field status=active --format json
pad item list conventions --field trigger=on-implement --field status=active --format json
` + "```" + `

Pay attention to branching, commit format, testing, and build conventions —
they almost always apply.

### 3. Mark in-progress

` + "```" + `bash
pad item update <TASK-REF> --status in-progress --comment "Starting — <one-line intent>"
` + "```" + `

### 4. Create a feature branch

Derive a descriptive name from the task title. Prefer the project's branching
convention if one exists. Common defaults: ` + "`feat/<slug>`" + `, ` + "`fix/<slug>`" + `,
` + "`docs/<slug>`" + `, ` + "`refactor/<slug>`" + `, ` + "`chore/<slug>`" + `.

` + "`git checkout -b feat/<slug>`" + `

### 5. Implement

Follow the task description. Read before editing. Keep changes minimal and
focused — a task is PR-sized; resist scope creep. If the task reveals
something that should be a separate task, note it and create a new Pad item
at the end (don't expand the current PR).

### 6. Build + test

Run the project's full local verification — read it from a convention or
take it as a playbook arg; don't hardcode. Common commands:

- Go: ` + "`go build ./... && go vet ./... && go test ./...`" + `
- Node/Web: ` + "`npm run build && npm test`" + ` (or ` + "`cd web && npm run build`" + `)
- Python: ` + "`pytest`" + ` (or whatever the project uses)

If verification fails, fix it before moving on. Do NOT commit a broken build.

### 7. Commit

Use the project's commit convention (conventional commits is the common case).
Reference the task ref in the message body. Describe what changed AND why.

` + "```" + `bash
git add -A && git commit -m "$(cat <<'EOF'
feat(scope): short summary (TASK-REF)

Longer description of what and why. Reference the parent Plan if relevant.

Parent: PLAN-XXX.
EOF
)"
` + "```" + `

### 8. Push + open PR

` + "```" + `bash
git push -u origin feat/<slug>
gh pr create --title "<same as commit subject>" --body "$(cat <<'EOF'
## Summary
<what + why>

## Context
Implements ` + "`<TASK-REF>`" + ` under ` + "`<PLAN-REF>`" + ` (if any).

## Test plan
- [x] build — clean
- [x] tests — pass
- [x] ... any manual checks
EOF
)"
` + "```" + `

Capture the PR number from the output.

### 9. Run the review loop

This is the core of the workflow — request an automated review, address every
finding, push, re-review, exit when the reviewer returns zero findings (or the
safety cap trips).

The example below uses Codex CLI (` + "`codex exec -s read-only -o <file> \"<review prompt>\" < /dev/null`" + `).
Pass the prompt as a positional argument — ` + "`codex exec`" + ` reads it from stdin
when none is given, so with ` + "`< /dev/null`" + ` an arg-less command reviews nothing.
**Swap for your review tool of choice** — Gemini, ` + "`claude review`" + `, a GitHub bot,
whatever you have. The loop shape (synchronous review → address findings →
push → re-review) is the part worth keeping.

**If you use ` + "`codex exec`" + `: always redirect stdin from ` + "`/dev/null`" + `.** It reads
extra input from stdin and hangs with zero output when stdin stays open (piped
or backgrounded) — which looks exactly like a prompt-length wedge but is NOT
fixed by a leaner prompt; only closing stdin fixes it. (The deprecated
` + "`--full-auto`" + ` flag has been dropped; ` + "`-s read-only`" + ` is sufficient.)

` + "```" + `
iteration = 0
prev_findings = "(initial review — no prior round)"
while iteration < 5:
  1. Request a focused review. Lean prompt template:
     "Review the diff between main and HEAD. Output findings or CLEAN."
     For re-reviews:
     "Re-review the diff. The previous round flagged: <bullet list>.
      Confirm fixes and look for new issues. Output findings or CLEAN."

  2. Read the output. If it returns ZERO findings → BREAK clean.

  3. For each finding (HIGH, MEDIUM, AND LOW):
     - Read the cited file
     - Apply the minimal fix
     - If the fix is non-obvious or you disagree, capture the reasoning
       in the commit message (the human reviewer will see it)
     - Only skip a finding if you genuinely disagree on technical grounds
       — then explain why in the next round's prompt so the reviewer
       stops re-flagging it

  4. Run local verification (build + tests). Fix anything that breaks.

  5. Commit: "fix(scope): <what> per review (round N)". Push.

  6. prev_findings = the findings you just addressed.
  iteration += 1
` + "```" + `

**Why all findings, not just HIGH?** A clean PR means clean. Leaving unfixed
LOW findings creates loose ends that compound across PRs and prevent any
future PR from ever converging to a true clean state. If a LOW would take
an hour and the PR is otherwise ready, that's a sign the LOW belongs in
its own follow-up PR — spawn a Pad item, document the deferral in code +
PR body, and explicitly note it in the next round's prompt so the reviewer
doesn't keep flagging it.

**Review prompt size matters.** Verbose prompts (focus areas, max-finding
caps, severity-format directives) can wedge some review CLIs. Keep it lean
— the shortest possible ask is the most reliable. (With ` + "`codex exec`" + `, rule
out open stdin FIRST — see above — since that produces the identical
zero-output symptom and isn't fixed by shortening the prompt.)

**Safety exits:**
- ` + "`iteration >= 5`" + ` — cap reached. Report remaining findings to the user.
- Review CLI fails or wedges twice on the lean prompt (for ` + "`codex exec`" + `,
  after confirming stdin is closed with ` + "`< /dev/null`" + `) — the tool itself
  is broken. Report and ask.

### 10. Merge

When the review loop exits clean:

` + "```" + `bash
gh pr merge <PR_NUMBER> --<MERGE_STRATEGY> --delete-branch
git checkout main
git pull --ff-only
` + "```" + `

### 11. Mark task done

` + "```" + `bash
pad item update <TASK-REF> --status done --comment "Merged PR #<N> after <R> review round(s). <Summary>."
` + "```" + `

If this was the last task under a plan, consider closing the plan too:

` + "```" + `bash
pad item update PLAN-XXX --status completed --comment "All child tasks shipped."
` + "```" + `

### 12. Stop-between check

If ` + "`stop-after-each`" + ` is set, report completion and ask the user whether to
continue. Otherwise proceed immediately to the next task.

## Post-run

After the last task merges:

1. **Install** — unless ` + "`no-install`" + ` was set, run the project's install step
   (` + "`make install`" + ` is the common case for Go projects shipping a binary).
   Report what was built.
2. **Report** — summarize the run: PRs merged, review rounds per PR, follow-ups
   captured as new Pad items.

## Stop conditions

Stop the run and report to the user (don't plow through) if:

- Build or tests fail AND the fix isn't obvious from the error
- A task's description is ambiguous or underspecified
- The reviewer raises a HIGH that requires design discussion (architectural
  disagreement, missing requirements you can't infer)
- A merge conflict needs human judgment
- ` + "`gh`" + `, ` + "`pad`" + `, or the review CLI breaks mid-run
- The safety cap (5 review rounds per PR) trips

When stopping: leave the current task ` + "`in-progress`" + `, leave the PR open with
the last commit pushed, report the state, and note what task comes next so
the user can pick up with ` + "`/pad ship <remaining-task-refs>`" + `.

## Philosophy

- **Autonomous, not reckless.** Go task to task without check-ins; stop the
  moment something stops being mechanical.
- **Follow the conventions.** They exist because the team wrote them. If a
  commit hook or build check rejects the work, the conventions say what to
  do — don't bypass hooks.
- **Small, focused diffs.** Each PR does one task. When a task grows, split
  it; don't let it metastasize.
- **Drive review synchronously.** The review tool finishing is the signal —
  not polling for a bot reaction.
- **Re-review every push.** Even fixes for trivial findings get a fresh
  review round. That's how you catch the regressions your fix introduced.
`

// shipPlaybookArguments is the structured argument spec mirroring the
// playbook body's `## Arguments` section. Two-way binding between this
// queryable form and the markdown is what the web UI editor builds on
// (TASK-1384).
//
// Types follow PLAN-1377's vocabulary: ref, string, flag, enum, number.
// Default values are passed through opaquely; the agent decides what to
// do with non-literal defaults like "∞".
var shipPlaybookArguments = []map[string]any{
	{
		"name":        "target",
		"type":        "string",
		"required":    true,
		"description": "PLAN-ref, TASK-ref, or comma-separated list of refs — what to ship.",
	},
	{
		"name":        "stop-after-each",
		"type":        "flag",
		"default":     false,
		"description": "Pause for confirmation between merges.",
	},
	{
		"name":        "merge-strategy",
		"type":        "enum",
		"enum":        []string{"squash", "merge", "rebase"},
		"default":     "squash",
		"description": "How PRs get merged.",
	},
	{
		"name":        "limit",
		"type":        "number",
		"description": "Ship at most N tasks from the list.",
	},
	{
		"name":        "no-install",
		"type":        "flag",
		"default":     false,
		"description": "Skip the post-merge install step.",
	},
}

// ShipPlaybook returns the seeded `ship` playbook for the startup template.
// PLAN-1377's headline deliverable: gives every fresh `startup` workspace
// a real, working example of the playbook invocation model rather than
// having to bootstrap one from scratch.
//
// Trigger is `manual` because there's no natural trigger event for "ship
// a list of tasks" — the user invokes it explicitly via `/pad ship <args>`
// or `pad playbook run ship`.
//
// invocation_slug=`ship` is the kebab-case routing token: typing
// `/pad ship PLAN-9` in a Claude Code session dispatches here.
func ShipPlaybook() SeedPlaybook {
	fields := map[string]any{
		"status":          "active",
		"trigger":         "manual",
		"scope":           "all",
		"invocation_slug": "ship",
		"arguments":       shipPlaybookArguments,
	}
	encoded, _ := json.Marshal(fields)
	return SeedPlaybook{
		Title:   "Ship tasks",
		Content: shipPlaybookBody,
		Fields:  string(encoded),
	}
}
