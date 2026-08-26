# Pad — Development Guide

## What This Is

Pad is a project management tool for developers and AI agents. Single Go binary with embedded SvelteKit web UI, SQLite storage, and multi-agent skill support (Claude Code, Cursor, Windsurf, Codex, OpenCode, Copilot, Amazon Q, Junie).

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

### Working in a git worktree

Agent sessions take a `git worktree` per task rather than sharing the main checkout (a shared checkout means a shared stash stack, branch state, and dirty files across sessions). Three rules keep web tooling working there:

- **Symlinking `web/node_modules` to the main checkout's copy is fine** (and fast). Vitest, `vite build`, and `npm run check` all work through the symlink.
- **A fresh worktree has no `web/.svelte-kit`** (gitignored, generated). Run `npx svelte-kit sync` in `web/` before any vitest/vite command — or `npm run check`, which syncs first. Without it, vitest fails with `Failed to load tsconfig '.svelte-kit/tsconfig.json': Tsconfig not found` regardless of how `node_modules` was set up. (This missing generated dir was historically misdiagnosed as a symlink problem — `npm ci` "fixed" it only because its `prepare` script runs `svelte-kit sync`.)
- **Never run `npm ci` in a worktree whose `web/node_modules` is a symlink — including via make.** `npm ci` lives in the `web` target, so every target whose dependency chain reaches it is off-limits too: currently `web`, `build`, `install`, `serve`, `web-check`, and `check` (via `web-check`). Everything else — `build-go`, `dev`, `restart`, `test`, `test-pg`, `lint`, `vuln`, `web-test`, `dev-web`, `clean` — never reaches `npm ci`. `npm ci` deletes through the symlink into the shared tree, breaking every other worktree and session at once with a confusing `vitest: not found`. If you want a real, isolated `node_modules` instead of a symlink, `npm ci` in an un-symlinked `web/` is ~5s on a warm cache and regenerates `.svelte-kit` as a side effect.

`web/vitest.config.ts`'s `server.fs.allow` note covers the other worktree wrinkle (symlink realpaths vs the dev-server file-serving guard) and points back at this section.

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
- `POST /workspaces/{ws}/items/{slug}/copy/preflight` — cross-workspace copy dry run: what would carry / drop / need a value, plus the full warning set. Read-only and safe to call repeatedly (PLAN-2357)
- `POST /workspaces/{ws}/items/{slug}/copy` — cross-workspace copy; with `archive_source` it is the move. Same request shape as the preflight. **Never retry it automatically** — there is no idempotency key, so a retry duplicates the item
- `GET /workspaces/{ws}/dashboard` — computed project overview (active items, plans, attention, blockers)
- `GET /workspaces/{ws}/activity` — workspace activity feed (enriched with item titles + change details)
- `GET/POST/DELETE /workspaces/{ws}/webhooks` — webhook management
- `GET /workspaces/{ws}/items/{slug}/children` — child items linked to a parent
- `GET /workspaces/{ws}/items/{slug}/progress` — child item completion progress
- `GET/POST /workspaces/{ws}/items/{slug}/links` — item relationships (blocks/blocked-by, parent/child)
- `GET /search?q=query&workspace=slug` — full-text search
- `GET /api/v1/events?workspace=slug` — SSE real-time events (workspace-scoped)
- `GET /api/v1/events/stream` — SSE watch/push notifications (USER-scoped, spans every workspace the caller belongs to; backs `pad watch --stream`)

Both SSE endpoints share one admission budget, enforced **per instance**: `PAD_SSE_MAX_CONNECTIONS` (default 1000) and `PAD_SSE_MAX_PER_USER` (default 50) cover both; `PAD_SSE_MAX_PER_WORKSPACE` (default 100) covers `/api/v1/events` only. Over the limit is `429` with code `sse_limit_exceeded` and a `Retry-After` header — clients must back off, not retry immediately (BUG-2726). The CLI does; browsers cannot, since `EventSource` exposes neither the status nor the header to the page (BUG-2733). On `/api/v1/events` only, callers with no resolved user (a legacy workspace token, or the fresh-install window before the first admin exists) are bounded per *workspace* instead of per user; `/api/v1/events/stream` has no such case — it requires a resolved user and answers `401` otherwise.
- `GET /api/v1/collab/{itemID}?schema_version=N` — WebSocket upgrade for real-time collaborative editing (Yjs binary protocol; client must announce schema version)
- `GET /workspaces/{ws}/members` — list members + pending invitations
- `POST /workspaces/{ws}/members/invite` — invite user to workspace
- `GET /api/v1/auth/session` — auth status (`setup_required`, `setup_method`, `auth_method`, `authenticated`, `email_configured`, `user`)
- `POST /api/v1/auth/bootstrap` — create the first admin account from localhost on a fresh instance
- `POST /api/v1/auth/register` — create account (admin-created or invitation-based after setup)
- `POST /api/v1/auth/login` — email/password login (returns session token)
- `POST /api/v1/auth/logout` — destroy session
- `GET/PATCH /api/v1/auth/me` — current user profile (GET) and update name/password (PATCH)
- `POST /api/v1/auth/forgot-password` — request password reset email
- `POST /api/v1/auth/reset-password` — reset password with token
- `POST /api/v1/auth/local-reset` — localhost-only account recovery (self-host, non-cloud). Loopback-gated, no auth — the bootstrap trust model. Returns a single-use reset link, or a temp password with `{"temp_password": true}`. Backs `pad auth reset-password`.
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
pad auth reset-password user@example.com  # Recover a locked-out account (run ON THE SERVER HOST)
pad auth reset-password user@example.com --temp-password  # ...set a temp password instead of a reset link

