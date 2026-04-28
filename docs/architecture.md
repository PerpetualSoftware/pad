# Architecture

High-level map of the Pad codebase for contributors. CLAUDE.md covers the
same ground but is written for AI agents working in the repo — this doc is
the human-readable companion.

## Shape

Pad ships as one Go binary. The SvelteKit web UI is built into static
assets and embedded into the binary at compile time via `go:embed`, so a
deployed Pad has exactly one moving piece on the filesystem. SQLite is the
default backing store; PostgreSQL + Redis is the alternate mode for
multi-node production use.

```
┌────────────┐     ┌────────────┐     ┌───────────────┐
│  CLI (pad) │     │  Web UI    │     │  AI agents    │
│            │     │  (Svelte 5)│     │  via /pad     │
└─────┬──────┘     └──────┬─────┘     └───────┬───────┘
      │ HTTP              │ HTTP/SSE          │ HTTP
      ▼                   ▼                   ▼
              ┌─────────────────────────────┐
              │       Pad HTTP server       │
              │  (internal/server/*.go)     │
              └─┬────────┬───────────────┬──┘
                │        │               │
                ▼        ▼               ▼
          ┌──────────┐ ┌─────────┐ ┌────────────┐
          │ SQLite / │ │ EventBus│ │ Webhooks + │
          │ Postgres │ │ (SSE)   │ │ Email      │
          └──────────┘ └─────────┘ └────────────┘
```

## Backend (Go)

- **`cmd/pad/main.go`** — Cobra CLI entry point. Every `pad <command>` is
  registered here. The same binary also serves as the daemon
  (`pad server start`) and as the CLI client that talks to it.
- **`internal/server/`** — HTTP router (chi), middleware, handlers, SSE
  hub. `server.go` is the main router; `handlers_*.go` group endpoints
  by resource.
- **`internal/store/`** — SQL abstractions and migrations. Each resource
  (workspaces, items, users, webhooks, sessions, …) has a `<resource>.go`
  file; `migrations/` is the golang-migrate style migration sources.
  Backed by SQLite by default, PostgreSQL when
  `PAD_DB_DRIVER=postgres`.
- **`internal/models/`** — shared Go structs that flow through the API
  response boundary (`Collection`, `Item`, `User`, `View`, etc.).
- **`internal/items/`** — field-schema validation: collection schemas
  declare typed fields (select, text, date, number, …) and this package
  validates item fields against them.
- **`internal/collections/`** — default collection definitions per
  template (`startup`, `scrum`, `hiring`, `interviewing`, `product`)
  and workspace bootstrap logic.
- **`internal/cli/`** — HTTP client used by the CLI to talk to the local
  daemon, plus formatting helpers for terminal output.
- **`internal/events/`** — in-process EventBus that fans SSE updates to
  connected clients. In Redis mode, a pub/sub bridge replaces the
  in-memory bus so multiple Pad replicas stay in sync.
- **`internal/webhooks/`** — outbound webhook dispatcher with HMAC
  signing, retries, and delivery log.
- **`internal/email/`** — transactional email via Maileroo. Used for
  workspace invitations and password resets; nil-safe if unconfigured.
- **`internal/diff/`** — per-item version history storage + diff
  rendering.
- **`internal/links/`** — resolver for `[[wiki-link]]` syntax across
  items.
- **`internal/config/`** — workspace detection, `.pad.toml` parsing,
  environment-variable loading.

### Request flow

1. CLI or web UI sends an HTTP request to `/api/v1/…`.
2. Middleware chain in `internal/server/middleware_*.go` handles auth,
   rate limiting, metrics, audit logging.
3. The handler in `internal/server/handlers_*.go` parses the request,
   calls one or more `internal/store/*` methods, and writes the JSON
   response.
4. If the mutation is observable (item change, comment, etc.), the
   handler publishes an event to `internal/events` which fans out to
   connected SSE clients at `/api/v1/events`.

## Frontend (SvelteKit + Svelte 5)

