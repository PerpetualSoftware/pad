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

- **Give each worktree its own `web/node_modules`: run `npm ci` in its `web/`. Do not symlink the main checkout's copy.** It installs from the npm cache in about 5s warm, costs about 470 MB of disk per live worktree, and runs `svelte-kit sync` as a side effect. A symlink tests the worktree against whatever the MAIN checkout last installed. `web/package-lock.json` changes every few days, and the main checkout is not kept current, so that is routinely the wrong dependency set: on day 86 (2026-10-05), worktrees borrowed Tiptap 3.31.3 after main had moved to 3.31.4, plus a materializer bundle built from it, and the corpus tests failed for reasons that had nothing to do with the change. SvelteKit 3 also writes its generated tsconfig inside `node_modules` with relative paths, so through a symlink a worktree's type check would silently check the main checkout's tree (TASK-3423). (Dave's ruling, 2026-10-06. A pnpm-style shared store is the follow-up if disk becomes the constraint.)
- **A fresh worktree has no `web/.svelte-kit`** (gitignored, generated). Run `npx svelte-kit sync` in `web/` before any vitest/vite command — or `npm run check`, which syncs first. Without it, vitest fails with `Failed to load tsconfig '.svelte-kit/tsconfig.json': Tsconfig not found` regardless of how `node_modules` was set up. (This missing generated dir was historically misdiagnosed as a symlink problem — `npm ci` "fixed" it only because its `prepare` script runs `svelte-kit sync`.)
- **A fresh worktree has no `web/build` either**, and that one breaks the GO gates rather than the web ones. `embed.go` does `//go:embed all:web/build`, so `make test` and `make lint` fail with `pattern all:web/build: no matching files found` before a single test runs — `go build ./...` dies first, two packages report `[setup failed]`, and it reads as a broken tree rather than a missing generated directory. `npx vite build` in `web/` fixes it (`make web` would too, and is FORBIDDEN here — see the next rule). **It also has no `web/build-materializer`** (TASK-2198, gitignored, generated): `embed.go` does `//go:embed web/build-materializer/materializer.js`, and a missing file fails `go build ./...` the same way, with `pattern web/build-materializer/materializer.js: no matching files found`. Run `node scripts/build-materializer.mjs` in `web/` after `npx vite build` (`npm run build` does both). It is built with esbuild, which is a devDependency of its own (vite 8 does not bring it), so a `web/node_modules` older than the lock file that added it cannot build the bundle. Unlike `web/build`, a placeholder file here is not enough for `make test`: `internal/materialize`'s tests run the real bundle.

- **If you find a worktree whose `web/node_modules` is still a symlink (an older one), do not run `npm ci` in it, including via make.** `npm ci` would delete through the symlink into the main checkout's tree and break every session that shares it, with a confusing `vitest: not found`. `npm ci` lives in the `web` target, so every target whose dependency chain reaches it is affected too: currently `web`, `build`, `install`, `serve`, `web-check`, and `check` (via `web-check`). Replace the symlink first: `rm web/node_modules` removes the link only, then run `npm ci`. In a worktree with its own `node_modules`, those targets are safe.

