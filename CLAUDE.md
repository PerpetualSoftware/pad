# Pad — Development Guide

## What This Is

Pad is a project management tool for developers and AI agents. Single Go binary with embedded SvelteKit web UI, SQLite storage, and multi-agent skill support (Claude Code, Cursor, Windsurf, Codex, Copilot, Amazon Q, Junie).

**Related repo:** The marketing website (getpad.dev) lives at `../pad-web` — a separate SvelteKit site deployed to Vercel.

## Architecture

- **Backend:** Go (cmd/pad/main.go) → REST API (internal/server/) → SQLite (internal/store/)
- **Frontend:** SvelteKit 2 + Svelte 5 (web/src/) → static build embedded in Go binary
- **Data model:** Workspaces → Collections (typed with JSON schemas) → Items (structured fields + rich content)
- **CLI:** Cobra commands in cmd/pad/main.go, HTTP client in internal/cli/
- **Agent skill:** Single natural-language `/pad` skill in skills/pad/SKILL.md

## Build & Install

```bash
make build      # Build web UI + Go binary (./pad)
make install    # Build, kill server, install to ~/.local/bin/pad, restart
make build-go   # Build Go only (skip web — faster when only backend changes)
make test       # Run Go tests
make web        # Build web UI only
make dev-web    # Run SvelteKit dev server (hot reload on :5173)
```

**After making changes, always run `make install`** to rebuild the binary, install it, and restart the server. The web UI at http://localhost:7777 will reflect the changes.

### Quick iteration loop

- **Backend only:** `make install` (skips web rebuild if no frontend changes — edit Makefile to use `build-go` instead of `build` in the install target)
- **Frontend only:** `make web && make install` or use `make dev-web` for hot reload during development
- **Full rebuild:** `make install`

## Key Directories

```
cmd/pad/main.go          — CLI entry point, all Cobra commands
internal/
  server/                — HTTP API handlers, SSE, middleware
  store/                 — SQLite CRUD, migrations, FTS
  models/                — Go types (Collection, Item, View, etc.)
  items/                 — Field validation against schemas
  collections/           — Default definitions, workspace templates
  cli/                   — HTTP client, formatting helpers
  events/                — EventBus for real-time SSE
  config/                — Workspace detection, .pad.toml
  diff/                  — Version diff storage
  webhooks/              — Webhook dispatcher with HMAC signing
  email/                 — Transactional email via Maileroo
  links/                 — Wiki-link parsing
web/src/
  routes/                — SvelteKit pages
  lib/api/client.ts      — TypeScript API client
  lib/types/index.ts     — TypeScript types
  lib/stores/            — Svelte 5 rune stores
  lib/components/        — Reusable UI components
skills/pad/SKILL.md      — Claude Code skill (embedded in binary)
```

## API

REST API at `/api/v1/`. Key endpoints:

- `GET/POST /workspaces/{ws}/collections` — collection CRUD
- `GET/POST /workspaces/{ws}/collections/{coll}/items` — item CRUD
- `GET/PATCH/DELETE /workspaces/{ws}/items/{slug}` — item by slug
- `GET /workspaces/{ws}/dashboard` — computed project overview (active items, plans, attention, blockers)
- `GET /workspaces/{ws}/activity` — workspace activity feed (enriched with item titles + change details)
- `GET/POST/DELETE /workspaces/{ws}/webhooks` — webhook management
- `GET /workspaces/{ws}/items/{slug}/children` — child items linked to a parent
- `GET /workspaces/{ws}/items/{slug}/progress` — child item completion progress
- `GET/POST /workspaces/{ws}/items/{slug}/links` — item relationships (blocks/blocked-by, parent/child)
- `GET /search?q=query&workspace=slug` — full-text search
- `GET /api/v1/events?workspace=slug` — SSE real-time events
- `GET /api/v1/collab/{itemID}?schema_version=N` — WebSocket upgrade for real-time collaborative editing (Yjs binary protocol; client must announce schema version)
- `GET /workspaces/{ws}/members` — list members + pending invitations
- `POST /workspaces/{ws}/members/invite` — invite user to workspace
- `GET /api/v1/auth/session` — auth status (`setup_required`, `setup_method`, `auth_method`, `authenticated`, `user`)
- `POST /api/v1/auth/bootstrap` — create the first admin account from localhost on a fresh instance
- `POST /api/v1/auth/register` — create account (admin-created or invitation-based after setup)
- `POST /api/v1/auth/login` — email/password login (returns session token)
- `POST /api/v1/auth/logout` — destroy session
- `GET/PATCH /api/v1/auth/me` — current user profile (GET) and update name/password (PATCH)
- `POST /api/v1/auth/forgot-password` — request password reset email
- `POST /api/v1/auth/reset-password` — reset password with token
- `GET/POST/DELETE /api/v1/auth/tokens` — user-scoped API tokens
- `GET/PATCH /api/v1/admin/settings` — platform settings (admin-only)
- `POST /api/v1/admin/test-email` — send test email (admin-only)
- `POST /api/v1/invitations/{code}/accept` — accept workspace invitation
- `GET /api/v1/workspaces/{ws}/agent/bootstrap` — one-round-trip agent context (workspace + user + collections + always-on conventions + roles + playbook metadata + dashboard + `needs_onboarding` flag). Same payload as the MCP `pad://workspace/{ws}/bootstrap` resource and the `pad_set_workspace` embed.

