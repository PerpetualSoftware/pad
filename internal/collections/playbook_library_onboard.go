package collections

import "encoding/json"

// playbook_library_onboard.go ships the canonical `/pad onboard`
// library playbook (PLAN-1496 / TASK-1499). This is the workspace's
// adaptive bootstrap interview — the playbook the agent runs to turn
// a freshly-created workspace (any template, but especially `blank`)
// into a configured workspace whose collections, conventions,
// playbooks, and roles actually match the user's project.
//
// Design principles, in order of importance:
//
//   1. **Adapt, don't curate.** Library entries are STARTING POINTS,
//      not finished artifacts. The body says this explicitly because
//      the default agent instinct is to copy-paste, which is wrong
//      here. Read library text, understand the rule, then rewrite
//      using the project's actual commands, paths, vocabulary.
//
//   2. **Surface-agnostic.** The body describes intent, not specific
//      commands. "Read README/manifest if you can" — not "run pad
//      onboard --detect-only". "Create via your available tool —
//      pad_item MCP or pad item CLI" — not "shell out and grep."
//      Pure MCP users (no shell) must be able to follow the same
//      flow as Claude Code users with full Bash.
//
//   3. **Mode-aware.** Blank workspaces build from scratch. Templated
//      workspaces enter audit/extend mode and rewrite seeded items
//      to match the project. The playbook detects the right mode
//      from bootstrap state.
//
//   4. **Confirmation before mutation.** Especially for destructive
//      ops (collection delete, item delete). The agent proposes a
//      change, the user approves, then it lands.
//
//   5. **Self-removing nudge.** Bootstrap surfaces a "this workspace
//      needs onboarding" flag (TASK-1504) until the user has any
//      user-created items. The playbook doesn't manage the flag
//      directly — it just runs the interview that produces the
//      user-created items that clear it.