- **`web/src/routes/`** — page routes. File-based: a folder corresponds
  to a URL segment. `[username]/[workspace]/...` is the main
  workspace-scoped tree; `console/` is the server-admin UI.
- **`web/src/lib/components/`** — reusable UI (BottomSheet, FieldEditor,
  ReactionPicker, NestedChildren, etc.).
- **`web/src/lib/stores/`** — Svelte 5 rune-based stores for cross-route
  state (current workspace, page title, current user).
- **`web/src/lib/api/client.ts`** — typed HTTP client. Every REST
  endpoint the backend exposes has a method here; adding an endpoint
  means adding a client method too.
- **`web/src/lib/types/index.ts`** — mirrors `internal/models/` as
  TypeScript types.

The web UI is built with `npm run build` (static adapter) and the
`build/` output is embedded into the Go binary via `//go:embed` in
`internal/server/embed.go`. `npm run dev` runs a Vite dev server on
`:5173` that proxies API requests to the running Pad daemon on `:7777`
— fast iteration without rebuilding the binary.

## Data model

```
Workspaces
  └── Collections            (typed by a JSON schema)
        └── Items            (structured fields + optional markdown content)
              ├── parent/child links
              ├── blocks / blocked-by dependency links
              └── comments, reactions, tags
```

- **Collections** have a `fields` JSON schema that declares field keys
  (e.g. `status`, `priority`, `due_date`) with types and options.
- **Items** have structured `fields` JSON validated against the
  collection's schema, plus optional rich Markdown `content`.
- **Parent/child links** power progress tracking and burndown; any item
  type can be a parent of any item type.
- **`[[wiki-link]]` syntax** resolves across all items in a workspace
  and renders as clickable links in the UI.

## CLI ↔ daemon model

There is only one binary, `pad`. Some commands run purely client-side
(`pad item show REF`), but most go through the daemon:

- `pad server start` — run the daemon foreground (normal dev mode).
- `pad auth configure` — first-run credential setup, auto-starts the
  local daemon on first use.
- All other `pad <verb>` commands are CLI → HTTP → daemon → SQLite.

The CLI discovers the daemon via `~/.pad/credentials.json` (see
`internal/cli/client.go`). In Remote or Cloud mode, the same client
targets a network-served Pad instance instead of a local daemon.

## Agent integration

`skills/pad/SKILL.md` ships inside the binary and gets installed into
an AI agent's configuration by `pad agent install`. The skill is
natural-language — it documents the CLI well enough that any
Claude/Cursor/Copilot-style agent can drive Pad via terminal calls.

## Testing

- **Go:** `go test ./...` covers the backend; `internal/store/` tests
  run against real SQLite by default and against PostgreSQL when
  `PAD_TEST_POSTGRES_URL` is set (see `Makefile` targets `test` and
  `test-pg`).
- **Web:** `cd web && npm run build` to catch type / build errors;
  `npm run check` runs svelte-check.
- **CI:** `.github/workflows/ci.yml` runs the full matrix — Go
  (SQLite + PostgreSQL + race), govulncheck, golangci-lint (new-issues
  mode), web build, npm audit, svelte-check.

## Build and install

See `CLAUDE.md` for the day-to-day commands (`make install` is the one
you'll run most). The short version:

- `make build` — build web UI + Go binary to `./pad`.
- `make install` — build, kill running daemon, install to
  `$HOME/.local/bin/pad`, restart. **Heads-up:** `make install` runs
  `killall -9 pad` system-wide, so any other `pad` process on the host
  (including other users' daemons) gets killed.
- `make dev-web` — SvelteKit hot-reload dev server.

## Further reading

- [`CLAUDE.md`](../CLAUDE.md) — agent-focused development guide
  (identical scope, different audience).
- [`docs/deployment.md`](deployment.md) — full environment-variable
  reference, production deployment shapes.
- [`docs/backup.md`](backup.md) — backup and restore procedures.
- [`SECURITY.md`](../SECURITY.md) — reporting a vulnerability, threat
  model, hardening tips.