## Authentication

User-based authentication with email/password. When no users exist (fresh install), everything works without auth until the instance is initialized with `pad auth setup`. Once the first admin exists, all API requests require authentication.

```bash
# First-time setup
pad auth setup         # Create the first admin account on the server host

# Subsequent logins
pad auth login         # Email + password prompt
pad auth whoami        # Show current user
pad auth logout        # Sign out
pad auth reset-password user@example.com  # Generate reset link (admin fallback)

# Credentials stored in ~/.pad/credentials.json (0600 permissions)
# CLI auto-attaches auth token to all API requests
```

After any workspace is created (via `pad init` or `pad workspace init` — note that `pad auth setup` only creates the admin account, not a workspace), the success output points new users at the canonical onboarding entry point. Open a fresh agent session in the workspace's directory and say:

```
/pad onboard
```

Every new workspace ships with the `onboard` playbook auto-activated (PLAN-1496 / TASK-1499 / TASK-1500). The playbook walks the agent through an interview that adapts the workspace's collections, conventions, roles, and seeded playbooks to match the actual project. Works regardless of which template the user picked (or no template — see the `blank` template).

The pre-PLAN-1496 design seeded `IDEA-1` / `PLAN-2` / `TASK-3` / `DOC-4` (and `BACK-1` / `FEAT-1` siblings for scrum/product) as first-person-future-self notes; that pattern was retired in TASK-1501 / TASK-1502 in favor of the playbook-driven flow.

### Workspace membership
```bash
pad workspace members                         # List workspace members
pad workspace invite user@example.com         # Invite (adds directly if user exists, creates join code if not)
pad workspace invite user@example.com --role viewer  # Invite with specific role
pad workspace join <code>                     # Accept a workspace invitation
```

Roles: `owner` (full access), `editor` (CRUD items), `viewer` (read-only).

### Email (optional)

Transactional email via Maileroo. When configured, workspace invitations are sent by email. Without it, everything works via CLI-based join codes.

```bash
# Environment variables (or ~/.pad/config.toml)
PAD_MAILEROO_API_KEY=your-sending-key   # Required to enable email
PAD_EMAIL_FROM=noreply@yourdomain.com   # Sender address (default: noreply@getpad.dev)
PAD_EMAIL_FROM_NAME=Pad                 # Sender display name (default: Pad)
```

## CLI

Items are referenced by **issue ID** (e.g. `TASK-5`, `BUG-8`) wherever a `<ref>` argument appears.
Slugs also work but issue IDs are preferred.