# Credentials stored in ~/.pad/credentials.json (0600 permissions)
# CLI auto-attaches auth token to all API requests
```

### Locked-out account recovery (self-host, no email)

When a self-hosted instance has no email provider, a forgotten password can't be reset by email. Two host-side recovery paths (both require shell access to the server — the same trust boundary as `pad auth setup`):

- **`pad auth reset-password <email>`** — run it **on the server host**. It calls the loopback-only `/api/v1/auth/local-reset` endpoint (no login required — that's the point) and prints a single-use reset link. Add `--temp-password` to instead set a random temporary password printed to the terminal (headless boxes with no browser). The endpoint refuses proxied/remote requests and is disabled in cloud mode.
- **Server log** — if a user submits the web `/forgot-password` form on a non-cloud instance with no email, the server logs the reset path (`slog.Info ... reset_path=/reset-password/<token>`). Paste it after the instance's base URL to finish the reset by hand.

The web `/forgot-password` page detects `email_configured == false` (from the session payload) and shows the `pad auth reset-password` recovery instructions instead of a dead "we emailed you a link" message.

Code: `internal/server/handlers_auth.go::handleLocalReset` (loopback + non-cloud gates), `cmd/pad/main.go::resetPasswordCmd`, `web/src/routes/forgot-password/+page.svelte`.

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
                              # Collection change WITHIN a workspace (cross-workspace is `item copy`).
                              # Field values the target schema has no home for are dropped — and since
                              # BUG-2674 the move REPORTS them, in its activity entry's `dropped_fields`
                              # and in the item timeline. System metadata (implementation_notes,
                              # decision_log, github_pr, convention) always survives a move; it used to
                              # be destroyed silently.
pad item copy <ref> --to-workspace <slug> --collection <slug> [--dry-run] [--archive-source] [--field k=v]
                              # Cross-workspace copy; --archive-source makes it a move.
                              # --dry-run previews the field mapping + warnings.
                              # Refuses rather than guessing when a destination field needs a value,
                              # and NEVER retries the mutating call (no idempotency key — PLAN-2357 DR-13).
                              # Content semantics: markdown is copied verbatim except `pad-attachment:`
                              # refs that resolve to a LIVE attachment in the SOURCE workspace — those are
                              # repointed at the clones (+ variants). Foreign / soft-deleted / dangling ids
                              # are left literal and counted as unresolvable, never cloned.
                              # `[[wiki-links]]` are NOT rewritten — they re-resolve in the DESTINATION,
                              # so a link can silently retarget to a different item or break;
                              # `[[workspace::REF]]` stays a genuine cross-workspace reference.
                              # The web dialog (item pane ⋯ → "Copy or move to workspace…") says the same.
                              # System metadata (BUG-2674): implementation_notes and decision_log CARRY —
                              # they describe the item's own history and are true wherever it lands.
                              # github_pr does NOT carry across workspaces: it names the SOURCE project's
                              # repo, so on the copy it would render a live PR link about a project the
                              # destination may have nothing to do with. It is reported in the dropped
                              # bucket as `referent_not_portable`, and DOES carry on a same-workspace
                              # move/copy, where the repo context is unchanged. None of these four keys
                              # (+ `convention`) is settable via `--field` on copy or move — they are
                              # written by `pad item note` / `pad item decide` / `pad github link`.
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
pad session register [--agent NAME]   # Record this session (harness pid + agent name) in ~/.pad/sessions; the plugin monitor runs it on start
pad session list [--agent X] [--cwd D] [--all]  # Registered sessions on this machine with a liveness verdict each (alive/dead/unknown); --format json is the stable shape
pad session prune [--older-than DUR]  # Remove dead sessions' records; unknown-liveness ones only under an explicit age bound
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

Pad runs as a local Model Context Protocol server so Claude Desktop / Cursor / Windsurf can call non-interactive `pad` commands as tools. The tool surface is a **hand-curated catalog** (currently v0.26) in `internal/mcp/catalog_*.go` — one ToolDef per resource (`pad_item`, `pad_workspace`, `pad_collection`, `pad_project`, `pad_role`, `pad_search`, `pad_meta`, `pad_playbook`, `pad_library`, `pad_attachment`) with an `action` enum dispatching to underlying CLI commands. v0.26 (IDEA-2756) makes `pad_workspace.create` REFUSE with a 403 when the calling OAuth connection's grant carries `may_create_workspaces=false` — that consent checkbox previously gated only the post-creation auto-add, so a connection whose user declined it created workspaces it could then not see. The same gate covers `POST /workspaces/import` (a second door onto `store.ImportWorkspace` → `CreateWorkspace`, with no MCP action today). No escape-hatch param, deliberately: the gate expresses the USER's consent decision, so only the user can lift it — by re-authorizing, or by enabling the flag on the existing connection at `/console/connected-apps`. v0.25 (TASK-2657 / BUG-2702) makes `pad_library.activate` resolve its destination collection from the target's declared artifact kind rather than the literal `conventions` / `playbooks` slugs, and surfaces a lookup ERROR instead of falling back. v0.24 (#1066) makes the `pad_item` `fields` OBJECT a real write form on create/update — reads return `fields` as a native object (BUG-991 normalization), and writing that shape back was a silent no-op: not a declared param, no `additionalProperties`, so it was accepted, never mapped by `BuildCLIArgs`, and dropped while the PATCH still bumped `updated_at`. The alias merges into the same path as `field: ["key=value"]` / the dedicated params (`catalog_item_fields.go`), refusing the same key in two places with conflicting values; and input validation is now STRICT across all catalog tools — an undeclared top-level key fails with a structured `validation_failed` naming it, instead of being silently dropped (a small documented compat list survives: pad_item's v0.16 `assigned_user_id` / `agent_role_id` remote clear form). One bump covers both halves — they are one contract change. v0.23 (BUG-2627 part 2 + BUG-2675, PR #1166) refuses raw `field` setters naming system-metadata keys in `fields_patch` on every transport (`github_pr` exempt on UPDATE only — the sole remote writer, itself broken: BUG-2696) and adds the retry-hostile `stored_state_unreadable` error code. v0.22 (BUG-2674, PR #1165) makes reserved metadata survive a move and refuses `field` setters naming those keys on move/copy — see `internal/mcp/version.go` for both full entries. v0.21 (BUG-2608) bounds `pad_item.action=history`, which was unbounded on every surface: the `limit` param now covers it (default 50, max 300 — the NEWEST N versions, with no `offset`, because reverse-patch storage makes only a newest-end window cheap to reconstruct), applied in the CATALOG action so it lands on both transports, and summary mode now asks the server to skip patch resolution (`?summary=true`) instead of resolving every body and discarding it. Additive param bump — `limit` already existed and nothing changed shape. v0.20 (BUG-2302 + BUG-2305, one bump) adds explicit MCP tool annotations (`readOnlyHint`/`destructiveHint`/`idempotentHint` derived from the catalog's own write-shape knowledge, fixing read-only tools that advertised `destructiveHint:true`) and makes `pad_item.list` summary-shaped on the REMOTE /mcp transport too (the hand-written `dispatchItemList` projects via `cli.ToItemSummaries`; `full=true` opts back into complete bodies) — see `internal/mcp/version.go` for the authoritative per-version changelog. Post-0.20 without a bump (BUG-2304): `item backlinks` / `item history` / `project report` gained HTTP route coverage — they were advertised but answered "not yet implemented over HTTP transport" — and a catalog↔route parity test (`dispatch_http_parity_test.go`) now drives every catalog action and fails on any future advertised-but-unrouted action; no names, enums, or shapes changed, hence no bump. v0.19 adds a `clear_parent` boolean to `pad_item` — the canonical, schema-discoverable way to detach an item from its parent, backed by a new `--clear-parent` bareword flag on `pad item update` (BUG-2078). v0.18 adds `clear_assigned_user` / `clear_agent_role` booleans to `pad_item` — the canonical, schema-discoverable way to unassign, backed by new `--clear-assigned-user` / `--clear-agent-role` bareword flags on `pad item update` (IDEA-2584). Update-only, deliberately asymmetric with create. v0.17 carries the empty-string clear to the LOCAL STDIO transport, which shells out to the CLI — `cmd/pad/cmd_item.go` now lifts `assigned_user_id` / `agent_role_id` onto their columns instead of into the fields blob, on create and update (BUG-2583). v0.16 makes an empty-string `assigned_user_id` / `agent_role_id` CLEAR the assignment instead of being silently dropped, so an MCP agent can finally unassign an item (TASK-2571). v0.15 adds the `pad_item.list` `unparented` boolean, mutually exclusive with `parent`, for items with no parent or implements relationship (TASK-2096). v0.2 introduced the catalog (PLAN-969 / TASK-981); v0.3 added `pad_playbook`, `pad_meta.action: bootstrap`, `pad_set_workspace`'s embedded-bootstrap response, and the `pad://workspace/{ws}/bootstrap` resource (PLAN-1377 / TASK-1380); v0.4 trimmed the bootstrap payload by ~40% (PLAN-1410) — slim `BootstrapCollection` + `BootstrapRole` projections (no UUIDs/timestamps/settings; nested `schema` object; redundant labels omitted), removed top-level `recent_activity` duplicate, dropped convention `slug`, and added a `BootstrapDashboard` wrapper that caps five sub-arrays (`attention`, `recent_activity`, `active_items`, `active_plans`, `by_role`) at 5 entries each with parallel `*_overflow_count` fields. The pre-catalog v0.1 cmdhelp leaf walker is retired.