- **`make test-pg` is safe to run from several worktrees at once, but NOT against itself in one worktree** (TASK-2708; the second half measured on BUG-3000). `TEST_PG_PROJECT` is derived from the worktree's basename and a checksum of its absolute path, so two overlapping runs in the SAME worktree resolve to the SAME compose project: the second `up -d --wait` attaches to the first run's container instead of creating its own, and the first run to finish executes the target's unconditional `down -v` and tears the database out from under the other. The survivor fails with `dial tcp 127.0.0.1:<port>: connect: connection refused` while cloning the test database — which the `THE DATABASE DIED DURING THE RUN` banner reports correctly, but reads as evidence about the code to anyone who does not know a sibling run was up. Run it alone per worktree, and do not diagnose such a failure from surrounding connection-refused noise (a passing run carries ~41 redis connection-refused lines of its own); the failure TEXT is the evidence. It used to bind the Postgres test container to a fixed host port, so a second worktree failed with `port is already allocated` and a stack orphaned by a removed worktree blocked the port for everyone. Docker now assigns the port and the Makefile reads it back, so each worktree gets its own container on its own port under its own compose project. If you have been starting a private container by hand to avoid the collision, you no longer need to. Two other things that target now does: it REFUSES to run, with a `NO TESTS EXECUTED` banner, when the database is unreachable — `go test` exiting 2 with zero FAIL lines had already been mistaken for a pass once — and it says so explicitly when the database dies mid-run, so the failures read as infrastructure rather than as evidence about the code. To reap a stack whose worktree was deleted before teardown: `docker ps --filter name=padtest-` lists them and the container name carries the compose project, so `docker compose -p <that-project> down -v` reaps it from anywhere. From inside the worktree, `make test-pg-project` prints the name and `make test-pg-down` does it for you. (The project is `padtest-<basename>-<checksum-of-the-absolute-path>` — NOT the bare directory name, which would collide between two checkouts sharing a basename.)

- **Go build caches never go under `/tmp`.** `/tmp` is a 7.3G tmpfs, so every byte written there is RAM. Two ad-hoc `/tmp/docapp*-go-cache` directories created by agent sessions held 3.6G between them and were what the harness's memory reaper was reacting to when it started killing background test runs. Use the default (`~/.cache/go-build`), or `~/.cache/go-build-<worktree>` on disk if you want a per-worktree cache to avoid lock contention on the shared one. `GOTMPDIR` already points at `~/.cache/go-tmp` and stays there.

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

REST API at `/api/v1/`. **Before adding or changing an endpoint, a status or error code, or a request or response shape, read [docs/api.md](docs/api.md).** It is the endpoint reference, moved out of this file verbatim (TASK-3485), and new endpoint notes go there, not here: accretion is how this file passed Claude Code's 150k-character limit.

What docs/api.md covers:
- The workspace 404 marker (`details.scope`), and collection and item CRUD: list field filters; the write warnings (`undeclared_fields`, `content_outcome`, `dropped_fields`, `options_added`, `pruned_pending_edits`); `content_state`; reserved metadata keys and `github_pr`; `refuse_undeclared_fields`; pending-flush refusals; the append members; field usage and `warnings.orphaned`; archived collections and restore.
- Item copy and preflight, version restore and diff, collab set-aside rows, library activation and built-in origin, attachment attach, reminders, typed decisions, playbook match.
- Dashboard, activity, webhooks, item-scoped comment writes and tombstones, children, progress, links, search.
- SSE (`/events`, `/events/stream`), the rule that every long-lived connection ends with its credential, the SSE and collab admission budgets, and the per-tab content-write ordering.
- Members and app principals, workspace tabs, invitations, tutorials, UI dismissals.
- Auth (session, bootstrap, register, login, logout, me, password reset, local reset, tokens), admin settings, MCP over HTTP, the decision provider, and the agent bootstrap.

## Authentication

User-based authentication with email/password. When no users exist (fresh install), everything works without auth until the instance is initialized with `pad auth setup`. Once the first admin exists, all API requests require authentication.

```bash
# First-time setup
pad auth setup         # Create the first admin account on the server host

# Subsequent logins
pad auth login         # Browser-based login (add -i for an email + password prompt)
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
pad workspace invite user@example.com         # Invite: always a PENDING invitation, never a direct add (BUG-2136)
pad workspace invite user@example.com --role viewer  # Invite with specific role
pad workspace invitations                     # Your pending invitations (with ids)
pad workspace accept <id>                     # Accept one of them
pad workspace decline <id>                    # Decline one (deletes it)
pad workspace join <code>                     # Accept a workspace invitation by code
```

