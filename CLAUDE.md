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
- `GET /workspaces/{ws}/dashboard` — computed project overview (active items, phases, attention, blockers)
- `GET /workspaces/{ws}/activity` — workspace activity feed (enriched with item titles + change details)
- `GET/POST/DELETE /workspaces/{ws}/webhooks` — webhook management
- `GET /workspaces/{ws}/items/{slug}/children` — child items linked to a parent
- `GET /workspaces/{ws}/items/{slug}/progress` — child item completion progress
- `GET/POST /workspaces/{ws}/items/{slug}/links` — item relationships (blocks/blocked-by, parent/child)
- `GET /search?q=query&workspace=slug` — full-text search
- `GET /api/v1/events?workspace=slug` — SSE real-time events
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
pad collection create "Name" --fields "key:type[:opts]; ..."
pad item edit <ref>           # Open in $EDITOR
pad workspace init [--template X]  # Create workspace
pad agent install [tool]      # Install /pad skill for AI tools
pad workspace onboard         # Analyze codebase, suggest conventions
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

## Data Model

- **Collections** have JSON schemas defining typed fields (select, text, date, number, etc.)
- **Items** have structured `fields` JSON + optional rich `content` (markdown)
- **Parent/child links:** Any item can be a parent of child items (`--parent REF`). Children get progress tracking, burndown charts, and nested rendering. Phases are the most common parent, but Ideas, Docs, or Tasks can also have children.
- **Wiki-links** `[[Title]]` resolve across all items, rendered as clickable links
- **Default collections:** Tasks, Ideas, Phases, Docs
- **Templates:** startup (default), scrum, product — set via `pad workspace init --template`

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