cmdhelp is still consumed at dispatch time — `BuildCLIArgs` reads individual command schemas to translate the catalog's snake_case input map into CLI args. cmdhelp no longer drives tool naming or count.

**When adding a new `pad` command, decide whether it belongs on the MCP surface.** If yes, add an action to the appropriate `pad_<resource>` ToolDef in `internal/mcp/catalog_<resource>.go`. The action's handler — usually `passThrough([]string{"resource", "subcommand"})` — wires it through to dispatch. Don't expose interactive (prompts the user), destructive (mutates auth / filesystem state), long-running (streaming watcher), or recursive (would spawn another MCP server) commands.

```bash
pad mcp serve                 # JSON-RPC over stdio (called by clients)
pad mcp install <client>      # Write the client's mcp.json entry
pad mcp uninstall <client>    # Remove the entry
pad mcp status                # Install state across supported clients
```

Surface:
- **Tools:** the v0.26 catalog — ten resource × action tools (`pad_item`, `pad_workspace`, `pad_collection`, `pad_project`, `pad_role`, `pad_search`, `pad_meta`, `pad_playbook`, `pad_library`, `pad_attachment`) plus `pad_set_workspace` (takes a `workspace` slug only — no action enum). The ten resource × action tools take `action: <verb>` to choose what they do. `pad_item` (v0.19) exposes `clear_parent` as the canonical parent-detach (update only); (v0.18) exposes `clear_assigned_user` / `clear_agent_role` booleans as the canonical unassign (update only); (v0.17) treats an empty-string `assigned_user_id` / `agent_role_id` as a clear on BOTH transports via `field: ["assigned_user_id="]` — the direct param form is remote-only, since it isn't schema-declared and stdio's BuildCLIArgs drops unknown keys (IDEA-2584); v0.16 fixed remote only; (v0.15) adds the `unparented` list parameter; v0.14 added `history` + `expected_updated_at`. `pad_project` (v0.13) adds `ready` (actionable backlog) + `stale` (items needing attention); `pad_project.activity` (v0.12) is the non-streaming, bounded activity feed — catch up on what other agents/users changed since you last worked. `pad_attachment` is the read-only attachment-metadata surface — `list`/`show` (upload/download/view stay CLI-only). `pad_library` is the convention+playbook library surface — `list`/`get`/`activate`. `pad_playbook` is the playbook surface from PLAN-1377 — `list`/`get`/`run` mirror the CLI's `pad playbook` subcommands; `run` is side-effect-free and returns the body + bound args for the agent to execute. v0.4 (PLAN-1410) didn't change the tool/action surface; it trimmed the bootstrap JSON those tools/resources return — see the Stability contract subsection below for details.
- **Resources:** `pad://workspace/{ws}/items/{ref}`, `pad://workspace/{ws}/items`, `pad://workspace/{ws}/dashboard`, `pad://workspace/{ws}/collections`, `pad://workspace/{ws}/attachments/{id}` (bounded base64 image via `thumb-md`; non-images and image bytes over 1 MiB (pre-base64) rejected), `pad://workspace/{ws}/bootstrap` (one-shot workspace overview — user + collections + always-on conventions + roles + playbook metadata + dashboard + recent activity), plus the server-wide `pad://_meta/version`.
- **Prompts:** `pad_plan`, `pad_ideate`, `pad_retro`, `pad_onboard` — multi-step workflows lifted from `skills/pad/SKILL.md`.