An invitation becomes a membership only when the invitee accepts, from the CLI above, the web "+" menu (which shows a count badge), or the `/join/<code>` link. The link asks a signed-in visitor, including one who signs in from it, to Accept or Decline; only registering a NEW account through the code joins in one step. Inviting the same address again replaces the pending invitation.

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
                              # RELATION fields (TASK-2878): a carried value is resolved against the
                              # workspace, so a valid relation SURVIVES a move and only an unresolvable
                              # one is dropped (and reported). A `--field` OVERRIDE naming a relation is
                              # a write and is REFUSED if it does not name a live item in the collection
                              # that field declares.
                              # A `--field` OVERRIDE naming a field the TARGET collection does not declare
                              # is REFUSED (400 malformed_override, BUG-2379), as `item copy` always did.
                              # COMPUTED and UNIQUE target fields (BUG-2367, same on copy): a value is never
                              # carried into a computed field; a carried value another item in the target
                              # already holds on a unique_scope field is DROPPED and printed as
                              # `  dropped: <field> "<value>" is taken by <REF>` (the holder is named only
                              # when you may see it); a `--field` value that collides is refused 409.
                              # STATE CHANGE (BUG-2367 item 4): a move that would change the item between
                              # open, done and abandoned (e.g. a done task landing on ideas' default `new`)
                              # is REFUSED unless you name the done field; the CLI prints the exact
                              # `--field status=<a|b|c>` hint. Two values that both mean done carry.
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
                              # move/copy, where the repo context is unchanged.
                              # RELATION fields (TASK-2878): every CARRIED relation value is dropped on a
                              # cross-workspace copy without a lookup — it names a row in the SOURCE
                              # workspace, so nothing in the destination could make it true — and is
                              # reported as `referent_not_portable`, the same bucket github_pr uses. The
                              # preflight reports the identical drop; both doors call one store function,
                              # because they sit in different packages and that is how they drift.
                              # A supplied `--field` override naming a relation must resolve in the
                              # DESTINATION workspace or the copy is refused.
                              # None of these four keys
                              # (+ `convention`) is settable via `--field` on copy or move — they are
                              # written by `pad item note` / `pad item decide` / `pad github link`.
pad attachment attach <attachment-id> <item-ref>  # bind an unattached ("upload -") attachment to an item, so a share of that item renders it
pad library diff <ref>        # a built-in convention/playbook vs Pad's current library text: what the library changed,
                              # what you changed, and the settings an update would replace (TASK-3462). Read-only
pad library update <ref> [--overwrite-pending-edits]
                              # take the library's text (body + settings; never status or title), guarded by the seq
                              # read just before. Both refuse a server without the `builtin_update` capability. Not on MCP
pad item remind <ref> --remind-at <RFC3339>   # arm a one-shot reminder; --rearm <id> moves an existing one
pad item reminders <ref>      # list an item's reminders (armed / fired / acknowledged)
pad item ack <reminder-id>    # acknowledge a fired reminder, removing it from `project next` / `ready`
pad item unremind <reminder-id>  # disarm
                              # A reminder fires at an instant, emits item.reminder_due on the outbox rails,
                              # and appears in `pad project next` / `ready` until acknowledged. NOTHING else
                              # acknowledges one — completing the item does NOT, since a reminder may have
                              # been armed to fire after the work was done; a reminder on a completed item is
                              # hidden from the recommendation surface and left untouched in the table.
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
pad item edit <ref> [--force] # Open in $EDITOR. The save is guarded by the seq it was seeded from (a conflict or failed
                              # save writes your text to a recovery file and prints its path); refuses a stale body
                              # (content_state) unless --force (BUG-3035), and --force also sends
                              # overwrite_pending_edits on the save (BUG-3133)
pad workspace init [--template X]  # Create workspace
pad agent install [tool]      # Install /pad skill for AI tools
                              # Every skill file pad writes ends with a stamp line (<!-- pad:skill v=… sha256=… -->,
                              # BUG-3466). pad init / workspace init / agent install / agent update KEEP a file that
                              # was edited (its body no longer hashes to its stamp, or an unstamped file matching no
                              # past release's output in internal/cli/skill_legacy_hashes.go) or that a newer pad
                              # wrote, and say so; --force (on agent install and agent update) replaces it. Hashes
                              # ignore CRLF and trailing newlines. A dev build never refuses as a downgrade.
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

