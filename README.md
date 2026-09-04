<p align="center">
  <h1 align="center">Pad</h1>
  <p align="center"><strong>Project Management for the agent era.</strong></p>
  <p align="center">
    <a href="https://github.com/PerpetualSoftware/pad/actions/workflows/ci.yml"><img src="https://github.com/PerpetualSoftware/pad/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
    <a href="https://github.com/PerpetualSoftware/pad/releases"><img src="https://img.shields.io/github/v/release/PerpetualSoftware/pad" alt="Release"></a>
    <a href="https://goreportcard.com/report/github.com/PerpetualSoftware/pad"><img src="https://goreportcard.com/badge/github.com/PerpetualSoftware/pad" alt="Go Report Card"></a>
    <a href="https://github.com/PerpetualSoftware/pad/pkgs/container/pad"><img src="https://img.shields.io/badge/ghcr.io-perpetualsoftware%2Fpad-blue?logo=docker&logoColor=white" alt="Container image on GHCR"></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache%202.0-blue" alt="License"></a>
    <a href="https://github.com/sponsors/xarmian"><img src="https://img.shields.io/github/sponsors/xarmian?label=sponsors&logo=github" alt="GitHub Sponsors"></a>
  </p>
  <p align="center">
    <a href="https://getpad.dev">Website</a>
    &nbsp;·&nbsp;
    <a href="https://getpad.dev/docs">Docs</a>
    &nbsp;·&nbsp;
    <a href="https://getpad.dev/blog">Blog</a>
    &nbsp;·&nbsp;
    <a href="https://getpad.dev/changelog">Changelog</a>
    &nbsp;·&nbsp;
    <a href="https://www.reddit.com/r/getpad/">Reddit</a>
    &nbsp;·&nbsp;
    <a href="https://x.com/getpaddev">X</a>
    &nbsp;·&nbsp;
    <a href="https://bsky.app/profile/getpaddev.bsky.social">Bluesky</a>
  </p>
</p>

---