**`pad_set_workspace`** pins the session default workspace; its response embeds the bootstrap blob so agents pin + load workspace context in one round-trip. The same payload is available on demand via `pad_meta.action: bootstrap` and the `pad://workspace/{ws}/bootstrap` resource.

**Stability contract.** Two version constants live in `internal/mcp/version.go`, advertised in the handshake under `capabilities.experimental.padCmdhelp` and `capabilities.experimental.padToolSurface`:
- `CmdhelpVersion` (currently `"0.1"`) — the cmdhelp CLI help-tree contract. Bump when CLI flag/arg schemas change incompatibly.
- `ToolSurfaceVersion` (currently `"0.26"`) — the MCP tool catalog contract. Bump when tool names, action enums, or parameter shapes change incompatibly. **v0.26** (IDEA-2756) is a BEHAVIOR bump on the v0.9/v0.16/v0.25 grounds — no tool name, action enum, or param shape changed, but `pad_workspace.create` now refuses a call it used to permit. Closest precedent is v0.10, which likewise turned a server-side gate into a structured refusal; unlike v0.10 there is no `allow_draft`-style override, because the gate encodes a decision the USER made at consent time and a bypass param would be the app overriding its own grant. `POST /workspaces/import` is gated by the same shared helper (import mints a workspace through `store.ImportWorkspace`), though it has no MCP action today. **v0.25** (TASK-2657 / BUG-2702) resolves `pad_library.activate`'s destination collection from the target's declared artifact kind rather than the literal `conventions` / `playbooks` slugs, so activating into a workspace that renamed either collection lands correctly; a lookup ERROR is surfaced rather than silently falling back. **v0.24** (#1066) adds the `fields` OBJECT param to `pad_item` create/update — an alias merging into the same path as `field`/the dedicated params, so the shape reads return is finally a valid write shape; the same key supplied twice with conflicting values is REFUSED (refuse-on-ambiguity, the v0.18/v0.19 disposition), equal duplicates collapse to one write, and non-writer actions refuse a `fields` param loudly. It also makes input validation STRICT for every catalog tool: undeclared top-level keys are rejected with a structured error naming them, instead of being accepted and silently dropped by `BuildCLIArgs` — which is the mechanism that made the `fields` object a session-scoped silent no-op in the first place. Compat carve-out: `pad_item`'s v0.16 `assigned_user_id` / `agent_role_id` remote-transport clear form stays accepted (documented, undeprecated, deliberately never schema-declared). The strict half changes behaviour for inputs that previously "succeeded", but that reliance was indistinguishable from a caller bug (the key never did anything), so the break is the fix; one bump covers both halves. **v0.23** (BUG-2627 part 2 + BUG-2675) refuses system-metadata keys through `fields_patch` on all three doors at once, `github_pr` exempt on update (move/copy still refuse it), and adds the retry-hostile `stored_state_unreadable` code. **v0.22** (BUG-2674) stops `pad_item.action=move` destroying system metadata and refuses `field` setters naming the reserved keys there. **v0.21** bounds `pad_item.action=history` (BUG-2608): the `limit` param now covers it, default 50 / max 300, applied in the CATALOG action so it reaches both transports (HTTP reads the input; stdio gets the CLI's new `--limit` via BuildCLIArgs). The window is the NEWEST N and there is deliberately no `offset` — versions are reverse patches, so only a newest-end window is cheap to reconstruct. Additive param bump; a v0.20 consumer sending no limit now receives the newest 50 rather than every version, which is the fix. Summary mode additionally asks the server to skip patch resolution rather than resolving bodies the dispatcher discards. **v0.19** adds a `clear_parent` boolean to `pad_item` (BUG-2078) — an ADDITIVE param bump, same grounds as v0.18; nothing existing changed shape. The server has supported clearing a parent since BUG-2013 (`extractParentLink` treats a present-but-empty `parent` key in `fields_patch` as detach), but neither client surface could reach it — `--parent ""` was a silent no-op on the CLI and the MCP `parent` param has the same "empty means not provided" convention every other declared string on the tool has. Boolean rather than overloading the empty string, same two reasons as v0.18: keeps that invariant intact for every other param, and only a boolean reaches LOCAL STDIO via `BuildCLIArgs`, mapping to a new `--clear-parent` bareword flag exactly as `clear_assigned_user` maps to `--clear-assigned-user`. Update-only, same asymmetry as v0.18. A simultaneous `parent` + `clear_parent` — including via `field: ["parent=..."]` or the `plan` alias `extractParentLink` also accepts — is REFUSED on both transports, not silently resolved (codex round 1). Also refused, not silently applied: `clear_parent` against a collection whose schema declares its own `parent`/`plan` field — `extractParentLink` skips hierarchy handling entirely for a schema-shadowed key and lets it fall through as an ordinary field write, so the wire shape `{"parent":""}` can no longer distinguish clear-hierarchy intent from a legitimate blank-a-real-field write once it reaches the server; the ambiguity is created at the client surface that accepted `clear_parent`, so that surface refuses rather than guessing (codex round 2). **v0.18** adds `clear_assigned_user` / `clear_agent_role` booleans to `pad_item` (IDEA-2584) — an ADDITIVE param bump (v0.5/v0.6 precedent); nothing existing changed shape and v0.16/v0.17's empty-string forms still work, undeprecated. v0.16 and v0.17 made the clear WORK; nothing advertised it, because the params that do it were never in the catalog, so an agent reading the schema reached for `assign: ""` (a no-op, and it stays one). Booleans rather than declaring the string params, for two reasons: an empty DECLARED string is inert everywhere else on the tool, so giving one a destructive meaning would let a param-padding client silently unassign everything; and only a boolean can reach LOCAL STDIO, since `BuildCLIArgs` emits the CLI's real flags and a param with no flag behind it is dropped — these map to new `--clear-assigned-user` / `--clear-agent-role` bareword flags, exactly as `allow_draft` maps to `--allow-draft`. Update-only, deliberately asymmetric with create (clearing at create has no honest behaviour but a no-op; a test fails if someone adds them there). Server-side it is wiring, not new semantics: `models.ItemUpdate.ClearAssignedUser`/`ClearAgentRole` already existed with store support since BUG-2566. **v0.17** closes the transport gap v0.16 documented: local stdio MCP shells out to the CLI, which wrote `--field assigned_user_id=<uuid>` into the item's FIELDS BLOB while the column stayed stale and then printed "Updated TASK-9". `cmd/pad/cmd_item.go` now lifts `columnFieldKeys` onto the columns on create AND update, mirroring `liftFieldsToColumns` and its INVARIANT. Two compat changes, ruled separately: non-empty values move to the column and stop writing the blob key (relying on the old behaviour is relying on a shadowing defect), and empty values clear (falls out of the lift, inherits BUG-2566). Existing stray blob keys are left alone — the fix stops minting new ones. Another behaviour-only bump (BUG-2583). **v0.16** lets an MCP agent UNASSIGN an item over the REMOTE transport (TASK-2571). No tool/action/param shape changed — this is a BEHAVIOR bump on the same grounds as v0.9: an empty-string `assigned_user_id` / `agent_role_id`, passed at the top level or as `field: ["assigned_user_id="]`, was silently dropped by two dispatch-path filters (`mapItemUpdate`, `liftFieldsToColumns`) and is now forwarded as a clear-to-NULL. The store has had defined clear semantics for exactly these two columns since BUG-2566 and HTTP inherited them, so this is uniformity restoration — MCP was the only surface with no way to unassign. Compat posture accepted deliberately: today's `""` senders get a no-op, and a no-op is the surprising reading. The empty-string filter on `tags` at the same call site STAYS (codex #547 r3 P2) — `tags: ""` is a corrupt JSONB/TEXT write, not a clear; same-looking guard, opposite justification. `clear_assigned_user` / `clear_agent_role` schema flags (option (b)) deliberately skipped as additive sugar, though codex review reopened the case — the catalog exposes `assign` / `role`, NOT the ID params, so an agent reading the schema still can't discover the clear (IDEA-2584); an empty `assign` is deliberately left inert because every other schema-declared string on that mapper treats empty as not-provided. **Transport scope:** v0.16 fixed the REMOTE /mcp transport only; v0.17 (BUG-2583) closed the local-stdio half at the CLI. **v0.15** adds the `unparented` boolean to `pad_item.list`, mutually exclusive with `parent`, for structural loose-item filtering (TASK-2096). **v0.14** added a `history` action to `pad_item` (read-only item version history — newest-first metadata; content body omitted for token thrift) and an `expected_updated_at` param for optimistic concurrency on `update` (round-trip the `updated_at` you last read; a stale value fails with a structured 409 `code=update_conflict`). The `update` action's field writes are now a server-side field-level MERGE (only the keys you set change) rather than a full-blob replace, closing the concurrent-update lost-write race (IDEA-1480 / TASK-2022) — pure addition to the action enum + param vocabulary; existing `pad_item` actions/params are unchanged and backwards-compatible. **v0.13** adds `ready` + `stale` actions to `pad_project`, mirroring the existing CLI `pad project ready` / `pad project stale` (TASK-2019): `ready` (read-only) returns the actionable backlog — the query-oriented counterpart to `next`, reusing the dashboard's suggested-next logic; `stale` (read-only) lists items needing attention (stalled, blocked, overdue, or out of the active workflow). Both HTTP dispatchers already existed (`dispatch_http_project.go`); this just wires them onto the catalog. `pad project reconcile` stays CLI-only (shells out to `gh` for live PR state — a local-git dependency MCP agents lack). Pure addition of two read-only actions — existing actions unchanged; backwards-compatible for v0.12 consumers that don't enumerate the new actions. **v0.12** adds an `activity` action to `pad_project`, mirroring the new CLI `pad project activity [--limit N] [--actor user|agent] [--since DATE]` (TASK-2018) — the non-streaming, bounded query counterpart to the CLI-only `pad project watch` SSE stream. Read-only snapshot of the workspace's enriched activity feed (item refs, titles, field-level change details) backed by the existing `GET /workspaces/{ws}/activity` endpoint (previously web-UI-only, now extended with a server-side `since` date filter so `limit`/`actor`/`since` behave identically across CLI, stdio MCP, and cloud HTTP), so agents can catch up on what other agents/users did since they last worked. Adds `actor` + `limit` params to the `pad_project` vocabulary (`since` already existed for changelog); pure addition — existing actions unchanged; backwards-compatible for v0.11 consumers that don't enumerate the new action. **v0.11** adds the read-only `pad_attachment` tool (the tenth resource × action tool) with `list` + `show` actions, mirroring the CLI `pad attachment list` / `pad attachment show` (TASK-2017): `list` enumerates a workspace's attachments (optional filters: item / category / collection / attached / unattached / sort / limit / offset); `show` returns one attachment's metadata (MIME, size, filename, ETag, last-modified) via a HEAD request without transferring bytes. Both HTTP dispatchers already existed (`dispatch_http_attachments.go`); this just wires them onto the catalog. Upload / download / view stay CLI-only (filesystem-bound, excluded per the catalog's exclusion rules). Pure addition — existing tools/actions unchanged; backwards-compatible for v0.10 consumers that don't enumerate the new tool. The base64 image RESOURCE for multimodal agents (`pad://workspace/{ws}/attachments/{id}`) shipped later in TASK-2077 (PR #930) as a bounded, image-only resource; TASK-2101 brought it — and the full read-only resource set — to the remote /mcp transport via the in-process `HTTPResourceFetcher`, so resources are no longer local-stdio-only. **v0.10** enforces the draft-playbook gate server-side: `pad_playbook.run` (and the underlying `POST /playbooks/{ref}/run`) now refuses a playbook whose `status` isn't `active` with a structured `playbook_not_active` error, adds an `allow_draft` boolean param (bareword `--allow-draft` on the CLI) as the escape hatch, and echoes the playbook `status` on both the `run` and `get` responses (BUG-2020). **v0.9** makes `pad_item.list` summary-shaped by default (drops item `content`, adds a default result limit of 50 / hard max 300 on MCP; CLI `--full` restores the complete shape) — a behavior change to the tool's return shape, hence the bump, though tool names, action enums, and parameter shapes are unchanged (TASK-2000). **v0.8** adds `restore` + `deleted` actions to `pad_workspace`, mirroring the CLI `pad workspace restore` / `pad workspace deleted` (TASK-1972): `deleted` (read-only) lists the caller's soft-deleted workspaces still inside the 30-day restore window; `restore` (mutating, not destructive, owner-only) un-soft-deletes a workspace by `slug` while it's still restorable. Both reuse the existing `slug` param — no new params; pure addition. **v0.7** adds `export` + `import` actions to `pad_item`, mirroring the CLI `pad item export` / `pad item import` (covers playbooks AND conventions). `export` (read-only) takes `ref` and returns the portable artifact text — it forces the CLI's stdout sink (`-o -`) so the bytes come back as the result instead of a file. `import` (mutating, not destructive) takes a new `artifact` param (the full artifact text) and returns `{ref, slug, warnings}`; the ExecDispatcher can't pipe stdin, so it spills the artifact to a temp file and dispatches `item import <tmpfile>`. v0.6 added the `pad_item.backlinks` action; v0.5 added `pad_library`. v0.3 (PLAN-1377 / TASK-1380) introduced `pad_meta.action: bootstrap`, `pad_set_workspace`'s embedded-bootstrap response, and the `pad://workspace/{ws}/bootstrap` resource. **v0.4 (PLAN-1410)** is a comprehensive bootstrap-payload trim — same tool catalog, slimmer JSON shape inside bootstrap responses: `BootstrapCollection` projection drops `id`/`workspace_id`/timestamps/`settings` and emits `schema` as a nested object; `BootstrapRole` projection drops UUIDs/timestamps/`tools`; convention `slug` dropped; top-level `recent_activity` (a duplicate of `dashboard.recent_activity`) removed; new `BootstrapDashboard` wrapper caps five sub-arrays (`attention`, `recent_activity`, `active_items`, `active_plans`, `by_role`) at 5 entries each with parallel `*_overflow_count` fields; redundant schema labels omitted when `label == TitleCase(key)`. Cumulative size reduction: ~40% on a representative workspace, ~54% on the fixture (see PLAN-1410's Result section for per-section deltas). Compatibility: most changes are subtractive (dropped fields) or additive (overflow counts), but **one type change is breaking**: `collections[].schema` went from a JSON-encoded string to a nested JSON object — clients that JSON.parse()'d the string need to consume it directly as an object now. The dropped fields (UUIDs, timestamps, settings, duplicate `recent_activity`, convention `slug`) have canonical alternatives (slugs for addressing; `pad collection list` / `pad role list` for the full models when needed).

Both are also returned by `pad://_meta/version` and `pad_meta.action: version`.

**Where result caps live.** Two layers, deliberately different numbers. The MCP catalog action injects the agent-facing default and ceiling (list / backlinks / history: default 50, max 300) because a token budget is only knowable there. The HTTP endpoint's own clamp is a server-resource ceiling on what any caller may ASK for (`maxItemListQueryLimit` = 1000; `maxItemVersionsQueryLimit` = 500, lower because resolving a version can cost a patch application per row), and an ABSENT limit is left unbounded rather than defaulted — a server that truncates a request nobody bounded is a silent-truncation trap for direct API consumers. The CLI carries its own default for the same reason the catalog does.

**Dispatchers.** Two ship in `internal/mcp/`:

- `ExecDispatcher` — shells out to the `pad` binary; subprocess inherits credentials from `~/.pad/credentials.json`. Used by `pad mcp serve` for local stdio MCP.
- `HTTPHandlerDispatcher` — calls pad-cloud's HTTP handlers in-process with the requesting user attached via `server.WithCurrentUser`. Backs the **live** remote MCP server on the dedicated `mcp.getpad.dev` vhost (PLAN-943), where the dispatcher serves multiple OAuth users from a single process. The Streamable HTTP transport is mounted by `Server.SetMCPTransport` / `registerMCPRoutes` (cloud-mode-gated; self-hosted binaries leave it unmounted) — see `internal/server/handlers_mcp.go`. Tools are wired into the route table at `internal/mcp/dispatch_http.go` (`routeTable`); add a `RouteMapper` per command — `mapItemCreate` is the seed entry from TASK-965.

**Resource fetchers.** The read-only resource templates (`RegisterResources` in `internal/mcp/resources.go`) are transport-agnostic — they parse the pad CLI's `--format json` output, and a `ResourceFetcher` supplies those bytes. Two implementations mirror the dispatchers:

- `ExecResourceFetcher` — shells out to `pad` (stdio MCP), same credential model as `ExecDispatcher`.
- `HTTPResourceFetcher` (`internal/mcp/resources_http.go`, TASK-2101) — the in-process equivalent for remote /mcp. It translates each resource's fixed CLI-arg vector into an in-process HTTP read through the same handler chain (reusing `HTTPHandlerDispatcher`'s user resolution + `buildAuthedRequest` auth/scope/consent perimeter), reproducing the CLI shape the handlers expect (e.g. `item list` → `cli.ToItemSummaries`, `workspace list` → `{slug,name,updated_at}`, `attachment show` → HEAD-header synthesis). Attachment bytes flow through a `cappedResponseWriter` that preserves PR #933's 1 MiB download bound. Because it satisfies `ResourceFetcher`+`BinaryResourceFetcher`, all resource handlers register unchanged on both transports.

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