```bash
pad item create <collection> "title" [--status X] [--priority X] [--parent REF]
pad item list [collection] [--status X] [--parent REF] [--all]
pad item show <ref>           # e.g. pad item show TASK-5
pad item update <ref> [--status X] [--priority X]
pad item delete <ref>
pad item move <ref> <target-collection>
pad item search "query"
pad project dashboard         # Project dashboard
pad project next              # Recommended next task
pad project standup [--days N]  # Daily standup report
pad project changelog [--days N] [--parent REF]  # Release notes from completed items
pad item block <source> <target>  # e.g. pad item block TASK-5 TASK-8
pad item blocked-by <item> <blocker>
pad item deps <ref>           # Show dependencies
pad item unblock <source> <target>
pad collection list           # List collections
pad collection create "Name" --fields "key:type[:opts]; ..."  # compact DSL for simple schemas
pad collection create "Name" --schema '<json>'                # full CollectionSchema (terminal_options, defaults, computed, relations)
pad item edit <ref>           # Open in $EDITOR
pad workspace init [--template X]  # Create workspace
pad agent install [tool]      # Install /pad skill for AI tools
# Workspace onboarding: run `/pad onboard` from an agent session inside the
# workspace (Claude Code, MCP, etc.). The /pad onboard playbook is
# auto-seeded into every new workspace.
pad server open               # Open web UI in browser
pad project watch             # Real-time activity stream
pad github link [item-ref]    # Link current branch's PR to item
pad github status [item-ref]  # Show PR status for linked items
pad github unlink <item-ref>  # Remove PR link from item
pad item bulk-update --status done TASK-5 TASK-8  # Batch operations
pad webhook list/create/delete/test               # Webhook management
pad auth setup                # Initialize a fresh instance with the first admin
pad auth login                # Log in
pad auth logout               # Sign out
pad auth whoami               # Show current user
pad workspace members         # List workspace members
pad workspace invite <email> [--role X] # Invite user to workspace
pad workspace join <code>     # Accept workspace invitation
```

Collection names accept singular forms: `task`→`tasks`, `idea`→`ideas`, `doc`→`docs`.

## MCP server

Pad runs as a local Model Context Protocol server so Claude Desktop / Cursor / Windsurf can call non-interactive `pad` commands as tools. As of PLAN-1410 the tool surface is a **hand-curated v0.4 catalog** in `internal/mcp/catalog_*.go` — one ToolDef per resource (`pad_item`, `pad_workspace`, `pad_collection`, `pad_project`, `pad_role`, `pad_search`, `pad_meta`, `pad_playbook`) with an `action` enum dispatching to underlying CLI commands. v0.2 introduced the catalog (PLAN-969 / TASK-981); v0.3 added `pad_playbook`, `pad_meta.action: bootstrap`, `pad_set_workspace`'s embedded-bootstrap response, and the `pad://workspace/{ws}/bootstrap` resource (PLAN-1377 / TASK-1380); v0.4 trimmed the bootstrap payload by ~40% (PLAN-1410) — slim `BootstrapCollection` + `BootstrapRole` projections (no UUIDs/timestamps/settings; nested `schema` object; redundant labels omitted), removed top-level `recent_activity` duplicate, dropped convention `slug`, and added a `BootstrapDashboard` wrapper that caps five sub-arrays (`attention`, `recent_activity`, `active_items`, `active_plans`, `by_role`) at 5 entries each with parallel `*_overflow_count` fields. The pre-catalog v0.1 cmdhelp leaf walker is retired.

cmdhelp is still consumed at dispatch time — `BuildCLIArgs` reads individual command schemas to translate the catalog's snake_case input map into CLI args. cmdhelp no longer drives tool naming or count.

**When adding a new `pad` command, decide whether it belongs on the MCP surface.** If yes, add an action to the appropriate `pad_<resource>` ToolDef in `internal/mcp/catalog_<resource>.go`. The action's handler — usually `passThrough([]string{"resource", "subcommand"})` — wires it through to dispatch. Don't expose interactive (prompts the user), destructive (mutates auth / filesystem state), long-running (streaming watcher), or recursive (would spawn another MCP server) commands.

```bash
pad mcp serve                 # JSON-RPC over stdio (called by clients)
pad mcp install <client>      # Write the client's mcp.json entry
pad mcp uninstall <client>    # Remove the entry
pad mcp status                # Install state across supported clients
```