> One binary. Local-first. No accounts required. Pad gives you a CLI, a web UI, and an AI agent skill — all backed by SQLite, all running on your machine. Your project data stays on your laptop — unless you take it to [Pad Cloud](https://app.getpad.dev).

<p align="center">
  <img src="docs/screenshots/dashboard.png" width="900" alt="Pad dashboard showing collection summaries, active work, an active plan with progress, and a recent activity feed" />
</p>

## Quick Start

```bash
brew install PerpetualSoftware/tap/pad
cd your-project
pad init                    # configure, auth, workspace, AI skill — all in one
pad server open             # opens the web UI at localhost:7777
```

`pad init` is the smart entry point — it auto-detects what's needed, walks you through each step, and is safe to re-run anytime (it skips finished steps and prints a status summary).

Then, in a fresh agent session in your project, say:

```
/pad onboard
```

Your new workspace ships with the canonical `onboard` playbook auto-activated. The agent walks an interview, inspects your codebase if it has shell access, and adapts your workspace's collections, conventions, roles, and playbooks to match the project. It's the fastest way to go from empty workspace to "okay, this is mine."

## Why Pad?

Tools like Linear, Jira, and Notion are built for teams on the cloud. Pad is built for **developers on their machine** — and for the AI agents working alongside them. When you do want your projects on every device or a teammate on the board, [Pad Cloud](https://app.getpad.dev) hosts the same product with sync, workspace invites, and role-based access.

| | Pad | Linear / Jira | Notion |
|---|---|---|---|
| **Setup** | `pad init` | Create account, invite team, configure | Create account, pick template |
| **AI agents** | Native `/pad` skill for 7+ tools | Third-party integrations | Third-party integrations |
| **Data** | Local SQLite you own — or opt-in Pad Cloud | Their cloud | Their cloud |
| **Offline** | Full functionality | Read-only cache at best | Limited |
| **CLI** | First-class | Afterthought | None |
| **Price** | Free, open source | Per-seat pricing | Per-seat pricing |

## Features

### For Developers

**CLI that doesn't get in your way.** Create tasks, search items, check status — without leaving the terminal.

```bash
pad item create task "Fix OAuth redirect" --priority high
pad item create idea "Real-time collaboration" --category infrastructure
pad item list tasks --status in-progress
pad item search "authentication"
pad project dashboard                   # Project dashboard
pad project next                        # What should I work on?
pad server info                         # How this client is connected to Pad
```

**Web UI that stays out of your way.** A clean, dark-themed interface at `localhost:7777` with:

- **Board, list, and table views** — drag-and-drop between status columns
- **Keyboard navigation** — `j`/`k` to move, `Enter` to open, `Esc` to go back, `Cmd+K` to search
- **Rich text editor** — Tiptap-based with markdown, formatting toolbar, and auto-save
- **Wiki-links** — type `[[Title]]` to link between items
- **Real-time updates** — agent creates a task in the terminal, it appears in the browser instantly (via SSE)
- **Dashboard** — collection overview, active work, plan tracking, activity feed

<p align="center">
  <img src="docs/screenshots/board.png" width="900" alt="Pad tasks board view: kanban columns for Open, In-Progress, Done, Cancelled with task cards in each" />
</p>

### For AI Agents

**Your agent becomes a project partner.** Install the `/pad` skill once, and your AI coding tool can read, create, and update project items through natural language.

```bash
pad agent install        # Auto-detects your tools and installs the skill
```

Works with **Claude Code**, **Cursor**, **Windsurf**, **Codex**, **OpenCode**, **GitHub Copilot**, **Amazon Q**, and **JetBrains Junie**.

Then just talk to your project:

```
> /pad what should I work on next?
> /pad I finished the OAuth fix
> /pad create a task to add rate limiting
> /pad let's brainstorm about the API redesign
```

**Conventions and playbooks** teach agents how your project works:

- **Conventions** — trigger-based rules like "run tests before marking a task done" or "use conventional commits"
- **Playbooks** — multi-step workflows like "when implementing a feature: read the spec, create a branch, write tests first, then implement". Playbooks can declare a kebab-case `invocation_slug` so users can invoke them directly: `/pad ship PLAN-42`, `/pad release 0.5.0`. Fresh `startup` workspaces ship a generic `ship` playbook out of the box.

```bash
pad item create convention "Run tests before completing tasks" \
  --field trigger=on-task-complete \
  --field scope=all \
  --field priority=must
```

Agents load relevant conventions automatically, and every agent action is attributed in the activity feed — so you can see what the AI changed rather than finding it later in a diff.

**Name your agents:**

An agent that identifies itself gets its name shown on its writes — in the activity feed's Live and Audit views, on the dashboard's recent activity, on item timeline *activity* entries, and in the admin console's audit log and per-user activity views. With more than one agent working a project, that is the difference between "something automated touched this" and knowing which one.

Pad takes the first of these it finds:

```bash
# 1. Per-workspace, committed with the project — the deliberate choice.
#    In .pad.toml:
#      agent_name = "reviewer"

# 2. Per-process, runtime-agnostic. Any harness can set it.
export PAD_AGENT=reviewer

# 3. Otherwise Pad detects the runtimes it knows — Claude Code reports
#    "claude-code" — and that detected id is used as the name.

# 0. Per-session, and ahead of all three: the name this session REGISTERED
#    as. `pad session register --agent rook` re-attributes every later write
#    from that session to "rook", whatever .pad.toml or $PAD_AGENT say — the
#    registry row and the write stamp are one value, not two.
pad session register --agent rook
```

**If none of these produce a name, the write is not marked as an agent's at all** — it is recorded as the person whose credentials it used, which is the case the caveat below is about. The generic `agent` label you may see on older entries is a write that identified itself before Pad stored names, or an event type that records the actor without the name (workspace membership changes, sign-ins).

The name is rendered exactly as sent — Pad keeps no list of approved names, and does not re-case or rewrite what you choose.

**Sessions carry the name too, locally.** A session with the Claude Code plugin records itself in `~/.pad/sessions` on start (best effort — the plugin monitor is silent by contract, so a registration that fails, e.g. on a malformed pid variable, is only visible by running `pad session register` by hand) — the harness session's pid, the agent name above, and its working directory — and `pad session list` reads that back with a liveness verdict per row (`alive`, `dead`, or `unknown` where the platform cannot probe). It is a local, deterministic answer to "which of my sessions on this machine are running, and as which agent" — no server round-trip, no guessing from process names. What a row says about *who* is self-declared, like the name itself; on Linux the pid claim is additionally checked against the registering process's ancestry and reported as `session_pid_verified`. Any other harness gets the same by calling `pad session register` from its session-start hook with `PAD_SESSION_PID` (the session process) and `PAD_AGENT` exported. Records of sessions the register can see are dead are pruned on every register; `pad session prune --older-than 72h` also clears ones whose liveness cannot be determined. The record never leaves the machine.

Reading the output as a decision — "is this name in use here right now?" — takes a rule, and `pad session list --help` spells it out: count only rows that are `alive`, not `legacy`/`malformed`, and `session_pid_verified`; treat `unknown`, legacy, or malformed rows in the same directory as indeterminate rather than free (so list without `--agent` and filter yourself); read an empty result as "no registered row", not "nobody" — a harness that never registers is invisible; and never pick between two alive rows by `registered_at`, which is each session's own clock. The registry is per OS user.

Not every entry can show it. Activity entries store it, and comments (replies included) read it through the activity each one links to — so a comment written by an agent that sent a name shows that name in its chip, next to the person whose credentials it used. Version snapshots and implementation-note/decision entries record only *that* an agent acted, because nothing links them to a named row — they still read `Agent`.

**What this does not claim.** The name is supplied by the client and self-declared, so it records honesty, not identity. From `ResolveAgentName`'s own contract in `internal/cli/agent_identity.go`:

> - an agent that omits it is indistinguishable from the human whose credentials it is using;
> - a human running `! pad ...` inside an agent's terminal inherits that terminal's environment and will be attributed to the agent.

So it is not a basis for machine-verifiable provenance: treat it as a label an actor chose, useful for reading a trail, not as evidence about who acted. Because the credentials belong to a person either way, surfaces that exist for provenance show both — the admin audit log renders `reviewer (via Dana)` rather than picking one.

Since the name is chosen by whoever is writing, it is displayed as an isolated unit: it is shown as sent, but it cannot re-order or restyle the text around it, and the account half of `name (via account)` is rendered separately so a chosen name cannot forge it.

**Onboard agents to a new codebase:**

Open an agent session in the workspace directory and run `/pad onboard`. The agent walks an interview, detects your build/test/CI tooling, and adapts your workspace's collections, conventions, roles, and playbooks to match the project. Works for any agent that speaks Pad — Claude Code, MCP-only agents, etc.

### Collections & Custom Fields

Pad organizes work into **collections** — typed containers with structured fields.

**Built-in collections:**

| Collection | Purpose |
|---|---|
| **Tasks** | Work items with status, priority, assignee, effort, due date |
| **Ideas** | Feature ideas with impact and category |
| **Plans** | Project milestones with progress tracking |
| **Docs** | Documentation, decisions, reference material |
| **Conventions** | Project rules that guide agent behavior |
| **Playbooks** | Multi-step workflows for agents to follow |

**Create your own** with typed fields — select, text, date, number, url, relation, checkbox:

```bash
pad collection create "Bug Reports" \
  --fields "severity:select:low,medium,high,critical; browser:text; reproducible:checkbox"
```

Items get reference numbers automatically (`TASK-5`, `BUG-12`) and can be moved between collections with field migration.

## Installation

### Homebrew (macOS and Linux)

```bash
brew install PerpetualSoftware/tap/pad
```

### Build from Source

```bash
git clone https://github.com/PerpetualSoftware/pad
cd pad
make build
cp pad ~/.local/bin/   # or /usr/local/bin/
```

Requires Go 1.26+ and Node.js 22+. Alternatively, `nix develop` provides a shell with the exact Go and Node versions pinned — see the [Nix](#nix) section below.

The `go install github.com/PerpetualSoftware/pad/cmd/pad@latest` path is not supported for the full Pad binary, because the web UI must be built and embedded during the source build.

### Docker

```bash
docker run -p 127.0.0.1:7777:7777 -v pad-data:/data ghcr.io/perpetualsoftware/pad
```

This publishes Pad to `localhost:7777` on the host machine, which is the recommended default for local use.

**First run — create the first admin.** Open `http://localhost:7777` and you'll hit a setup page asking for a bootstrap token. On first start with no users, Pad logs a one-time setup URL to stderr (captured by `docker logs`) — grep it and open the printed link:

```bash
docker logs <container> 2>&1 | grep -A6 'Pad first-run setup'
# → http://<your-host>:7777/setup#token=<one-time-token>
```

Open that URL, create your admin account, and the token is consumed (the banner stops appearing). If you'd rather stay on the CLI, `docker exec -it <container> pad auth setup` works too — running inside the container counts as loopback, which the bootstrap gate allows. On a network you already trust, set `PAD_BYPASS_SETUP_TOKEN=true` to skip the token and create the admin straight from `http://<your-host>:7777/setup` (only safe when the port isn't reachable from the open internet).

**Single user, more than one device?** Publish to all interfaces so you can reach Pad from your phone, tablet, or another machine on the same LAN, Tailscale network, or home VPN:

```bash
docker run -p 7777:7777 -v pad-data:/data ghcr.io/perpetualsoftware/pad
```

For multi-instance deployments, Pad supports Postgres + Redis via `docker-compose.yml` — see [docs/deployment.md](docs/deployment.md) for the full setup.

### Nix

Run without installing:

```bash
nix run github:PerpetualSoftware/pad
```

Or install into your profile:

```bash
nix profile install github:PerpetualSoftware/pad
```

A flake devShell (Go, Node, and friends, pinned to the same versions CI uses) is also available for contributors:

```bash
nix develop
```

> A `nixpkgs` package (`nix-shell -p pad` / `environment.systemPackages`) is planned but not yet merged upstream. Until then, use the `github:PerpetualSoftware/pad` flake reference above.

### Binary Download

Pre-built binaries for macOS, Linux, and Windows are available on the [releases page](https://github.com/PerpetualSoftware/pad/releases).

### Pad Cloud (hosted)

Don't want to run anything? [Pad Cloud](https://app.getpad.dev/register) is the managed option — same product, same CLI, same `/pad` skill, free during beta. Sign up on the web, then connect a project directory:

```bash
pad init --url https://app.getpad.dev --workspace my-workspace
```

Self-hosting stays first-class: the binary is unchanged and no features are Cloud-only.

### Upgrading Pad

Pad ships a new binary on a roughly weekly cadence. Upgrades are designed to be boring: install the new binary and restart. Database migrations run automatically at startup, only the ones your database is missing are applied, and each migration commits atomically (a failed migration rolls back cleanly and is retried next boot).

**The one rule: only ever move forward.** Newer binaries know how to migrate an older database; older binaries do **not** understand a newer schema. Since Pad added its schema-ahead guard, a downgraded binary that finds a database newer than itself refuses to start rather than silently running old code against a newer schema (which can corrupt data):

```
database schema is newer than this pad binary: ... This almost always means the
binary was DOWNGRADED (e.g. brew/docker rollback) ... Upgrade pad back to a build
that includes those migrations, or re-run with `pad start --force`.
```

To recover, reinstall the newer binary (`brew upgrade pad`, pull the newer Docker tag, etc.). If you have *intentionally* downgraded and accept the risk, start with `pad start --force` (or set `PAD_ALLOW_SCHEMA_AHEAD=1`) to override the guard.

**Automatic pre-migration snapshot (SQLite).** Whenever a SQLite-backed instance has pending migrations to apply, Pad first copies the database file to `pad.db.pre-<version>` next to it. If an upgrade ever goes wrong, stop the server and copy that snapshot back over `pad.db`. This is a convenience net, not a backup strategy — keep your own backups (see [docs/backup.md](docs/backup.md)). PostgreSQL instances are skipped here; use `pg_dump` or a provider snapshot before upgrading.

Recommended upgrade flow:

```bash
# 1. Back up first (SQLite shown; see docs/backup.md for Postgres)
pad db backup -o pad-backup-$(date +%Y%m%d).db

# 2. Stop the server, install the new binary, restart
#    (migrations + the pre-migration snapshot run automatically on start)
brew upgrade pad        # or: docker pull, binary download, make install

# 3. Confirm it's healthy
pad --version
curl -s localhost:7777/api/v1/health
```

## Getting Started

### 1. Set up Pad

```bash
cd ~/projects/myapp
pad init "My App"
```

`pad init` is the smart entry point that handles everything in one command:

- Configures this client's connection (local server, remote, or Docker)
- Auto-starts the local server
- Creates the first admin account on a fresh local install (Docker / remote hosts run `pad auth setup` on the server instead)
- Logs you in if needed
- Creates or links a workspace for the current directory (writes `.pad.toml`)
- Installs the `/pad` skill for any AI tools detected in the project

Run from your project root. Safe to re-run anytime — it skips finished steps and prints a status summary if nothing's needed.

**Choose a template** with `--template`, or omit it for an interactive picker grouped by category (Software / People / …):

```bash
pad workspace init --list-templates                   # See the full catalog grouped by category
pad init "My App" --template scrum                    # Scrum-style with sprints
pad init "My App" --template product                  # Product management focused
pad init "My Hiring" --template hiring                # Company-side: requisitions, candidates, interview loops, feedback
pad init "Job Search" --template interviewing         # Candidate-side: applications, interviews, companies, contacts
pad init "My App" --template blank                    # Custom: system collections only — let /pad onboard build the rest
```

Pad ships templates for software (startup / scrum / product), people workflows (hiring, interviewing), and a custom `blank` template — system collections (Conventions, Playbooks) only, with the `/pad onboard` playbook as its sole seeded content. `blank` is the entry point for the agent-driven `/pad onboard` flow: it walks you through shaping collections, conventions, and roles to match your actual project. Reserved categories for research, content, operations, and personal use await their first templates, so the same project-management primitives fit well beyond code projects. There's also a hidden `demo` template — the `startup` layout pre-loaded with realistic sample data — that's kept out of the picker but can be built explicitly with `--template demo`.

### 2. Start working

```bash
# From the CLI
pad item create task "Set up CI pipeline" --priority high
pad item create idea "Add WebSocket support" --category infrastructure
pad project dashboard

# From the web UI
pad server open              # Opens localhost:7777 in your browser

# From your AI agent
# Just use /pad in Claude Code, Cursor, etc.
```

### 3. Teach your agents the rules

In an agent session inside the workspace:

```
/pad onboard
```

The agent walks an interview, detects your tooling, and adapts the workspace's collections, conventions, roles, and playbooks. To browse the library directly:

```bash
pad library list --type conventions  # Pre-built conventions you can adopt
pad library list --type playbooks    # Pre-built multi-step workflows
```

### 4. Optional — connect a desktop AI app via MCP

Pad ships an MCP (Model Context Protocol) server so Claude Desktop, Cursor,
Windsurf, Claude Code, or Codex can manage items, plans, ideas, and dependencies
as native tools, read workspace state by URL, and load multi-step workflows as
prompts.

```bash
pad mcp install claude-desktop   # or: cursor, windsurf, claude-code, codex, --all
# Restart the client; pad shows up as the "pad" MCP server.
```

`pad mcp install` writes each client's native config: JSON `mcpServers` for
Claude Desktop / Cursor / Windsurf, a **project-local `.mcp.json`** in the current
directory for `claude-code`, and an `[mcp_servers.pad]` table in
`~/.codex/config.toml` (TOML) for `codex`. Because Claude Code's config is
project-scoped, it's install-on-request only — `--all` and `pad mcp status` cover
the per-user clients (including Codex) and skip it.

**Tool catalog (v0.29)** — ten resource × action tools plus `pad_set_workspace` (eleven total), no flat verb explosion. Undeclared input keys are rejected with a structured error rather than silently dropped. `pad_item` create/update accept field values as a `fields` object (the same shape reads return) as an equivalent to the dedicated params / `field: ["key=value"]`, and its values keep their JSON types where the transport can carry them. Field values are typed against the collection schema server-side, so a declared number or json field is writable from the remote transport (which sends every value as a string). Keys the schema does not declare are stored and NAMED back in `warnings.undeclared_fields`. One key supplied through two doors is adjudicated once: differing values are refused, equal ones collapse, and two names for the same target — `parent`/`plan`, `assign`/`assigned_user_id`, `role`/`agent_role_id` — are refused even when the values match. `pad_item.list` accepts `unparented: true` (mutually exclusive with `parent`) to select items with no parent or implements relationship, and is summary-shaped by default on both transports (`full: true` opts into complete content bodies):

| Tool | Actions |
|---|---|
| `pad_item` | `create`, `update`, `delete`, `get`, `list`, `move`, `restore`, `link`, `unlink`, `deps`, `star`, `unstar`, `starred`, `comment`, `list-comments`, `backlinks`, `bulk-update`, `note`, `decide`, `export`, `import`, `history`, `remind`, `ack-reminder` |
| `pad_workspace` | `list`, `members`, `invite`, `storage`, `audit-log`, `create`, `claim`, `deleted`, `restore` |
| `pad_collection` | `list`, `create`, `update`, `delete` |
| `pad_project` | `dashboard`, `next`, `ready`, `stale`, `standup`, `changelog`, `report`, `activity` |
| `pad_role` | `list`, `create`, `update`, `delete` |
| `pad_search` | `query` |
| `pad_playbook` | `list`, `get`, `run` |
| `pad_library` | `list`, `get`, `activate` |
| `pad_attachment` | `list`, `show` |
| `pad_meta` | `server-info`, `version`, `tool-surface`, `bootstrap` |
| `pad_set_workspace` | session-default workspace pinning (response embeds the bootstrap blob) |

Plus resources at `pad://workspaces`, `pad://workspace/{ws}/dashboard`,
`pad://workspace/{ws}/items`, `pad://workspace/{ws}/items/{ref}`,
`pad://workspace/{ws}/collections`,
`pad://workspace/{ws}/attachments/{id}` (bounded image bytes),
`pad://workspace/{ws}/bootstrap`,
and `pad://_meta/version`.

**Stability contract** — two version constants, both advertised in the
initialize handshake under `capabilities.experimental.padCmdhelp` and
`capabilities.experimental.padToolSurface` (and queryable at
`pad://_meta/version`):

- `cmdhelp_version: "0.1"` — CLI help-tree contract (used at dispatch time)
- `tool_surface_version: "0.29"` — MCP tool catalog contract (v0.5 added `pad_library`; v0.6 `pad_item.backlinks`; v0.7 `pad_item` `export`/`import`; v0.8 `pad_workspace` `deleted`/`restore`; v0.9 made `pad_item.list` summary-shaped by default with a default+max result cap; v0.10 enforced the draft-playbook gate server-side on `pad_playbook.run` with an `allow_draft` escape hatch; v0.11 added the read-only `pad_attachment` tool (`list`/`show`); v0.12 added `pad_project.activity` (agent-accessible non-streaming activity feed); v0.13 added `pad_project` `ready`/`stale` (agent-oriented backlog + attention queries); v0.14 added `pad_item` `history` + optimistic concurrency (TASK-2022); v0.15 added the `pad_item.list` `unparented` parameter (TASK-2096); v0.16 made an empty-string `assigned_user_id` / `agent_role_id` CLEAR the assignment instead of being silently dropped, so an agent can finally unassign an item (TASK-2571); v0.17 carried that to the LOCAL STDIO transport by teaching the CLI to lift those keys onto their columns instead of into the fields blob (BUG-2583); v0.18 added `clear_assigned_user` / `clear_agent_role` booleans — the canonical, schema-discoverable way to unassign, backed by new `--clear-assigned-user` / `--clear-agent-role` flags on `pad item update` (IDEA-2584); v0.19 added a `clear_parent` boolean — the canonical, schema-discoverable way to detach an item from its parent, backed by a new `--clear-parent` flag on `pad item update` (BUG-2078); v0.20 gave every tool an explicit annotation block derived from the catalog’s read-only knowledge — fully-read-only tools advertise `readOnlyHint: true` / `destructiveHint: false`, all-additive-write tools (`pad_workspace`, `pad_library`) drop `destructiveHint`, overwrite/delete-capable tools stay conservatively destructive, `openWorldHint: false` everywhere — replacing mcp-go’s defaults that marked every tool destructive (BUG-2302), and made `pad_item.list` summary-shaped on the remote HTTP transport too, with a declared `full` boolean as the opt-in for complete bodies on both transports (BUG-2305); v0.21 bounded `pad_item.history`, which was unbounded on every surface — `limit` now covers it (default 50, max 300, the NEWEST N; no `offset`, because reverse-patch storage makes only a newest-end window cheap), applied in the catalog action so it lands on both transports, and summary mode now asks the server to skip patch resolution rather than resolving bodies the dispatcher discards (BUG-2608); v0.22 stopped `pad_item.move` destroying an item’s system metadata — implementation notes, decision log, linked PR and convention data now survive a move, any field the destination schema has no home for is REPORTED in the move’s activity entry rather than vanishing, and a `field` setter naming one of those reserved keys is refused with `malformed_override` instead of writing it (BUG-2674); v0.23 closed the same door on the ordinary update — a `field` setter naming `implementation_notes`, `decision_log` or `convention` is now refused on every transport at once (`validation_error` on HTTP, surfaced to MCP clients as `validation_failed`); the one gate covers the CLI, remote MCP and stdio MCP at once because all three lower a `field` setter into the same `fields_patch`; `github_pr` is deliberately exempt ON UPDATE (move and copy still refuse it), since `pad github link` cannot run on remote MCP and refusing it would leave those agents with no door at all (that door is itself broken — BUG-2696); item CREATE stays open, deliberately, because its full-`fields` payload is shared with Pad’s own writers. v0.23 also added the retry-hostile `stored_state_unreadable` error code so an agent told its target item’s stored data is unreadable stops instead of retrying a permanent failure (BUG-2627 / BUG-2675); v0.24 made the `pad_item` `fields` object a real write form on create/update — reads return `fields` as a native object, and writing that shape back was a silent no-op (accepted, never mapped, dropped while the PATCH still bumped `updated_at`) — merging it into the same path as `field`/the dedicated params with conflicting duplicate keys refused, and made input validation strict across all catalog tools: undeclared top-level keys now fail with a structured error instead of being silently dropped (#1066); v0.25 made `pad_library.activate` resolve its DESTINATION collection from the target’s declared artifact kind (SPEC-5 collection traits) rather than the literal `conventions` / `playbooks` slugs, so activating into a workspace that renamed either collection lands correctly instead of failing not-found with the collection sitting right there (BUG-2702); a lookup ERROR is now surfaced rather than silently falling back to the canonical slug, because falling back on an error means writing to a slug nothing was confirmed about (TASK-2657); v0.26 made `pad_workspace.create` REFUSE with a 403 when the calling OAuth connection's grant has `may_create_workspaces=false` — that checkbox previously gated only the post-creation auto-add, so a connection whose user declined it could still create workspaces — and on a connection with an explicit workspace allow-list, could not then see them (a wildcard `all_current_workspaces` connection could, which is why the consent mismatch rather than the invisibility is the defect); the same gate covers `POST /workspaces/import`, which mints a workspace through a second door. There is deliberately no escape-hatch parameter: the gate expresses the USER's consent decision, so only the user can lift it — by re-authorizing, or by enabling the flag on the existing connection at `/console/connected-apps` (IDEA-2756); v0.27 typed field values server-side so a declared number/json field is writable from the remote transport at all, carried the `fields` object with its JSON types intact, named undeclared keys back in `warnings.undeclared_fields` (accepted rather than refused — a census of 1012 items found 14 such keys across 168 live values, so refusing would have broken read-modify-write on items nobody had edited wrongly), and replaced the accreted per-site conflict guards with ONE check over a canonical view of every source; that check refuses several ambiguities v0.26 resolved silently, chiefly two names for one target in a single call (`parent`/`plan`, `assign`/`assigned_user_id`, `role`/`agent_role_id`), refused even when the values match because the names address one thing through incomparable vocabularies and the two doors resolved them differently (BUG-2850); v0.28 added two ADDITIVE `pad_item` actions — `remind`, which arms a one-shot reminder at an RFC3339 instant (`remind_at`), and `ack-reminder`, which acknowledges a fired one by id (`reminder_id`); a bare `YYYY-MM-DD` is refused rather than read as midnight, since a date names a 24-hour span and picking an hour inside it would be the server choosing a time nobody did (IDEA-2641); v0.29 made a `relation` field value have to NAME A LIVE ITEM in the collection that field declares — `internal/items` only ever checked the SHAPE of a relation ("must be a string"), because deciding whether a string names an item is a database question and that package is DB-free, so any string at all was accepted and stored and no client could render it honestly; every write door now refuses a value that names nothing, names an item in the WRONG collection, sits in a field whose schema declares no target collection, or is a SLUG (a deliberate divergence from `ResolveItem`: a slug is neither an ID nor stable, and free text like "red" resolving to whatever is slugged `red` today is exactly the corruption this closes). A CARRIED value — one already on the item, asserted by nobody — is never refused, because refusing would make every legacy item un-updatable, un-movable and un-copyable: within a workspace it resolves and survives, across a workspace boundary it is dropped without a lookup and reported in `warnings.dropped_fields`, so `pad_item.action=copy` now names a drop where v0.28 silently landed a dangling reference (PLAN-2857 / TASK-2878); see `internal/mcp/version.go` for the full changelog)

External agents pin against these so a future rename doesn't break them
silently. Errors come back as structured envelopes (`{error: {code,
message, hint, available_workspaces, ...}}`) with a closed code
taxonomy — 17 codes as of v0.23, enumerated in
`internal/mcp/errors.go`. Branch on `code`, not on message text; a code
you don't recognize is possible, and `stored_state_unreadable` in
particular means STOP rather than retry.

Full guide at [getpad.dev/mcp/local](https://getpad.dev/mcp/local) — install
paths, action enums per tool, error taxonomy, troubleshooting.

**On Pad Cloud?** Skip the install: add `https://mcp.getpad.dev` as a remote
MCP server in Claude Desktop, Claude.ai, Cursor, or Windsurf and sign in with
OAuth — same tool surface, no local binary. Setup guide at
[getpad.dev/mcp/remote](https://getpad.dev/mcp/remote).

## CLI Reference

```
pad auth configure                    Configure how this client connects to Pad
pad auth setup                        Initialize the first admin account
pad auth login                        Sign in
pad auth whoami                       Show current user

pad server start                      Start the Pad API server
pad server stop                       Stop the Pad server
pad server info                       Show client, connection, and local server status
pad server open                       Open web UI in browser

pad workspace init [name]             Initialize workspace in current directory
pad workspace link <workspace>        Link current directory to an existing workspace
pad workspace list                    List all workspaces
pad workspace switch <workspace>      Switch active workspace
pad workspace context                 Show structured workspace context
pad workspace context set --file X    Update structured workspace context from JSON
# Workspace onboarding: run `/pad onboard` from an agent session inside the workspace
pad workspace members                 List workspace members
pad workspace invite <email>          Invite a workspace member
pad workspace join <code>             Accept an invitation
pad workspace export                  Export workspace data
pad workspace import <file>           Import workspace data

pad project dashboard                 Project dashboard
pad project next                      Recommended next task
pad project ready                     Query actionable next items
pad project stale                     Query stalled or attention-worthy items
pad project standup [--days N]        Daily standup report
pad project changelog [--days N]      Release notes from completed items
pad project watch                     Real-time activity stream
pad project reconcile                 Reconcile item and PR state

pad item create <coll> "title"        Create item (task, idea, plan, doc, ...)
pad item list [collection]            List items (filters: --status, --priority, --all)
pad item show <ref>                   Show item detail
pad item open <ref>                   Open item in web UI
pad item update <ref>                 Update item fields
pad item delete <ref>                 Delete item
pad item move <ref> <collection>      Move item between collections
pad item edit <ref>                   Open item in $EDITOR
pad item search "query"               Full-text search across all items
pad item comment <ref> "text"         Add comment to an item
pad item comments <ref>               View item comments
pad item note <ref> "summary"         Append an implementation note to an item
pad item decide <ref> "decision"      Append a decision log entry to an item
pad item block <src> <target>         Create dependency
pad item blocked-by <item> <blk>      Mark item as blocked
pad item deps <ref>                   Show dependencies
pad item unblock <src> <target>       Remove dependency
pad item related <ref>                Show direct relationships for an item
pad item implemented-by <ref>         Show incoming implementers for an item
pad item bulk-update --status X       Batch update multiple items

pad collection list                   List collections with item counts
pad collection create <name>          Create a custom collection

pad library list                      Browse convention and playbook library
pad library activate <title>          Activate a convention or playbook

pad agent install [tool]              Install /pad skill for AI coding tools
pad agent status                      Show supported tools and installation status
pad agent update                      Update installed tool integrations

pad github link [item-ref]            Link current branch's PR to item
pad github status [item-ref]          Show PR status for linked items
pad github unlink <item-ref>          Remove PR link from item

pad webhook list             List workspace webhooks
pad webhook create <url>     Create webhook

pad session register         Record this session (harness pid + agent name) locally
pad session list             Registered sessions on this machine, with liveness
pad session prune            Remove records of sessions that are dead
```

All commands accept `--format json` for machine-readable output and `--workspace` to target a specific workspace.

### Shell completion

`pad` ships completion scripts for bash, zsh, fish, and PowerShell:

```bash
# Bash — current session only
source <(pad completion bash)
# Bash — persistent
pad completion bash > /etc/bash_completion.d/pad                   # Linux
pad completion bash > $(brew --prefix)/etc/bash_completion.d/pad   # macOS (Homebrew)

# Zsh (make sure compinit runs in your ~/.zshrc)
pad completion zsh > "${fpath[1]}/_pad"

# Fish
pad completion fish > ~/.config/fish/completions/pad.fish

# PowerShell (append the output to your $PROFILE)
pad completion powershell | Out-String | Invoke-Expression
```

Beyond command and flag names, completion is context-aware: collection arguments (e.g. `pad item list <TAB>`) complete against your workspace's collections, `--workspace` completes configured workspace names, and `--status` / `--priority` complete their valid values.

### Authentication

Pad runs without authentication by default for frictionless local use. For local installs, `pad init` creates the first admin account inline. The lower-level commands are useful when you're hosting a Pad server (Docker / remote) and need to set up auth on the server host directly:

```bash
pad auth setup         # Initialize the first admin account (server host, non-local mode)
pad auth login         # Sign in
pad auth whoami        # Show current user
pad auth logout        # Sign out
```

Once a user exists, all API requests and web UI access require authentication. Credentials are stored in `~/.pad/credentials.json`. Multiple users can be invited to workspaces with role-based access control (`owner`, `editor`, `viewer`).

#### Authenticating with an environment token

Set `PAD_TOKEN` to a Pad API token (minted under **Settings → API tokens** in the web UI) to authenticate without `pad auth login`:

```bash
PAD_TOKEN=pad_xxxxxxxx pad item list
```

`PAD_TOKEN` takes precedence over credentials saved by `pad auth login` — the same convention as `gh`'s `GH_TOKEN`. This is useful for CI, scripts, and machines where several AI agents share one CLI install but should act as different Pad users: give each agent its own token in its process environment, and the credential store is never touched. `pad auth whoami` reports the token's identity (with an `Auth: PAD_TOKEN environment override` line), and `pad auth login`/`logout` warn when the override is active — they manage the stored credentials, which the override bypasses. Deliberately, `pad auth logout` never invalidates the `PAD_TOKEN` session itself: it signs out the *stored* session only, and the env token's lifecycle belongs to wherever it was minted (revoke it under **Settings → API tokens**).

```bash
pad workspace members               # List workspace members
pad workspace invite user@example.com
pad workspace join <code>
```

## Architecture

```
┌──────────────────────────────────────────────┐
│              pad (single binary)              │
│                                               │
│  ┌──────────┐  ┌──────────┐  ┌────────────┐  │
│  │   CLI    │  │  REST    │  │  Embedded  │  │
│  │ (Cobra)  │  │  API     │  │  Web UI    │  │
│  └────┬─────┘  └────┬─────┘  │ (SvelteKit)│  │
│       │    HTTP      │        └────────────┘  │
│       └──────────────┤                        │
│                ┌─────▼─────┐                  │
│                │  SQLite   │                  │
│                │  + FTS5   │                  │
│                └───────────┘                  │
└───────────────────────────────────────────────┘
```

- **Go backend** — chi router, SQLite via [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) (pure Go, no CGO), FTS5 full-text search, SSE for real-time updates
- **SvelteKit frontend** — Svelte 5, Tiptap editor, drag-and-drop, adapter-static, embedded via `go:embed`
- **Single binary** — serves the API and web UI, runs on macOS, Linux, and Windows
- **Workspace-per-project** — each project gets its own workspace linked by a `.pad.toml` file

Self-hosted, all data lives in `~/.pad/pad.db`. Your data. Your machine. No telemetry, no accounts required — cloud only if you opt in.

## Community

- **[r/getpad](https://www.reddit.com/r/getpad/)** — how-tos, roadmap discussion, and notes from the agents that run Pad's own workspaces
- **[GitHub Issues](https://github.com/PerpetualSoftware/pad/issues)** — bugs and feature requests
- **[X](https://x.com/getpaddev)** / **[Bluesky](https://bsky.app/profile/getpaddev.bsky.social)** — release announcements

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the development guide.

```bash
make build      # Build web UI + Go binary
make test       # Run Go tests
make dev-web    # SvelteKit dev server with hot reload
make install    # Build, install to ~/.local/bin, restart server
```

## Security

See [SECURITY.md](SECURITY.md) for reporting vulnerabilities.

**Pushes into agent sessions are consent-gated.** `pad push` (and the web push composer) puts an item — and a message — in front of a running Claude Code session as direction from its own user. That is deliberate terminal instruction injection, so since v0.15.0 (PLAN-2613) receiving it is opt-in per session, not a side effect of installing the plugin:

- **No consent, no stream.** Nothing streams and nothing listens — watches and pushes alike — until the session consents (the plugin's always-on wrapper only registers presence and exits). `/pad:connect` arms the session locally and starts the monitor, which announces the armed state when its stream connects; `/pad:disconnect` withdraws; `/pad:status` reports the state. A repo can opt its sessions in at start with `push.auto_arm = true` in `.pad.toml` — an explicit file edit, never a machine-global default, and vetoable per user in `~/.pad/config.toml`.
- **Self-addressed only.** The server forces every push's target to the caller's own sessions; nobody can push into a session that isn't theirs. Delivery is filtered to armed sessions, and the surfaces are honest about it: the web composer shows the split ("2 connected, 0 accepting pushes") and withholds a send it knows nobody would accept; a CLI broadcast still publishes and reports `delivered_sessions` (in JSON output), and a targeted push to a session that is not accepting skips the publish rather than pretending.
- **No grandfathering.** Updating the plugin replaces the v0.14 always-on monitor with the gated one for everyone. Sessions that used to receive pushes receive none until they connect; the web composer's counts make that visible rather than silent.
- **The accepted caveat.** An agent can run the arm command from inside its own session. That is visible in the transcript, within the operator's sight: the gate protects sessions from the outside and does not police the inside. A push can inject text; it cannot click a permission prompt.

## License

[Apache License 2.0](LICENSE)
