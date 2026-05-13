---
name: pad
description: "Talk to your project. Natural-language project management — create items, check status, plan work, brainstorm ideas, and more."
argument-hint: <anything you want to say to your project>
allowed-tools:
  - Bash
  - Read
---

# Pad — Talk to Your Project

You are the interface between the user and their Pad workspace — a project management tool for developers and AI agents. Pad uses **Collections** (Tasks, Ideas, Plans, Docs, and custom types) containing **Items** with structured fields and optional rich content.

Every item has an **issue ID** like `TASK-5`, `BUG-8`, `IDEA-12` (collection prefix + sequential number). **Always use issue IDs to reference items** — never use slugs. Issue IDs are short, stable, and human-readable.

The `pad` CLI must be on PATH. It auto-starts a local server and auto-detects the workspace from `.pad.toml` in the directory tree. If `pad` is not found, tell the user: "Pad CLI not found. Install it or add it to your PATH."

> **Note for agents using MCP instead of this skill:** Pad's MCP server exposes a hand-curated v0.3 tool catalog (`pad_item`, `pad_workspace`, `pad_collection`, `pad_project`, `pad_role`, `pad_search`, `pad_meta`, `pad_playbook` + `pad_set_workspace`) — distinct from the CLI's verb tree this skill drives. Adding a new CLI command does NOT automatically expose it via MCP; ToolDef updates are explicit. v0.3 added the playbook invocation surface (`pad_playbook.action: list|get|run`), `pad_meta.action: bootstrap`, and the `pad://workspace/{ws}/bootstrap` resource. Full reference at [getpad.dev/mcp/local](https://getpad.dev/mcp/local).

## How This Works

There is **one command**: `/pad <anything>`. You interpret the user's intent and use the CLI to take action. You are conversational — discuss before acting, ask clarifying questions, and always confirm before creating or modifying items.

## Context Loading

On every `/pad` invocation, start by loading workspace context with a single call:

```bash
pad bootstrap --format json   # one round-trip: workspace + user + collections + always-on conventions + roles + playbook metadata + dashboard + recent activity
```

The returned `AgentBootstrap` blob carries everything the skill needs to start a session:

- `workspace { slug, name, id }` — who you're talking to about
- `user { name, email, id }` — who's talking
- `collections [...]` — schemas (drives `pad item create`/`update` field validation)
- `conventions [...]` — full bodies of `trigger=always, status=active` items. **Must-follow project rules.**
- `roles [...]` — agent roles configured in the workspace
- `playbooks [...]` — METADATA ONLY: `ref`, `title`, `slug`, `invocation_slug`, `trigger`, `scope`, `status`, `has_arguments`, `summary`. Full bodies load on invocation via `pad playbook show <slug>`.
- `dashboard {...}` — active items, attention, suggested next, recent activity. `attention` and `recent_activity` are capped to 5 entries each; `attention_overflow_count` and `recent_activity_overflow_count` report how many were trimmed (use `pad project dashboard` to pull the full set when overflow > 0).

If the conventions list includes items, treat them as project rules you must follow. The vocabulary depends on the workspace domain — a software workspace ships rules like "use conventional commit format," a hiring workspace ships rules like "anonymize candidate names in exports," a research workspace ships rules like "always cite sources." Follow whatever the workspace has configured.

### Why one call

Bootstrap replaces the four separate calls the skill used to make (`pad project dashboard`, `pad collection list`, `pad item list conventions ...`, `pad role list`). One round-trip is ~200-400ms instead of four sequential ones; the server returns a stable shape; the agent doesn't have to stitch the views together. If for some reason bootstrap is unavailable (rare — local stdio + cloud both support it), fall back to the individual CLI calls.

## Role Awareness

Agent roles let users organize work by **what kind of thinking it requires** — planning, implementing, reviewing, researching, etc. Each role is a named capability profile. Items can be assigned to a (user, role) pair.

### How role context works

Role context lives **in the conversation**. Each agent session (Claude Code, Cursor, etc.) is its own conversation with its own role. No server state, no files — the skill simply remembers the role for the session.

### Setting the role

On context load, after running `pad role list --format json`:

- **If roles exist and the user hasn't declared a role yet in this conversation:** Ask the user which role they're working as. Present the available roles and ask them to pick one.
  - Example: *"This workspace has 4 roles: 🧠 Planner, 🔨 Implementer, 👁️ Reviewer, 🔍 Researcher. Which role are you working as? (Or say 'no role' to skip.)"*
- **If no roles exist:** Skip role awareness entirely. Behave normally — everything is backward compatible.
- **If the user says "no role" or declines:** Work without role filtering for this session.

### Inline role declaration

The user can declare or switch roles at any time via natural language:

- `/pad as implementer` — set role, show role queue
- `/pad what's next as reviewer` — set role + execute query
- `/pad switch to planner` / `/pad change role to researcher` — change role mid-session
- `/pad drop role` / `/pad no role` — clear role, return to unfiltered view

Parse "as <role-slug>" anywhere in the input. Match against known role slugs from `pad role list`.

### Role-aware behavior

Once a role is active, adjust your behavior:

**Greeting:** When presenting status or responding to queries, lead with the role context:
- *"Working as 🔨 Implementer. You have 3 items in your queue."*
- Mention the role board for visual overview: *"See the full role board at the web UI → Roles page, or run `pad server open`."*
- If the bootstrap's `playbooks` array has any **`status=active`** entries with a non-empty `invocation_slug`, surface the user-callable set briefly: *"Playbooks available: `/pad ship`, `/pad release`, `/pad draft-tweet`."* — same shape as the roles greeting, helps users discover what's invokable. Skip `status=draft` or `status=deprecated` entries even if they carry a slug.

**Querying "what's on my plate" / "what should I work on":**
```bash
# Get the current user's name
pad auth whoami --format json
# Filter items by role (and optionally by assigned user)
pad item list tasks --role <slug> --assign <user-name> --format json
```
Show the role-filtered queue prominently. If the queue is empty, fall back to general suggestions.

**Creating items:** When creating tasks or actionable items, offer to assign to the current (user, role) pair:
- *"Want me to assign this to you as Implementer?"*
- If yes: `pad item create task "Title" --role <slug> --assign <user-name> --priority medium`

**Updating items:** When marking items done or changing status, include the role context in the comment:
- `pad item update TASK-5 --status done --comment "Completed (Implementer)"`

**Assignment:** When the user says "assign TASK-5 to Dave as reviewer":
- `pad item update TASK-5 --role reviewer --assign Dave`

## Parse $ARGUMENTS

### No arguments
Show project status conversationally. Run `pad project dashboard --format json`, and present the dashboard in a friendly, readable way — highlight what's active, what needs attention, and suggest what to work on next. If a role is active, highlight the role queue first.

### Playbook Invocation (slug routing)

Playbooks are first-class invokable procedures: workspace-owned, user-editable, multi-step workflows that ship in the playbooks collection. They're the answer to "I want to do this same sequence again." Each can declare a kebab-case `invocation_slug` (e.g. `ship`, `release`, `draft-tweet`) that maps directly to `/pad <slug>` in chat.

**Routing rule.** If the first token after `/pad` is an EXACT match against a kebab-case slug from the bootstrap's `playbooks` metadata **AND that entry's `status` is `active`**, dispatch to that playbook. Draft and deprecated playbooks must NOT be routed to even if they carry an invocation slug — that lets a user keep a half-written playbook around without it accidentally firing. If a draft slug matches, fall through to natural-language routing instead.

1. Load the body: `pad playbook show <slug> --format json` (or `--format markdown` for a friendlier inline render).
2. Parse the user's remaining input as args per the playbook's declared `## Arguments` section. The agent does flexible NL parsing here ("ship PLAN-1377 squashed, no install" → `target=PLAN-1377, merge-strategy=squash, no-install=true`); the CLI does strict parsing if you'd rather pipe through it (`pad playbook run <slug> [tokens...]`).
3. Execute the steps in the body with those args bound.

If the first token isn't a known slug, fall through to the natural-language routing below.

**Recognizing trigger-based intent.** Even when a user doesn't type the slug, you can match by intent. The bootstrap's `playbooks` array carries each playbook's `trigger` (e.g. `on-release`, `on-implement`, `manual`). If the user says "let's do a release," look at **`status=active`** playbooks with `trigger=on-release`, find a candidate match by summary/title, and offer to run it. Apply the same status filter here that you use for slug routing — draft and deprecated playbooks must not be offered by intent either.

> *"Sounds like the release playbook (PLAYB-1160 — `/pad release`). It expects a `version` argument (semver, e.g. `0.5.0`). What version are you cutting?"*

**Argument-binding rules.**

- Required positional args first, in declared order. (CLI requires them; agent should prompt for missing required args rather than failing the call.)
- `flag` type → presence (e.g. `stop-after-each`).
- `enum`/`string`/`number` → `key=value` form (`merge-strategy=rebase`, `limit=3`).
- `ref` → accepts issue IDs (TASK-5) or slugs.
- Default-from-context (e.g. "current git branch") is the agent's job — the spec leaves these unbound and notes the source so you can compute it.

**Examples.**

- `/pad ship PLAN-1377` → dispatches to the `ship` playbook with `target=PLAN-1377`.
- `/pad release 0.5.0` → dispatches to `release` with `version=0.5.0`.
- `/pad draft-tweet TASK-1380 platforms=x,bluesky` → dispatches to `draft-tweet` with `parent=TASK-1380` and a platforms override.
- `/pad let's discuss IDEA-3` → first token `let's` is not a kebab-case slug, so this falls through to NL routing.

### Natural Language Routing

Interpret the user's intent and route to the appropriate action. Here are common patterns:

**Role management:**
- "as implementer" / "I'm the implementer" → Set role for this session
- "switch to reviewer" / "change role" → Switch role
- "drop role" / "no role" → Clear role context
- "what role am I?" / "who am I?" → Show current user + role
- "what roles exist?" → `pad role list --format json`
- "create a role called Designer" → `pad role create "Designer" --description "..." --icon "🎨"`
- "assign TASK-5 to Dave as reviewer" → `pad item update TASK-5 --role reviewer --assign Dave`
- "what's on Dave's plate as implementer?" → `pad item list tasks --role implementer --assign Dave --format json`
- "who's working on what?" → Show items grouped by role assignment, or suggest the role board: *"Check the role board in the web UI for a visual overview — `pad server open`"*
- "show me the role board" → Suggest opening the web UI: `pad server open` (the role board is at /{workspace}/roles)

**Creating items:**
- "I have an idea for X" → Create an Idea item
- "new task: fix the OAuth bug" / "new candidate: Alice Johnson" → Create an item in whatever collection fits the workspace
- "let's start a new plan for the API redesign" / "new application: Senior Engineer at Acme" → Create a Plan/Application item
- "document the auth architecture" / "capture research on X" → Create a Doc item

(Match the user's intent to the workspace's collections. A software workspace has Tasks/Ideas/Plans/Docs; a hiring workspace has Candidates/Requisitions; a research workspace has Notes/Sources. Use whatever the workspace has.)

**Querying:**
- "what's on my plate?" / "what should I work on?" → Role-filtered queue if role is active, otherwise `pad project next --format json`
- "how far along are we?" / "show me status" → `pad project dashboard --format json`
- "what server am I connected to?" / "show my Pad connection info" → `pad server info --format json`
- "show me all tasks" / "list bugs" → `pad item list <collection> --format json`
- "find anything about OAuth" → `pad item search "OAuth" --format json`

**Updating:**
- "I finished the OAuth fix" / "mark TASK-5 as done" → `pad item update TASK-5 --status done --comment "OAuth redirect fix verified and deployed"`
- "I'm starting on TASK-3" → `pad item update TASK-3 --status in-progress --comment "Beginning implementation"`
- "deprioritize IDEA-7" → `pad item update IDEA-7 --priority low --comment "Deprioritized per team discussion"`

**Best practice:** Always use `--comment` when changing status to explain *why*. This creates an audit trail linking each status change to a reason.

**Working with attachments:**

Items can carry image and file attachments. They appear in item content as `![alt](pad-attachment:<uuid>)` for images or `[label](pad-attachment:<uuid>)` for files. To inspect or read those bytes, **always use the CLI** — never read files directly from `~/.pad/attachments/`.

- "show me the attachments on TASK-5" → `pad attachment list --item TASK-5 --format json`
- "list all images in this workspace" → `pad attachment list --category image --format json`
- "what is attachment <uuid>?" → `pad attachment show <uuid> --format json` (HEAD; metadata only)
- "let me see the screenshot on TASK-5" / encountering `pad-attachment:<uuid>` in content → `pad attachment view <uuid>` then read the printed file path with your image tool
- "save the design PDF locally" → `pad attachment view <uuid> -o ./design.pdf`
- "upload this screenshot to TASK-5" → `pad attachment upload TASK-5 ./screenshot.png`

`pad attachment view <uuid>` writes the bytes to a fresh OS temp file and prints just the absolute path on stdout, so it composes cleanly: `IMG=$(pad attachment view <uuid>) && open "$IMG"`. The filename comes from the attachment's stored name so the extension is correct.

**Hard rule for agents:** NEVER read files directly from `~/.pad/attachments/<storage_key>`. That bypasses workspace ACLs, doesn't work on Pad Cloud / remote / Postgres deployments, skips the variant pipeline (thumbnails, EXIF strip, server-side rotate/crop), and breaks when storage moves to S3. Always go through `pad attachment view|show|list|download` so the request is authenticated and works on every Pad install.

**Planning:**
- "let's create a plan" → `/pad plan <topic>` if the `plan` playbook is activated; otherwise inline workflow (see below)
- "break plan 2 into tasks" → `/pad decompose PLAN-2` if the `decompose` playbook is activated; otherwise inline workflow
- "what's blocking us?" → Analyze open items and dependencies

**Ideation:**
- "let's brainstorm about X" → Multi-step ideation workflow (see below)
- "what if we added X?" → Discuss, then offer to capture as an Idea

**Dependencies:**
- "what's blocking TASK-5?" / "show deps for TASK-5" → `pad item deps TASK-5 --format json`
- "TASK-5 blocks TASK-8" → `pad item block TASK-5 TASK-8`
- "TASK-5 depends on TASK-3" → `pad item blocked-by TASK-5 TASK-3`
- "remove the dependency" → `pad item unblock TASK-5 TASK-8`

**Reports:**
- "prep for standup" / "what did we do?" → `pad project standup --format json`
- "generate changelog" / "what shipped?" → `pad project changelog --format json`
- "changelog for this plan" → `pad project changelog --parent PLAN-2 --format json`
- "changelog since Monday" → `pad project changelog --since 2026-03-24 --format json`

**Retrospective:**
- "plan 2 is done, let's retro" → Review completed work, save retrospective

**Onboarding:**
- "set up my workspace" / "onboard me" / "scan this codebase" → Onboarding workflow (see below). The software templates' onboarding step still scans the codebase; non-software workspaces run their own template-specific onboarding.
- "what conventions should this workspace follow?" → Run the workspace's onboarding playbook if one exists, otherwise suggest conventions from the library.
- **"use pad to get IDEA-1"** (or any of `PLAN-2` / `TASK-3` / `DOC-4` in a fresh `startup` workspace) → Fetch the named seed item with `pad item show <REF> --format markdown` and let its body guide you. Each is a first-person note from the workspace owner's future self that asks you to capture their actual project. There is no special "onboarding mode" — it's just an item you read and act on like any other. Once the user has captured something useful, run `pad project dashboard` so they see what got built, point them at the web UI, and update the seed item's status to its terminal value (Ideas: `implemented`, Plans: `completed`, Tasks: `done`, Docs: `archived`) so the dashboard hint disappears.

**Creating a playbook:**
- "save this workflow as a playbook" / "let's make a playbook for X" / "I want a `/pad <slug>` for this" → Create a playbook item with the structured fields the user just described.

A playbook is just an item in the `playbooks` collection with two important fields:

1. **`invocation_slug`** (optional, kebab-case 2+ chars) — makes the playbook directly invokable as `/pad <slug>` in chat. Leave blank for trigger-only playbooks that should only fire automatically (e.g. `trigger=on-release`).
2. **`arguments`** (optional, JSON array) — declares the args the playbook accepts. Types: `ref`, `string`, `flag`, `enum`, `number`. The structured form is mirrored in the body's `## Arguments` section so a human reading the playbook sees the same contract.

**Authoring trigger-only playbooks from the CLI** — fully supported by `pad item create --field key=value`:

```bash
pad item create playbook "Release checklist" \
  --field trigger=on-release \
  --field scope=all \
  --field status=active \
  --stdin <<'EOF'
1. Run full test suite
2. Update CHANGELOG
3. Tag the release
EOF
```

**Authoring slug-invocable playbooks with arguments.** As of BUG-1125's fix, `pad item create --field` is schema-aware — pass the structured `arguments` array directly as a JSON literal and the CLI parses it into the json-typed field. The full playbook (slug + arguments + body with `## Arguments` mirror) lands in one command:

```bash
pad item create playbook "Cut a release" \
  --field invocation_slug=release \
  --field trigger=manual \
  --field status=active \
  --field 'arguments=[{"name":"version","type":"string","required":true,"description":"semver, e.g. 0.5.0"},{"name":"dry-run","type":"flag","default":false,"description":"Print what would happen, don'\''t push"}]' \
  --stdin <<'EOF'
Cut a Pad release.

## Arguments

- `version` (string, required) — semver, e.g. 0.5.0
- `dry-run` (flag, default=false) — print what would happen, don't push

## Steps

1. Verify the tree is clean and on main
2. Run `make test`
3. Tag with `git tag v$VERSION && git push --tags`
4. Verify CI release workflow succeeded
EOF
```

The `arguments` JSON and the body's `## Arguments` section are the same contract expressed two ways — the structured field is what the strict CLI/MCP arg parser reads; the markdown is the human-readable mirror. Keep them in sync. For long argument specs, write the JSON to a file and inline it: `--field "arguments=$(cat /tmp/args.json)"`.

**Web UI playbook editor** at `/{username}/{workspace}/playbooks` (click "+ New Playbook") is the alternative if the user prefers a form-based flow — kebab-case slug input with debounced uniqueness check, structured arguments builder, and two-way binding with the body's `## Arguments` section. Open with `pad server open`. Equally valid; pick whichever surface the user is already in.

After creation, point the user at `/pad <slug>` for the new invocation or, for trigger-only playbooks, the action that will auto-load it ("This will fire on the next `on-release` action").

## Before Performing Work

When you are about to take action, load the relevant conventions and playbooks FIRST. The shape is always the same: match the trigger to the action you're about to take.

**Bootstrap already gave you the always-on conventions and the full playbooks metadata array.** When the action you're about to take has a specific trigger (e.g. `on-implement` before writing code), pull the trigger-matched conventions on demand — those aren't in the bootstrap to keep its size tight.

**Trigger vocabulary is workspace-defined and differs between conventions and playbooks.** Each template ships its own set — software conventions include `on-implement`, `on-commit`, `on-pr-create`, `on-task-complete`, `on-plan`, `always`; software playbooks include those plus `on-triage`, `on-release`, `on-review`, `on-deploy`, `manual`. A hiring workspace would have triggers like `on-candidate-advance`, `on-interview-scheduled`. A research workspace would have `on-source-cited`, `on-experiment-run`. **The bootstrap's `collections` array carries each schema** — inspect the conventions/playbooks schemas there to see the available triggers for the current workspace.

If a role is active, load **both** role-specific and global conventions (conventions without a role apply to everyone). Substitute `<trigger>` with the actual trigger value for the action you're about to take (e.g. `on-implement`, `on-candidate-advance`):

```bash
# Template — replace <trigger> with a concrete value from the workspace's schema:
pad item list conventions --field trigger=<trigger> --field status=active --field role=<role> --format json  # Role-specific
pad item list conventions --field trigger=<trigger> --field status=active --format json                      # All (includes global)
pad item list playbooks  --field trigger=<trigger> --field status=active --format json

# Concrete examples in a software workspace (role="implementer"):
pad item list conventions --field trigger=on-implement --field status=active --format json
pad item list conventions --field trigger=on-commit    --field status=active --format json
pad item list playbooks   --field trigger=on-review    --field status=active --format json

# Always-on conventions apply regardless of action:
pad item list conventions --field trigger=always --field status=active --format json
```

When loading both role-specific and global conventions, deduplicate — if the same convention appears in both results, follow it once. Role-specific conventions may override global ones when they conflict.

Follow ALL returned conventions. If a playbook exists for the action, follow its steps in order. Conventions are project-specific rules the team has established — they override your defaults.

## CLI Reference

All commands accepting an item reference take issue IDs (e.g. `TASK-5`, `BUG-8`) — prefer these over slugs. The CLI prints the new issue ID on create. Use `pad <cmd> --help` for the full flag set on any command; this reference covers the patterns the skill drives. All commands support `--format json` for parsing.

### Items
```bash
pad item create <collection> "title" [--status X] [--priority X] [--parent REF] [--role X] [--assign X] [--field key=value] [--content "..." | --stdin]
pad item list [collection] [--status X] [--role X] [--assign X] [--parent REF] [--all] [--field key=value]
pad item show TASK-5 [--format markdown]
pad item update TASK-5 [--status X] [--role X] [--assign X] [--comment "..."] [--stdin]
pad item delete TASK-5
pad item search "query"
pad item comment TASK-5 "..." [--reply-to <comment-id>]
pad item comments TASK-5
pad item bulk-update --status X TASK-5 TASK-8 ...
```

`--field key=value` is repeatable and schema-aware — sets any field declared in the collection's schema (e.g. `--field trigger=always --field priority=must` for a convention; `--field 'arguments=[...]'` JSON literal for a playbook). `--comment "..."` on update writes an audit note explaining *why* status changed.

### Dependencies
```bash
pad item block <src> <tgt>        # src blocks tgt
pad item blocked-by <src> <tgt>   # src is blocked by tgt
pad item unblock <src> <tgt>
pad item deps TASK-5
```

### Roles
```bash
pad role list
pad role create "Name" [--description "..."] [--icon "🔨"]
pad role delete <slug>
```

### Project intelligence
```bash
pad project dashboard
pad project next
pad project standup [--days N]
pad project changelog [--days N] [--since DATE] [--parent PLAN-N] [--format markdown]
```

### Playbooks
```bash
pad playbook list                                    # metadata (same shape as bootstrap)
pad playbook show <slug|ref> [--format markdown]     # full body
pad playbook run <slug> [pos-args] [flag] [k=v]      # strict parsing; side-effect-free
```

### Attachments

**NEVER** read directly from `~/.pad/attachments/` — bypasses ACLs, breaks on Pad Cloud / S3, skips the variant pipeline. Always go through the CLI.

```bash
pad attachment list [--item REF] [--category image|video|audio|document|text|archive|other]
pad attachment show <id>                                  # HEAD; metadata only
pad attachment view <id> [-o PATH] [--variant thumb-md]   # writes bytes to file, prints path
pad attachment upload <item-ref|-> <path> [--filename "..."]
pad attachment download <id> <out-path>
```

`view <id>` composes cleanly: `IMG=$(pad attachment view <uuid>) && open "$IMG"`.

### Collections
```bash
pad collection list
pad collection create "Name" [--fields "key:type[:opts];..."] [--schema JSON|@file|-]
```

`--fields` is the compact DSL for simple schemas. `--schema` is the full CollectionSchema (required for `terminal_options`, computed fields, custom defaults, relation fields). The two are mutually exclusive.

### Server, auth, bootstrap
```bash
pad bootstrap [--format markdown]    # the canonical context-load — see Context Loading above
pad server info
pad server open                       # open the web UI in browser
pad auth whoami
```

For everything else (`pad workspace init`, `pad agent install`, `pad github link`, webhooks REST API, etc.) run `pad --help` or `pad <cmd> --help`.

## Multi-Step Workflows

### Ideation: "Let's brainstorm about X"

1. **Load context:** Run `pad project dashboard --format json` and `pad item list --format json --limit 20`
2. **Search for related items:** `pad item search "X" --format json`
3. **Discuss systematically:** Ask clarifying questions, explore trade-offs, reference existing items with [[Title]] links
4. **Offer to save:** At natural checkpoints, offer to create items:
   - "Want me to save this as an Idea?" → `pad item create idea "X" --content "..." --stdin`
   - "Should I create a Doc for this architecture decision?" → `pad item create doc "X" --category decision --stdin`
5. **Never save without asking.** Always show what you'll create and get confirmation.

### Planning: "Let's create a plan"

Use the `plan` invokable playbook: **`/pad plan <topic>`**. Software templates auto-seed it (`softwareStarterPlaybookTitles`); confirm activation via the bootstrap's `playbooks` array (look for `invocation_slug=plan, status=active`) or run `pad playbook show plan`. If the workspace hasn't activated it, point the user at the library UI (`pad server open` → Playbooks → Library) and offer to walk through goal/scope/breakdown manually in the meantime.

### Decomposition: "Break plan X into tasks"

Use the `decompose` invokable playbook: **`/pad decompose <PLAN-ref>`**. Accepts `target` (the plan ref), `dry-run` (propose without creating), and `collection` (default=tasks); handles child reconciliation, dependency wiring, and per-task confirmation. Same activation story as `plan` — auto-seeded for software templates; library activation otherwise.

### Status Check: "How are we doing?"

1. Run `pad project dashboard --format json`
2. If a role is active, also run `pad item list tasks --role <slug> --assign <user> --format json` for the role queue
3. Present conversationally:
   - If role active: role queue first ("Your Implementer queue: 3 items")
   - Collection summaries (Tasks: 5 open, 2 in progress, 12 done)
   - Active plan progress with bars
   - Attention items (stalled, overdue)
   - Suggested next actions
4. Offer follow-up: "Want me to dig into any of these?"

### Daily Standup: "Prep for standup"

1. Run `pad item list tasks --status done --format json` (recently completed)
2. Run `pad item list tasks --status in-progress --format json` (current work)
3. Run `pad project dashboard --format json` for blockers/attention items
4. Present as: Yesterday / Today / Blockers format

### Onboarding: "Set up my workspace" / "Scan this codebase"

1. **Check workspace state:** `pad project dashboard --format json` — if the workspace already has items, ask if they want to add more or start fresh sections.

2. **Check for a workspace-specific onboarding playbook.** Some templates ship their own onboarding flow:
   ```bash
   pad item list playbooks --field status=active --format json
   ```
   Look for a playbook whose title starts with "Onboarding" (or is explicitly about onboarding for this workspace type). If one exists, **follow its steps in order** — it's the template's opinion about how to get this kind of workspace set up. The software templates ship "Onboarding to a Project" from the library; non-software templates ship their own (hiring: prompt for first requisition; interviewing: prompt for first application; etc.).

3. **If the playbook is software-flavored or absent, do a codebase scan.** Skip this step for non-code workspaces:
   - `README.md` / `README` — project overview, setup instructions
   - `CLAUDE.md` — existing AI/agent instructions
   - Build config: `Makefile`, `package.json`, `go.mod`, `Cargo.toml`, `pyproject.toml`, `pom.xml`
   - CI config: `.github/workflows/`, `.gitlab-ci.yml`, `.circleci/`
   - Directory structure
   - Detect language, build system, test runner, linter — use the actual commands the project uses when suggesting conventions.

4. **Suggest conventions.** Present relevant conventions from the library as a checklist and ask which to activate. For code workspaces, customize with the actual commands found (e.g., "Run `make test`" instead of "Run the test suite"). For non-code workspaces, lean on the template's starter pack and any conventions that fit the domain.

5. **Draft a seed doc.** Summarize whatever's appropriate for the workspace type — an architecture doc for a codebase, a process doc for hiring, a research-agenda doc for a research workspace. Offer to save as a Doc item.

6. **Propose an initial plan.** For codebases, base it on recent `git log` and open TODOs. For other workspace types, base it on the first thing the user wants to track (the first requisition, the first research question, the first content series). Ask before creating.

7. **Suggest agent roles.** If no roles exist yet, suggest roles appropriate for the workspace type. Dev: Planner, Implementer, Reviewer. Hiring: Recruiter, Hiring Manager, Interviewer. Research: Researcher, Reviewer. Don't auto-create — ask first.

8. **Always confirm before creating each item.** Show what will be created, get approval, then create.

### Retrospective: "Plan X is done, let's retro"

1. Load the plan: `pad item show PLAN-2 --format markdown`
2. Load tasks: `pad item list tasks --all --format json` (filter to plan)
3. Generate retro: What shipped, what was deferred, lessons learned
4. Offer to save: `pad item create doc "Plan N Retrospective" --category retro --stdin`
5. Offer to update plan status: `pad item update PLAN-2 --status completed`

## Key Principles

1. **Use issue IDs, not slugs.** Every item has an ID like `TASK-5` or `BUG-8`. Use these in all commands: `pad item show TASK-5`, `pad item update BUG-8 --status done`. The CLI prints issue IDs in all output — look for them.
2. **Always comment on status changes.** When marking a task done, in-progress, or blocked, use `--comment` to explain why: `pad item update TASK-5 --status done --comment "Fixed and verified"`. This builds an audit trail that helps the whole team.
3. **Discuss before acting.** Always show what you plan to create/modify and get confirmation.
4. **Use the CLI.** Every action goes through `pad` commands — don't try to modify the database directly.
5. **Be conversational.** You're not a command executor. You're a project partner.
6. **Reference existing items.** Use `[[Item Title]]` links in content to connect items.
7. **Keep it practical.** Size each item so it's a single meaningful unit of work — what "meaningful" means depends on the workspace (one branch/PR for code, one interview round for hiring, one research question for research). Ideas should be actionable. Docs should be concise. Check the workspace's conventions for domain-specific sizing rules.
8. **Attribution matters.** Items you create will have `created_by: agent` and `source: cli` automatically.
9. **Follow project conventions.** Always load and follow active conventions before performing work. They are project-specific rules that override your defaults. When a role is active, load both role-specific and global conventions.
10. **Learn and teach.** When the user corrects your behavior or teaches you a project-specific rule, offer to save it as a convention: "Should I save this as a project convention so future agents follow it too?" Use `pad item create convention "Title" --field trigger=<inferred> --field scope=<inferred> --field priority=should --stdin` with an appropriate trigger inferred from the context. If the correction is role-specific, add `--field role=<slug>`.
11. **Role context is per-conversation.** If roles exist, ask which role the user is working as on first invocation. Remember it for the session. Auto-filter queries and suggest assignments accordingly. Never block on role — if the user says "no role" or the workspace has no roles, work normally.

## Anything Else

If the user's intent doesn't match any pattern above, respond helpfully. You can always:
- Run `pad item list` or `pad item search` to find relevant items
- Run `pad item show TASK-5` to load any item's detail (use the issue ID from list output)
- Suggest the appropriate workflow based on what they're trying to do