**Surface-agnostic body.** The playbook body (`internal/collections/playbook_library_onboard.go::onboardPlaybookBody`) describes intent, not specific CLI commands. It instructs the agent to use whatever surface it has — `pad_item` MCP, `pad item` CLI, `pad_collection` MCP, etc. — and works for pure-MCP agents (no shell) the same as for Claude Code. The body's `mode` argument is `auto` (default — detects from workspace state; any user-created item routes to revisit), `build` (blank workspace, build from scratch), `audit` (templated workspace, adapt seeded items), or `revisit` (already-onboarded, change something specific), plus a separate `defaults` flag (escape hatch — skip the interview, pick sensible defaults and report).

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
cd web && npm run test     # Web unit tests (vitest, run once)
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
1. Add the command constructor to the matching resource file under `cmd/pad/` — `cmd_item.go`, `cmd_collection.go`, `cmd_workspace.go`, `cmd_auth.go`, `cmd_project.go`, `cmd_playbook.go`, `cmd_role.go`, `cmd_tag.go`, `cmd_github.go`, `cmd_webhook.go`, `cmd_agent.go`, `cmd_server.go`, `cmd_attachment.go`, `cmd_db.go`, `cmd_library.go`, `cmd_bootstrap.go` (all `package main`, so helpers are shared across files). Create a new `cmd_<resource>.go` if none fits. Keep `main.go` for `main()`, `newRootCmd()`, and top-level wiring only — don't grow it back into a god file.
2. Wire it into the resource group in `cmd/pad/groups.go` (or `rootCmd.AddCommand()` in `main.go` for a new top-level group)
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