const onboardPlaybookBody = `Run the workspace bootstrap interview. This is THE entry point for
turning a freshly-created workspace into one that fits the user's
actual project — domain, vocabulary, conventions, tooling, all of it.
The playbook adapts to whatever template (or no template) the user
picked at workspace creation, and adapts EVERYTHING the template
seeded to the project at hand.

## Arguments

- ` + "`mode`" + ` (optional, enum: ` + "`auto`" + `, ` + "`build`" + `, ` + "`audit`" + `, ` + "`revisit`" + `, default=` + "`auto`" + `) — force a specific path. ` + "`auto`" + ` detects from workspace state.
- ` + "`defaults`" + ` (optional, flag) — fast-path through with sensible defaults instead of a full interview. Use when the user wants the agent to "just pick reasonable things and go."
- ` + "`skip-codebase`" + ` (optional, flag) — skip the codebase auto-detection step even if a codebase is present. Useful for non-code workspaces (hiring, research, content) and for code workspaces where the user wants to drive the conversation rather than have the agent volunteer detected commands.

## Hard rules — read before doing anything else

**ADAPT, DON'T CURATE.** This is the most important rule in this playbook. Every library entry, every template seed, every default vocabulary you encounter is a STARTING POINT — not a finished artifact. The default instinct is to copy-paste library text into the workspace verbatim. That instinct is WRONG here. Instead:

- Read the library entry. Understand the *rule* it expresses.
- Rewrite it in this project's voice, with this project's actual commands and paths and file extensions.
- If the library has nothing close to what the workspace needs, INVENT a new convention or playbook from scratch. The library is raw material, not a menu.
- If the template seeded items that don't fit (wrong collection names, wrong status vocabularies, wrong role descriptions) — REWRITE them. Use ` + "`pad collection update`" + ` / ` + "`pad item update`" + ` / ` + "`pad role update`" + ` (or the equivalent MCP actions). Don't add alongside; replace.
- If a seeded collection genuinely doesn't belong, suggest deleting it. Default collections refuse deletion server-side — adapt them via ` + "`update`" + ` instead.

**SURFACE-AGNOSTIC.** Don't assume the agent running this playbook has a shell. Describe intent, not specific commands. When you DO need to suggest a tool, offer both the CLI form (` + "`pad item create ...`" + `) and the MCP form (` + "`pad_item.action: create`" + `) so the agent picks whichever it has. NEVER write "run ` + "`pad onboard --detect-only --format json`" + `" or similar CLI-only escape hatches — those don't exist for MCP users.

**CONFIRMATION BEFORE MUTATION.** Especially for ` + "`pad collection delete`" + ` and ` + "`pad item delete`" + `. Show the user what you'll change. Get a yes. Then mutate.

**OWNER ROLE REQUIRED FOR SCHEMA MUTATIONS.** Collection update/delete and role update all require the workspace owner role server-side. If the agent's user is an editor or viewer, ` + "`pad collection update`" + ` will 403. In that case, fall back to suggesting the change to the user (who runs it themselves) instead of trying to do it.

## Pre-flight

1. **Load the bootstrap blob.** ` + "`pad bootstrap --format json`" + ` (CLI) or the ` + "`pad_meta.action: bootstrap`" + ` MCP action. This is one round-trip and gives you everything you need: workspace name, user, collections (with schemas), conventions, playbooks, roles, dashboard, recent activity.

   **Read ` + "`workspace.description`" + ` if present** — it's the free-text "what are you tracking?" the user gave at workspace creation. Treat it as the user's stated intent: it seeds the interview so you don't have to open with "what is this project?". Reflect it back to confirm rather than asking cold (see B1).

2. **Detect mode.** Inspect the bootstrap to pick the right path:
   - **build** — workspace has only the two system collections (` + "`conventions`" + `, ` + "`playbooks`" + `) AND no user-created items AND no seeded conventions/playbooks (beyond this onboard playbook itself). This is the ` + "`blank`" + ` template path.
   - **audit** — workspace has a template's seeded user-facing collections (Tasks/Ideas/Plans/Docs/Backlog/Sprints/etc.) and possibly seeded conventions/playbooks, but no user-created items yet. This is the non-blank templates path. The user picked an opinionated starting point; your job is to make it fit.
   - **revisit** — workspace has user-created items. The user has already onboarded once and is invoking this playbook to add/change something specific. Ask what they want to revisit; don't run the full interview.

3. **Honor the ` + "`mode`" + ` argument override.** If the user passed ` + "`mode=audit`" + ` on a workspace that looks blank, trust them — maybe they want to audit a workspace they just adapted manually.

4. **If ` + "`skip-codebase`" + ` is unset AND the agent can read files**, look at the workspace's working directory. Whether to even mention the codebase depends on what you find:
   - Code project markers: ` + "`README.md`" + `, ` + "`package.json`" + `, ` + "`go.mod`" + `, ` + "`Cargo.toml`" + `, ` + "`pyproject.toml`" + `, ` + "`Makefile`" + `, ` + "`.github/workflows/`" + ` — note the language, build tool, test command, CI provider. You'll surface these in the interview.
   - Non-code markers or empty directory — skip the codebase angle entirely.
   - **If the agent cannot read files** (pure MCP user), skip this step silently. You'll ask the user directly in the interview.

5. **Greet the user.** Brief, conversational. Mention the mode you detected so the user can correct you. Example: "I see a fresh ` + "`startup`" + `-template workspace — let's adapt the seeded collections to match your project. Want to walk through the interview, or fast-path with ` + "`defaults`" + `?"

## Mode: build (blank workspace)

The user picked the blank template (or somehow ended up with a workspace that has only system collections). Your job is to build everything from scratch via the conversation.

### B1. Discover the domain

If ` + "`workspace.description`" + ` was set at creation, lead by reflecting it back instead of asking cold: "You mentioned this workspace is for <description> — let's build around that. Tell me more about how you work?" Only fall back to the open question — "What is this workspace for?" — when no description was captured. Listen to the answer either way.

Use the codebase context if you have it. Example: "I see a Go project with a Makefile and GitHub Actions — does this workspace track software development for that codebase?"

Common domains and what they imply (these are HINTS, not a menu — the user might say something else entirely):
- Software development → Tasks, Ideas, Plans, Bugs, Docs collections. Conventions around commits / tests / PR reviews. Roles like Planner / Implementer / Reviewer.
- Hiring → Requisitions, Candidates, Interview Loops, Feedback. Conventions around candidate anonymization and feedback timeliness.
- Personal projects / journaling → Tasks, Notes, Decisions. Lightweight.
- Research → Experiments, Sources, Notes. Conventions around citation and reproducibility.
- Content production → Articles, Drafts, Edits, Distribution. Conventions around voice + publishing checklist.

Don't force the user into one of these. If they describe something the library doesn't have a template for — that's fine, INVENT the collections.

### B2. Propose collection set

Based on the domain, propose a set of collections. Show the proposed list. For each, mention what status enum it would have and what other fields. Get the user's approval.

Example: "Sounds like you want to track ` + "`tasks`" + ` (open / in-progress / done), ` + "`bugs`" + ` (open / fixing / fixed), and ` + "`docs`" + ` (draft / published). Does that match how you think about this work?"

Iterate until the user's happy. Then create the collections — one ` + "`pad collection create`" + ` (CLI) or ` + "`pad_collection.action: create`" + ` (MCP) per collection.

### B3. Propose conventions

The conventions collection already exists (blank template ships it). Now you fill it.

Browse the convention library:

- **CLI:** ` + "`pad library list --type conventions`" + ` (filter with ` + "`--category <name>`" + `; full body of one entry via ` + "`pad library get \"<title>\"`" + `).
- **MCP:** ` + "`pad_library`" + ` with ` + "`action: list, type: conventions`" + ` (and optional ` + "`category`" + `). Full body of one entry via ` + "`pad_library`" + ` with ` + "`action: get, title: \"<title>\"`" + `. Same shape on both surfaces. Closed by PLAN-1560 (IDEA-1514).

For each convention that's plausibly relevant, READ ITS BODY, then rewrite using this project's actual commands. Examples:

- Library has "Run tests before completing tasks." If the project is Go with a Makefile, your version says "Run ` + "`make test`" + ` before marking a task done. If the build fails, fix it before merging."
- Library has "Conventional commit format." If the project's existing commits don't follow that style, ASK the user before activating it — maybe they don't want it.

For each rewritten convention, propose it to the user. Confirm. Create via ` + "`pad item create conventions \"<title>\" --field trigger=<trigger> --field status=active --stdin`" + ` (or the MCP equivalent).

If the library has nothing close to what the user needs, INVENT a convention.

**Independent code reviewer (software projects).** One library convention is worth
calling out specially: **"Independent AI code review."** The principle — a reviewer
model DIFFERENT from your implementer model catches far more than self-review;
reviewing with the same model that wrote the code is closer to self-review. To
suggest it well, do two checks:
1. **Your model = the implementer.** You are the agent running this onboarding, so
   you know your own model (e.g. Claude). That's the implementer.
2. **Is a different-model review tool available?** If you have a shell, probe for one
   (e.g. ` + "`codex --version`" + `, ` + "`gemini --version`" + `); if you're a pure-MCP agent with no
   shell, just ask the user what review tools they have.

If a review tool is present AND it's a different model than yours, propose activating
the "Independent AI code review" convention and name the detected tool — e.g. *"I see
` + "`codex`" + ` installed; since you're implementing with Claude, it'd be a genuine independent
reviewer for the ship loop."* If the only available reviewer would be the SAME model
that implements, skip it (or suggest adding a different one). Never block — it's a
suggestion, and the convention states the principle (not any tool's operational details).

### B4. Propose roles

If the workspace has multiple agents or humans collaborating on different kinds of work, suggest roles. Three common starter sets:
- Software: Planner, Implementer, Reviewer, Researcher.
- Hiring: Recruiter, Hiring Manager, Interviewer.
- Solo developer: skip roles entirely or just create Implementer.

Don't auto-create. Propose, confirm, then create with ` + "`pad role create`" + ` (or ` + "`pad_role.action: create`" + `). The agent's user can rewrite the description/icon later via ` + "`pad role update`" + `.

### B5. Activate or rewrite library playbooks

Browse the playbook library. The canonical invokable playbooks are ` + "`plan`" + `, ` + "`decompose`" + `, and ` + "`ship`" + ` (software workspaces lean on all three; non-software workspaces might want only ` + "`plan`" + `).

Browse it the same way as conventions — ` + "`pad library list --type playbooks`" + ` (CLI) or ` + "`pad_library`" + ` with ` + "`action: list, type: playbooks`" + ` (MCP). The list returns summaries by default; ` + "`pad library get \"<title>\"`" + ` / ` + "`pad_library`" + ` with ` + "`action: get, title: \"<title>\"`" + ` pulls the full body of one entry. For each that's relevant:

- Read the library body.
- Activate via ` + "`pad library activate \"<title>\"`" + ` (CLI) or ` + "`pad_library`" + ` with ` + "`action: activate, title: \"<title>\"`" + ` (MCP).
- If it needs project-specific tweaks (the seeded ` + "`ship`" + ` references ` + "`make install`" + ` — change it if the project uses ` + "`npm run build`" + ` instead), activate AND THEN immediately edit the playbook body via ` + "`pad item update <PLAYB-ref> --stdin`" + ` with the rewritten content.
- If it doesn't fit at all, skip.

### B6. Seed a first item

Help the user create their first task / idea / plan / whatever the dominant collection is. The point is to get the workspace to "user-created items > 0" state so the bootstrap nudge clears and the workspace feels lived-in. Suggest something concrete and short. Get user input on what it should be.

### B7. Recap

Summarize what was created (collections, conventions, roles, playbooks, first item). Point the user at ` + "`pad project dashboard`" + ` or the web UI. Mention they can re-run ` + "`/pad onboard`" + ` later to add or change things.

## Mode: audit (templated workspace)

The user picked an opinionated template. The collections, conventions, and playbooks are already seeded, but they're GENERIC. Your job is to walk through what's there and rewrite anything that doesn't fit.

### A1. Confirm the domain

Even with a template chosen, the user might be using it for an unusual purpose. Confirm: "You picked the ` + "`startup`" + ` template — is this a software project, or something else that fits the same collection shape?"

### A2. Walk the collections

For each seeded user-facing collection (Tasks, Ideas, Plans, Docs, etc.), ask:
- Does the name match this team's vocabulary? Some teams say Issues, not Tasks. Some say Initiatives, not Plans. Rename via ` + "`pad collection update <slug> --name \"New Name\"`" + `.
- Does the status enum match this team's lifecycle? Some teams have ` + "`new / triaged / in-progress / done`" + ` instead of the seeded set. Adjust via ` + "`pad collection update <slug> --schema '<json>'`" + ` (or the MCP form). Use the existing schema as a starting point — read it via ` + "`pad collection list --format json`" + `, edit it, send back.
- Are there fields that should be added or removed? Same ` + "`--schema`" + ` mechanism.

For each seeded collection that genuinely doesn't apply: try ` + "`pad collection delete <slug>`" + `. Default collections refuse (server returns 400). For those, suggest the user keep them empty and ignore them, or just rename to something more useful.

### A3. Walk the conventions

For each seeded convention, READ ITS BODY. Ask:
- Does the rule make sense for this project? (Some seeded conventions assume specific tooling.)
- Are the commands in the body the real ones for this project? (The library version says "run the test suite"; the seeded version might say "run ` + "`make test`" + `"; the user's project might actually use ` + "`go test -race ./...`" + ` or ` + "`npm test`" + `.)

Rewrite the body via ` + "`pad item update <CONVE-ref> --stdin`" + ` (or MCP). Delete conventions that don't apply via ` + "`pad item delete`" + `.

For software workspaces, also consider proposing the **"Independent AI code review"**
library convention if a different-model review tool is available — same check as build
mode's reviewer note (your model is the implementer; probe for/ask about a review tool;
suggest only when the reviewer model differs from yours).

### A4. Walk the playbooks

Same as conventions: read each seeded playbook's body, check whether install/test/deploy/review commands match reality, rewrite or delete.

The seeded ` + "`ship`" + ` playbook is the most common one to rewrite — its de-personalized template body references generic build/test/install steps. Replace them with the project's actual commands.

### A5. Roles

If the workspace seeded roles (some templates do, some don't), ask whether the role names + descriptions match how this team divides work. Edit via ` + "`pad role update <slug>`" + `. Delete unused roles via ` + "`pad role delete <slug>`" + `.

### A6. Recap

Same as B7: summarize what was changed, point at the dashboard.

## Mode: revisit (already-onboarded workspace)

User-created items already exist. The user is running onboard to change something specific, not to onboard from scratch.

### R1. Ask what they want to revisit

Open question: "You've onboarded this workspace already — what do you want to revisit? Common reasons people re-run this: adding a new collection, rewriting a convention, adjusting role descriptions, or activating a new library playbook."

### R2. Drop into the relevant audit branch

Once the user names the area, run JUST that part of the audit-mode flow. Don't waste their time walking through everything.

## Mode: defaults (escape hatch)

The ` + "`defaults`" + ` flag short-circuits the interview. Use the codebase-detection signals (if any) to pick:

- Domain: software if a code project is detected; otherwise prompt the user for the single highest-level question ("what kind of work?") and pick a reasonable set.
- Collections: a standard set for the inferred domain (software → Tasks/Ideas/Plans/Bugs/Docs).
- Conventions: activate library defaults for the domain, rewritten with the detected commands.
- Roles: skip unless the user explicitly opts in.
- First item: prompt the user for one sentence.

Report what was picked. Tell the user they can re-run ` + "`/pad onboard mode=audit`" + ` to walk through and adjust.

## Philosophy

- **The library is raw material, not a menu.** Adapt aggressively.
- **The workspace is the user's tool, not the agent's preference.** Confirm before deciding for them.
- **No two workspaces should look the same** even if they pick the same template. The template is a starting point; adaptation is the point of this playbook.
- **Self-aware.** If the workspace already has user-created items, don't run the full interview. Revisit-mode keeps re-invocations cheap.
`