Pad serves its CLI as MCP tools, over local stdio (`pad mcp serve`) and as remote Streamable HTTP at `/mcp`. **Before adding or changing a tool, an action, a parameter, an annotation, a result shape or the server instructions, read [docs/mcp.md](docs/mcp.md).** It is the MCP reference, moved out of this file verbatim (TASK-3485), and new MCP notes go there, not here.

What docs/mcp.md covers: the tool catalog and its actions, resources and prompts; the stability contract (`CmdhelpVersion`, `ToolSurfaceVersion` and its full changelog); the golden wire files; where result caps live; the dispatchers and resource fetchers; `pad mcp` install commands.

Two rules to apply without opening it:
- Any catalog, annotation, schema or `instructions.md` change moves the golden bytes. Regenerate with `PAD_UPDATE_MCP_GOLDEN=1 go test ./cmd/pad/ -run TestMCPWireGolden`, and decide from that diff whether `ToolSurfaceVersion` (`internal/mcp/version.go`) owes a bump.
- When adding a `pad` command, decide whether it belongs on the MCP surface; docs/mcp.md says how.

## Data Model

- **Collections** have JSON schemas defining typed fields (select, text, date, number, etc.)
- **Items** have structured `fields` JSON + optional rich `content` (markdown)
- **Parent/child links:** Any item can be a parent of child items (`--parent REF`). Children get progress tracking, burndown charts, and nested rendering. Plans are the most common parent, but Ideas, Docs, or Tasks can also have children.
- **Terminal vs abandoned:** a select field's `terminal_options` are the values that CLOSE an item; its optional `abandoned_options` (BUG-2347) are the subset that close it WITHOUT delivering (tasks `cancelled`, bugs `wontfix`, ideas `rejected`, candidates `rejected`/`withdrawn`, …). Completed-work views — `pad project changelog`, `standup`'s completed list, report throughput — count terminal − abandoned, and omit abandoned items entirely. A field that declares none falls back to a global name list (`models.NegativeTerminals`: rejected, cancelled, wontfix, duplicate, declined, abandoned, disabled…), so a custom abandon word like `overturned` counts as SHIPPED until the collection declares it. `archived` is deliberately NOT in the fallback — declare it per collection where it means "not delivered". Set via `pad collection update <slug> --schema …` (or the web schema editor); `abandoned_options` must be a subset of `terminal_options` or the write is refused `400 validation_error`. Additive schema key, no MCP version bump. Templates seed it.
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
- `internal/materialize/` + `web/src/lib/collab/materializer/` — headless op-log materializer (TASK-2198): the editor's own JS, bundled by `web/scripts/build-materializer.mjs`, run in goja in a `pad __materialize-worker` process. Not wired into the server yet

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
HTTP 400 and the room manager empties the per-item op-log so the new
client doesn't replay incompatible old-schema ops. items.content is
untouched. The UNFLUSHED edits (content-bearing op-log rows above the
flush watermark) are not in it, and before BUG-3244 that prune deleted
them and cleared `content_state`, so the stale body read as current.
They are now SET ASIDE (`item_yjs_updates_set_aside`) and the item reads
`content_state: superseded_set_aside` until they are recovered or
discarded (`pad item set-aside <ref> [--discard]`). Nothing turns them
back into text yet, so **the schema version stays FROZEN:**
`internal/collab/schema_version_guard_test.go` fails on any bump until
TASK-3246 (a decoder for the outgoing era) lands and the freeze is ruled
lifted, and it also fails when the two constants disagree.

Pure UI/CSS/behavioural changes that don't alter the persisted
document shape DO NOT bump the schema version. When in doubt, load
an item edited under the old version after your change and confirm
the rendered tree is identical.