Surface:
- **Tools:** the v0.4 catalog — eight resource × action tools (`pad_item`, `pad_workspace`, `pad_collection`, `pad_project`, `pad_role`, `pad_search`, `pad_meta`, `pad_playbook`) plus `pad_set_workspace` (takes a `workspace` slug only — no action enum). The eight resource × action tools take `action: <verb>` to choose what they do. `pad_playbook` is the playbook surface from PLAN-1377 — `list`/`get`/`run` mirror the CLI's `pad playbook` subcommands; `run` is side-effect-free and returns the body + bound args for the agent to execute. v0.4 (PLAN-1410) didn't change the tool/action surface; it trimmed the bootstrap JSON those tools/resources return — see the Stability contract subsection below for details.
- **Resources:** `pad://workspace/{ws}/items/{ref}`, `pad://workspace/{ws}/items`, `pad://workspace/{ws}/dashboard`, `pad://workspace/{ws}/collections`, `pad://workspace/{ws}/bootstrap` (one-shot workspace overview — user + collections + always-on conventions + roles + playbook metadata + dashboard + recent activity), plus the server-wide `pad://_meta/version`.
- **Prompts:** `pad_plan`, `pad_ideate`, `pad_retro`, `pad_onboard` — multi-step workflows lifted from `skills/pad/SKILL.md`.

**`pad_set_workspace`** pins the session default workspace; its response embeds the bootstrap blob so agents pin + load workspace context in one round-trip. The same payload is available on demand via `pad_meta.action: bootstrap` and the `pad://workspace/{ws}/bootstrap` resource.

**Stability contract.** Two version constants live in `internal/mcp/version.go`, advertised in the handshake under `capabilities.experimental.padCmdhelp` and `capabilities.experimental.padToolSurface`:
- `CmdhelpVersion` (currently `"0.1"`) — the cmdhelp CLI help-tree contract. Bump when CLI flag/arg schemas change incompatibly.
- `ToolSurfaceVersion` (currently `"0.7"`) — the MCP tool catalog contract. Bump when tool names, action enums, or parameter shapes change incompatibly. **v0.7** adds `export` + `import` actions to `pad_item`, mirroring the CLI `pad item export` / `pad item import` (covers playbooks AND conventions). `export` (read-only) takes `ref` and returns the portable artifact text — it forces the CLI's stdout sink (`-o -`) so the bytes come back as the result instead of a file. `import` (mutating, not destructive) takes a new `artifact` param (the full artifact text) and returns `{ref, slug, warnings}`; the ExecDispatcher can't pipe stdin, so it spills the artifact to a temp file and dispatches `item import <tmpfile>`. v0.6 added the `pad_item.backlinks` action; v0.5 added `pad_library`. v0.3 (PLAN-1377 / TASK-1380) introduced `pad_meta.action: bootstrap`, `pad_set_workspace`'s embedded-bootstrap response, and the `pad://workspace/{ws}/bootstrap` resource. **v0.4 (PLAN-1410)** is a comprehensive bootstrap-payload trim — same tool catalog, slimmer JSON shape inside bootstrap responses: `BootstrapCollection` projection drops `id`/`workspace_id`/timestamps/`settings` and emits `schema` as a nested object; `BootstrapRole` projection drops UUIDs/timestamps/`tools`; convention `slug` dropped; top-level `recent_activity` (a duplicate of `dashboard.recent_activity`) removed; new `BootstrapDashboard` wrapper caps five sub-arrays (`attention`, `recent_activity`, `active_items`, `active_plans`, `by_role`) at 5 entries each with parallel `*_overflow_count` fields; redundant schema labels omitted when `label == TitleCase(key)`. Cumulative size reduction: ~40% on a representative workspace, ~54% on the fixture (see PLAN-1410's Result section for per-section deltas). Compatibility: most changes are subtractive (dropped fields) or additive (overflow counts), but **one type change is breaking**: `collections[].schema` went from a JSON-encoded string to a nested JSON object — clients that JSON.parse()'d the string need to consume it directly as an object now. The dropped fields (UUIDs, timestamps, settings, duplicate `recent_activity`, convention `slug`) have canonical alternatives (slugs for addressing; `pad collection list` / `pad role list` for the full models when needed).

Both are also returned by `pad://_meta/version` and `pad_meta.action: version`.

**Dispatchers.** Two ship in `internal/mcp/`:

- `ExecDispatcher` — shells out to the `pad` binary; subprocess inherits credentials from `~/.pad/credentials.json`. Used by `pad mcp serve` for local stdio MCP.
- `HTTPHandlerDispatcher` — calls pad-cloud's HTTP handlers in-process with the requesting user attached via `server.WithCurrentUser`. Used by the future `/mcp` endpoint (PLAN-943) where the dispatcher serves multiple OAuth users from a single process. Tools are wired into the route table at `internal/mcp/dispatch_http.go` (`routeTable`); add a `RouteMapper` per command — `mapItemCreate` is the seed entry from TASK-965.

Code lives in `internal/mcp/` (built on `github.com/mark3labs/mcp-go`). Public docs at `getpad.dev/mcp/local`.

## Data Model

- **Collections** have JSON schemas defining typed fields (select, text, date, number, etc.)
- **Items** have structured `fields` JSON + optional rich `content` (markdown)
- **Parent/child links:** Any item can be a parent of child items (`--parent REF`). Children get progress tracking, burndown charts, and nested rendering. Plans are the most common parent, but Ideas, Docs, or Tasks can also have children.
- **Wiki-links** `[[Title]]` resolve across all items, rendered as clickable links
- **Default collections:** Tasks, Ideas, Plans, Docs (software / `startup` template)
- **Templates** are grouped by category so Pad supports more than just software workflows:
  - **Software:** `startup` (default), `scrum`, `product`
  - **People:** `hiring` (company-side: Requisitions → Candidates → Loops → Feedback), `interviewing` (candidate-side: Applications, Interviews, Companies, Contacts)
  - **Custom:** `blank` — system collections only (Conventions, Playbooks), no user-facing seeds. Designed as the entry point for the `/pad onboard` agent-driven flow (see [Onboarding](#onboarding) below). PLAN-1496 / TASK-1498.
  - *Research / Content / Operations / Personal are reserved categories awaiting their first templates.*
- Each non-blank template ships a curated starter pack (conventions + playbooks) appropriate to its domain — trigger vocabularies vary (`on-commit` vs `on-candidate-advance` vs `on-interview-scheduled`).
- **The IDEA-1 / BACK-1 / FEAT-1 first-person seed-item pattern was retired in PLAN-1496** (TASK-1501 / TASK-1502). Templates no longer seed sample items; the `/pad onboard` playbook (auto-seeded into every workspace, TASK-1500) drives setup conversationally instead.
- Set the template via `pad workspace init --template <name>`. Running `pad init` with no flag in a TTY opens an interactive picker grouped by category. Run `pad workspace init --list-templates` to see the current catalog.
- See `PLAN-609` and `IDEA-583` for original design history; `PLAN-1496` for the onboarding refactor.

## Playbooks

Playbooks are first-class invokable procedures. They live in the `playbooks` collection (typed item, just like Tasks/Ideas/Plans) but carry two extra fields that make them user-callable:

- **`invocation_slug`** — optional, workspace-unique, kebab-case (regex `^[a-z0-9][a-z0-9-]*[a-z0-9]$`, 2+ chars). When set, the playbook is invokable by intent (NL is canonical) and via the per-surface slug shortcut — `/pad <slug>` in Claude Code, `$pad <slug>` in Codex, `pad_playbook action=run ref=<slug>` via MCP ("slug routing"). Leave blank for trigger-only playbooks (e.g. `trigger=on-release` that auto-load on intent match).
- **`arguments`** — JSON array of `{name, type, required, default, description, enum}` entries. Types: `ref`, `string`, `flag`, `enum`, `number`. Mirrors the playbook body's `## Arguments` section; the structured field is the queryable form (used by `pad playbook run`'s strict parser) and the markdown is the human-readable mirror.

**Invocation model.** Three surfaces, one playbook:

- **Claude Code (agent NL):** `/pad ship PLAN-1377 stop-after-each` — the `/pad` skill matches the first token against the bootstrap's playbook slug list and binds the rest with flexible NL parsing.
- **CLI (strict positional):** `pad playbook run ship TASK-10,TASK-11 merge-strategy=rebase` — the server applies strict positional + bareword-flag + `key=value` parsing.
- **MCP:** `pad_playbook` tool with `action: list | get | run`. `run` accepts either a pre-parsed `args` map or raw CLI tokens via `raw_args`.

**Bootstrap returns metadata at startup.** `pad bootstrap` (CLI + `GET /api/v1/workspaces/{ws}/agent/bootstrap` + `pad://workspace/{ws}/bootstrap` resource + `pad_set_workspace` response embed) returns the workspace's playbook metadata in one round-trip — `ref`, `title`, `slug`, `invocation_slug`, `trigger`, `scope`, `status`, `has_arguments`, `summary` per entry. **No bodies** in the bootstrap blob; the agent loads the full body via `pad playbook show <slug>` only when invoking. Keeps context light while still letting the agent route `/pad ship` without a tool call.

**Seeded `ship` playbook.** The `startup` template ships a generic `ship` playbook (`invocation_slug=ship`) derived from the personal `/ship-tasks` slash command. Fresh `pad workspace init --template startup` workspaces get it as PLAYB-N out of the box. See `internal/collections/templates_startup_ship.go` for the body + de-personalization choices.

**Library — discovery surface for invokable playbooks.** Per PLAN-1397's invokable-first overhaul, the playbook library (web UI: `/[username]/[workspace]/library?tab=playbooks`; JSON: `GET /api/v1/playbook-library`) carries the three canonical invokable workflow playbooks — **ship**, **plan**, **decompose** (invokable by intent; `/pad <slug>` · `$pad <slug>` · the `pad_playbook` MCP form are per-surface shortcuts) — under a single `agent-workflows` category. Each library card surfaces a `▶ <slug>` invoke chip (with an NL-canonical tooltip listing the per-surface shortcuts) and an `N args` badge so the invocation model is visible before activation. Software templates auto-seed `plan` + `decompose` via `softwareStarterPlaybookTitles`; `startup` separately prepends `ship` so all three land together at workspace init. The pre-PLAN-1377 trigger-only checklist entries (Implementation Workflow, Code Review Process, Plan Creation, Bug Triage, Retrospective, Onboarding to a Project, Release Process, Deployment, Incident Response) are stashed in `playbook_library_archive.go::archivedPlaybooks()` — compiled but not surfaced; per-entry "convert / promote to convention / retire" decisions tracked in IDEA-1396.

**Web UI editor.** `web/src/routes/[username]/[workspace]/playbooks/[slug]/+page.svelte` is the dedicated playbook editor — kebab-case slug input with debounced uniqueness check, structured arguments builder that round-trips with the body's `## Arguments` section, trigger selector with custom-trigger escape, and a "Test invocation" helper that renders `/pad`, `pad playbook run`, and `pad_playbook` MCP JSON forms from a slug + sample inputs. The reusable component lives at `web/src/lib/components/playbooks/PlaybookFormFields.svelte` and the shared parser/generator at `web/src/lib/playbooks/arguments.ts`.

**Code map:**

- `internal/server/handlers_playbooks.go` — `pad playbook list|show|run` HTTP handlers; `ParsePlaybookCLIArgs`, `resolvePlaybook`.
- `internal/server/handlers_bootstrap.go` — `pad bootstrap`; embeds playbook metadata.
- `internal/mcp/catalog_playbook.go` — `pad_playbook` MCP tool catalog entry.
- `internal/collections/templates.go` — playbooks collection schema (`invocation_slug` + `arguments` fields); `softwareStarterPlaybookTitles` (auto-seed lineup for software templates).
- `internal/collections/templates_startup_ship.go` — the seeded `ship` playbook (`ShipPlaybook()`, `shipPlaybookBody`, `shipPlaybookArguments`).
- `internal/collections/playbook_library.go` — the invokable-first library (`PlaybookLibrary()`, `LibraryPlaybook` struct with `InvocationSlug` + `Arguments`).
- `internal/collections/playbook_library_plan.go` — the `plan` library entry (`PlanPlaybook()`).
- `internal/collections/playbook_library_decompose.go` — the `decompose` library entry (`DecomposePlaybook()`).
- `internal/collections/playbook_library_archive.go` — retired pre-PLAN-1377 bodies; not surfaced, but compiled for future migrations (IDEA-1396).
- `web/src/lib/playbooks/arguments.ts` — `## Arguments` parser/generator, `INVOCATION_SLUG_PATTERN`, `buildTestInvocation`.

See `PLAN-1377` (invocation model) and `PLAN-1397` (library overhaul) in this workspace for the design history.

## Onboarding

Workspace setup is driven by the canonical **onboard** invokable library playbook (PLAN-1496 / TASK-1499) — invoked by intent ("set up my workspace") or the per-surface shortcut (`/pad onboard` in Claude Code, `$pad onboard` in Codex, the `pad_onboard` MCP prompt). Pad does not run a baked-in CLI onboarding wizard; the playbook body IS the onboarding script, and any agent that can dispatch a playbook (Claude Code, MCP client, CLI) can run it.

**Auto-seeded everywhere.** `pad workspace init` (with any non-blank `--template`) seeds the onboard playbook into the new workspace as `status=active, invocation_slug=onboard` (TASK-1500). The `blank` template ships it as the workspace's ONLY user-facing content. Empty-template-name workspace creation (`SeedCollectionsFromTemplate(ws, "")` — used by tests and direct API callers) intentionally skips the seed; see `internal/store/collections.go::SeedCollectionsFromTemplate` for the gating logic.

**Surface-agnostic body.** The playbook body (`internal/collections/playbook_library_onboard.go::onboardPlaybookBody`) describes intent, not specific CLI commands. It instructs the agent to use whatever surface it has — `pad_item` MCP, `pad item` CLI, `pad_collection` MCP, etc. — and works for pure-MCP agents (no shell) the same as for Claude Code. The body covers four modes: `build` (blank workspace, build from scratch), `audit` (templated workspace, adapt seeded items), `revisit` (already-onboarded, change something specific), and `defaults` (escape hatch — pick sensible defaults and report).

**Adaptation posture, not curation.** The body explicitly tells the agent: library entries are STARTING POINTS, not finished artifacts. Read the rule, rewrite using the project's actual commands and vocabulary. Invent when the library has nothing close. If the template seeded something that doesn't fit, edit or delete it. This is the core posture PLAN-1496 codifies — software templates seed generic "run the test suite" conventions, and `/pad onboard` rewrites them to `make test` / `go test ./...` / whatever the project actually uses.

**Mutation primitives.** The adaptation posture depends on agent-facing mutation tools, exposed by TASK-1510 / TASK-1511 / TASK-1512:

- `pad collection update <slug>` + `pad_collection.action: update` — rename collections, swap icons, reshape schemas (TASK-1510)
- `pad collection delete <slug>` + `pad_collection.action: delete` — remove user-created collections that don't fit (TASK-1511)
- `pad role update <slug>` + `pad_role.action: update` — rewrite role descriptions and icons (TASK-1512)

Server handlers existed pre-PLAN-1496; these tasks just wired CLI subcommands and MCP catalog actions to the existing HTTP endpoints. All three are owner-only server-side.

**`needs_onboarding` bootstrap flag.** `AgentBootstrap.NeedsOnboarding` (PLAN-1496 / TASK-1504) is true when the workspace has zero items with `source != 'template'` — i.e. nothing beyond what the template seeded. The agent skill (`skills/pad/SKILL.md`) and the MCP server instructions render an active, NL-canonical offer when true (PLAN-1847): *"This workspace is brand new and isn't set up yet. Want me to set it up?"* — an offer, not an auto-run. The flag flips to false the moment any user/agent-created item exists; the offer stops firing past that point. Computed per-request via `Store.WorkspaceHasUserCreatedItems(workspaceID)` (EXISTS-backed). PLAN-1496 / TASK-1505 also retired the standalone "Onboarding" workflow section from the skill — the playbook body owns that script now.

**Retired surfaces.** The pre-PLAN-1496 design had several surfaces that the playbook replaces; all retired:

- `pad onboard` Cobra subcommand (was: codebase scan + convention suggestions) — TASK-1502.
- `OnboardingPrimaryRef` field on `WorkspaceTemplate` (was: named IDEA-1 / BACK-1 / FEAT-1 per template) — TASK-1502. Dashboard banner auto-discovers seeds via `item_number=1 + source='template'` if a future template ever wants to reintroduce them.
- The `*OnboardingItems()` generators in `internal/collections/templates_onboarding*.go` (deleted files) — TASK-1501.
- The skill's standalone "Onboarding" workflow section — TASK-1505. Replaced by a one-paragraph pointer at the playbook.

**Code map:**

- `internal/collections/playbook_library_onboard.go` — the canonical playbook body + `OnboardPlaybook()` library entry + `OnboardSeedPlaybook()` auto-seed.
- `internal/collections/templates_blank.go` — minimal trigger/scope vocabularies for the blank template's seeded system collections.
- `internal/store/collections.go::SeedCollectionsFromTemplate` — wires the auto-seed for every non-empty templateName.
- `internal/store/items.go::WorkspaceHasUserCreatedItems` — the `needs_onboarding` query predicate.
- `internal/server/handlers_bootstrap.go::AgentBootstrap.NeedsOnboarding` — the bootstrap field.
- `skills/pad/SKILL.md` — the nudge-rendering rule in Context Loading; the routing entry under "set up my workspace".

## Testing

```bash
go test ./...              # All Go tests
go test ./internal/store/  # Store tests only
cd web && npm run build    # Verify frontend compiles
```

## Common Tasks

### Add a new API endpoint
1. Add handler in `internal/server/handlers_*.go`
2. Register route in `internal/server/server.go` setupRouter()
3. Add store method in `internal/store/` if needed
4. Add CLI client method in `internal/cli/client.go`
5. Add TypeScript type in `web/src/lib/types/index.ts`
6. Add API method in `web/src/lib/api/client.ts`
7. `make install`

### Add a new CLI command
1. Add function in `cmd/pad/main.go`
2. Register in rootCmd.AddCommand()
3. `make install`

### Modify the database schema
1. Add migration file in `internal/store/migrations/`
2. Update models in `internal/models/`
3. Update store methods in `internal/store/`
4. `make install` (migrations run automatically on server start)

## Real-time collaboration (Yjs / Tiptap)

Collab is wired through `/api/v1/collab/{itemID}` (WebSocket, Yjs
binary protocol). The relevant code lives in:

- `internal/collab/` — RoomManager, room lifecycle, dumb-relay
- `internal/store/yjs_updates.go` — op-log persistence
- `web/src/lib/collab/wsProvider.svelte.ts` — client provider
- `web/src/lib/collab/schemaVersion.ts` — client schema-version stamp

**Collab requires no additional container deps; the single Go binary
remains the self-hosted shape.** The dumb-relay design (server
persists raw Yjs binary updates without parsing them) means there's
no Yjs Go port to vendor and no separate sync-server process to run.
The op-log lives in the same SQLite/Postgres as everything else, and
the WebSocket relay is part of the main HTTP listener. Multi-instance
Redis fanout is deliberately out of scope for v1 (single-instance
everywhere); when horizontal scaling is needed it lands as a separate
IDEA, not a self-host complication.

### Tiptap multi-package coordinated bumps

The Y.Doc/ProseMirror schema is shared across three Tiptap packages:

- `@tiptap/core`
- `@tiptap/extension-collaboration`
- `@tiptap/y-tiptap`

**Rule: bump all three together, exact-pinned to the same version.**
Mixing minor versions across these can change the persisted Y.Doc
shape silently — peers running mismatched bundles produce divergent
ops that the relay can't reconcile. The `web/package.json` pins
each one explicitly (e.g. `"@tiptap/extension-collaboration": "3.22.5"`)
rather than using `^` ranges so npm can't slide one out of sync.

A coordinated bump that changes the ProseMirror node-spec MUST also
bump `web/src/lib/collab/schemaVersion.ts::SCHEMA_VERSION` AND
`internal/collab/manager.go::DefaultSchemaVersion` in lockstep. The
client announces the version on every WS connect; mismatch returns
HTTP 400 and the room manager prunes the per-item op-log so the new
client doesn't replay incompatible old-schema ops. items.content is
canonical and untouched, so no edit history is lost.

Pure UI/CSS/behavioural changes that don't alter the persisted
document shape DO NOT bump the schema version. When in doubt, load
an item edited under the old version after your change and confirm
the rendered tree is identical.