// onboardPlaybookArguments mirrors the body's `## Arguments` section.
// Keep them in sync — the structured form is the queryable contract
// the strict CLI parser uses; the markdown is the human-readable
// mirror.
var onboardPlaybookArguments = []map[string]any{
	{
		"name":        "mode",
		"type":        "enum",
		"enum":        []string{"auto", "build", "audit", "revisit"},
		"default":     "auto",
		"description": "Force a specific onboard path. `auto` detects from workspace state. `build` is for blank workspaces, `audit` for templated workspaces with seeded content, `revisit` for already-onboarded workspaces where the user wants to change something specific.",
	},
	{
		"name":        "defaults",
		"type":        "flag",
		"description": "Skip the interview and pick sensible defaults based on detected codebase signals. Useful when the user wants the agent to 'just go.'",
	},
	{
		"name":        "skip-codebase",
		"type":        "flag",
		"description": "Skip the codebase auto-detection step. Useful for non-code workspaces (hiring, research, content) and for cases where the user wants to drive the conversation rather than have the agent volunteer detected commands.",
	},
}

// OnboardSeedPlaybook returns the onboard playbook as a SeedPlaybook
// ready for SeedCollectionsFromTemplate to insert into a new workspace
// at init time. PLAN-1496 / TASK-1500 — auto-seeded into EVERY new
// workspace regardless of template, so /pad onboard is always
// invokable on day one without the user having to activate it from
// the library first. The seed shares its body + argument spec with
// OnboardPlaybook() so the library entry and the seeded item never
// drift (same pattern ShipPlaybook uses for `ship`).
//
// trigger="manual" + scope="all" stay inside the blank template's
// minimal seeded vocabulary (BlankPlaybookTriggers / Scopes) AND
// inside every domain template's vocabulary, so the seed validates
// against the playbooks collection schema regardless of which
// template the workspace was created from.
func OnboardSeedPlaybook() SeedPlaybook {
	fields := map[string]any{
		"status":          "active",
		"trigger":         "manual",
		"scope":           "all",
		"invocation_slug": "onboard",
		"arguments":       onboardPlaybookArguments,
	}
	encoded, _ := json.Marshal(fields)
	return SeedPlaybook{
		Title:   "Onboard a workspace",
		Content: onboardPlaybookBody,
		Fields:  string(encoded),
	}
}

// OnboardPlaybook returns the library entry for the canonical
// `/pad onboard` playbook. PLAN-1496 / TASK-1499.
//
// Title is "Onboard a workspace" — verb-object format matching
// "Ship tasks" / "Plan a new initiative" / "Decompose a plan into
// tasks" in the library. The card surfaces /pad onboard as the
// invocation; the body teaches the agent what to do.
func OnboardPlaybook() LibraryPlaybook {
	return LibraryPlaybook{
		Title:          "Onboard a workspace",
		Category:       "agent-workflows",
		Trigger:        "manual",
		Scope:          "all",
		InvocationSlug: "onboard",
		Arguments:      onboardPlaybookArguments,
		Content:        onboardPlaybookBody,
	}
}
