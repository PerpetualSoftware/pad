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
- `GET /workspaces/{ws}/activity` — workspace activity feed
- `GET/POST/DELETE /workspaces/{ws}/webhooks` — webhook management
- `GET/POST /workspaces/{ws}/items/{slug}/links` — item relationships (blocks/blocked-by)
- `GET /search?q=query&workspace=slug` — full-text search
- `GET /api/v1/events?workspace=slug` — SSE real-time events

## CLI

```bash
pad create <collection> "title" [--status X] [--priority X]
pad list [collection] [--status X] [--all]
pad show <slug>
pad update <slug> [--status X] [--priority X]
pad delete <slug>
pad move <slug> <target-collection>
pad search "query"
pad status                    # Project dashboard
pad next                      # Recommended next task
pad standup [--days N]        # Daily standup report
pad changelog [--days N]      # Release notes from completed items
pad blocks <source> <target>  # Create dependency
pad blocked-by <item> <blocker>
pad deps <slug>               # Show dependencies
pad unblock <source> <target>
pad collections               # List collections
pad collections create "Name" --fields "key:type[:opts]; ..."
pad edit <slug>               # Open in $EDITOR
pad init [--template X]       # Create workspace
pad install [tool]            # Install /pad skill for AI tools
pad onboard                   # Analyze codebase, suggest conventions
pad open                      # Open web UI in browser
pad watch                     # Real-time activity stream
```

Collection names accept singular forms: `task`→`tasks`, `idea`→`ideas`, `doc`→`docs`.

## Data Model

- **Collections** have JSON schemas defining typed fields (select, text, date, number, etc.)
- **Items** have structured `fields` JSON + optional rich `content` (markdown)
- **Wiki-links** `[[Title]]` resolve across all items, rendered as clickable links
- **Default collections:** Tasks, Ideas, Phases, Docs
- **Templates:** startup (default), scrum, product — set via `pad init --template`

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
