# Pad (docapp) — build guide

Project management tool for developers and AI agents. One Go binary with an embedded SvelteKit web UI, SQLite (Postgres in cloud mode) storage, and a `/pad` agent skill. The marketing site is a separate repo at `../pad-web`.

## Architecture

- Backend: Go. `cmd/pad/` (Cobra CLI) -> REST API `internal/server/` -> `internal/store/` (SQLite/Postgres).
- Frontend: SvelteKit 2 + Svelte 5 in `web/`, static build embedded in the binary (`embed.go`).
- Data model: Workspaces -> Collections (typed, JSON schema) -> Items (structured `fields` + markdown `content`). Parent/child links give progress tracking.
- Agent skill: `skills/pad/SKILL.md`, embedded in the binary.

## Commands

```bash
make build      # web UI + Go binary (./pad)
make build-go   # Go only (backend-only changes)
make web        # web UI only
make install    # build, install to ~/.local/bin/pad, restart server (UI on :7777)
make dev-web    # Vite dev server with hot reload on :5173
make test       # Go tests
make lint       # Go lint
make test-pg    # Go tests against a throwaway Postgres container (needs docker)
go test ./internal/store/            # one package
cd web && npm run test               # vitest, run once
cd web && npm run check              # svelte-check (runs svelte-kit sync first)
cd web && npm run build              # verify frontend compiles + builds materializer bundle
```

Run `make install` after changes so the running server picks them up.

## Layout

```
cmd/pad/main.go, cmd_*.go, groups.go   CLI (main.go is wiring only; add commands in cmd_<resource>.go)
internal/server/       HTTP handlers, SSE, middleware; routes in server.go setupRouter()
internal/store/        CRUD, migrations (internal/store/migrations/), FTS
internal/models/       Go types
internal/items/        field validation against schemas
internal/collections/  default definitions, templates, playbook library
internal/cli/          HTTP client + formatting
internal/mcp/          MCP catalog (stdio and /mcp)
internal/collab/       Yjs WebSocket relay; internal/materialize/ headless materializer
internal/events/ config/ diff/ webhooks/ email/ links/
web/src/routes/        SvelteKit pages
web/src/lib/           api/client.ts, types/index.ts, stores/ (runes), components/, collab/
skills/pad/SKILL.md    agent skill
```

## Worktrees (generated dirs a fresh worktree lacks)

- Give each worktree its own `web/node_modules`: run `npm ci` in `web/`. Never symlink the main checkout's copy: it holds a different dependency set, and SvelteKit's generated tsconfig lives inside it, so a symlinked type check silently checks the main tree.
- If you find a worktree whose `web/node_modules` is a symlink, do NOT run `npm ci` or any make target that reaches it (`web`, `build`, `install`, `serve`, `web-check`, `check`): `npm ci` deletes through the link and breaks every session sharing it. `rm web/node_modules` (removes the link only) first.
- No `web/.svelte-kit`: run `npx svelte-kit sync` in `web/` before vitest/vite, or vitest fails with "Tsconfig not found".
- No `web/build`: `embed.go` embeds `all:web/build`, so `make test`/`make lint` fail with "no matching files found". Run `npx vite build` in `web/`. Do not use `make web` in a worktree without its own `node_modules` (see above).
- No `web/build-materializer`: embedded too. Run `node scripts/build-materializer.mjs` in `web/` after `npx vite build` (`npm run build` does both). It needs esbuild, so a `node_modules` older than the lockfile cannot build it. A placeholder file is not enough: `internal/materialize` tests run the real bundle.
- `make test-pg` is safe across worktrees (each gets its own compose project and Docker-assigned port) but NOT twice at once in one worktree: both resolve to the same project and the first to finish runs `down -v` under the other. The failure then looks like `connection refused` against the code. It refuses to run (`NO TESTS EXECUTED`) when the database is unreachable. Reap a stack from a deleted worktree with `docker ps --filter name=padtest-` then `docker compose -p <project> down -v`; from inside a worktree, `make test-pg-down`.
- Refreshing an installed binary: use `scripts/install-refresh.sh`. `make install` runs `npm ci`, which rewrites `web/node_modules` and breaks any other process building from the same checkout.
- Keep Go build caches off a tmpfs `/tmp` (it is RAM-backed on many dev machines); point `GOCACHE`/`GOTMPDIR` at a disk path.

## API and MCP

- REST at `/api/v1/`. Before adding or changing an endpoint, status/error code or request/response shape, read `docs/api.md`. New endpoint notes go there, not in this file.
- MCP (`pad mcp serve` stdio, `/mcp` HTTP): before changing a tool, action, parameter, annotation, result shape or server instructions, read `docs/mcp.md`.
- Any MCP catalog, annotation, schema or `instructions.md` change moves the golden bytes. Regenerate: `PAD_UPDATE_MCP_GOLDEN=1 go test ./cmd/pad/ -run TestMCPWireGolden`, and decide from the diff whether `ToolSurfaceVersion` (`internal/mcp/version.go`) needs a bump.
- When adding a `pad` command, decide whether it belongs on the MCP surface (`docs/mcp.md` says how).
- CLI reference: `pad --help` and `pad <group> --help`. Items are referenced by issue ID (`TASK-5`); collection names accept singular forms.

## Data model facts that bite

- `terminal_options` close an item; `abandoned_options` (subset of terminal, else 400) close it without delivering. Completed-work views count terminal minus abandoned. A field declaring none falls back to `models.NegativeTerminals`, so a custom abandon word counts as shipped until declared.
- `item move`/`item copy`: values with no home in the target schema are dropped and reported (`dropped_fields`). Cross-workspace copy drops relation fields and `github_pr` (`referent_not_portable`). `implementation_notes`, `decision_log`, `github_pr`, `convention` are system metadata, never settable via `--field`. A move that changes open/done/abandoned state is refused unless the done field is named.
- Playbooks live in the `playbooks` collection with `invocation_slug` and `arguments`. `pad bootstrap` returns metadata only, no bodies. Templates and the `onboard` playbook seed from `internal/collections/` and `SeedCollectionsFromTemplate`; an empty template name intentionally skips the seed.
- Auth: with no users the instance is open until `pad auth setup`; afterwards every API call needs auth. Credentials in `~/.pad/credentials.json` (0600). Local lockout recovery (`pad auth reset-password`) uses the loopback-only `/auth/local-reset` endpoint and is disabled in cloud mode.
- Workspace invitations are always PENDING until the invitee accepts; there is no direct add.

## Common tasks

- New endpoint: handler in `internal/server/handlers_*.go`; route in `server.go` setupRouter(); store method; CLI client method in `internal/cli/client.go`; TS type in `web/src/lib/types/index.ts`; API method in `web/src/lib/api/client.ts`; `make install`.
- New CLI command: constructor in the matching `cmd/pad/cmd_<resource>.go` (create one if none fits); wire in `cmd/pad/groups.go`; `make install`.
- Schedules (recurring and deadline reminders) ride `pad item remind`: `--start`/`--every` create one, and `show`, `done`, `skip`, `snooze`, `move`, `agenda`, `edit`, `rm` are its subcommands. There is no `pad cal` and no top-level `pad remind`.
- Schema change: add a file under `internal/store/migrations/`, update `internal/models/` and store methods; migrations run on server start.

## Real-time collaboration (Yjs / Tiptap)

- WebSocket `/api/v1/collab/{itemID}`. The server is a dumb relay: it persists raw Yjs updates without parsing them (`internal/store/yjs_updates.go`). Client provider: `web/src/lib/collab/wsProvider.svelte.ts`.
- Bump `@tiptap/core`, `@tiptap/extension-collaboration` and `@tiptap/y-tiptap` together, exact-pinned to the same version (no `^`). Mixed versions silently change the persisted Y.Doc shape and peers diverge.
- A change to the ProseMirror node spec must bump `web/src/lib/collab/schemaVersion.ts::SCHEMA_VERSION` and `internal/collab/manager.go::DefaultSchemaVersion` together. On mismatch the server returns 400 and prunes the item's op-log. Unflushed edits are set aside (`item_yjs_updates_set_aside`, `content_state: superseded_set_aside`), not restored.
- The schema version is FROZEN: `internal/collab/schema_version_guard_test.go` fails on any bump (and when the constants disagree); lifting the freeze is a deliberate, reviewed change.
- UI/CSS/behaviour changes that do not alter the persisted document shape do not bump the version.

## Related repos and naming

- pad-web (getpad.dev, docs, blog) is a separate PRIVATE repo at `../pad-web`; only pad is public, so public good-first-issues point at pad code. Check "absent in pad-web" against a fresh origin/main.
- Text naming the legal entity (publisher, legal pages, licenses, store and package metadata) reads "Perpetual Software LLC". The brand "Pad" and org slug `PerpetualSoftware` stay.

## Surfaces and boundaries

- Plan every surface (CLI, API, web, TS type); a data feed for one web view gets API + TS types + web client only, no CLI or MCP action.
- Pad is a substrate: product behavior never depends on convention compliance, and no workspace's tool names or rituals compile into product code.
- Verify a server-produced value (enum, `kind`, error `code`) at the server source (store writer, migration CHECK, DTO, docs/api.md), cited file:line. Fixtures are not a source.

## Gates

- Pre-push on the exact head: gofmt, `make lint`, the repo guard tests, `go test -timeout=45m` on the touched packages (the 10m default is too short for internal/server), `-race` on the touched packages (internal/server, internal/store: `-run` the tests exercising the diff, report the count; CI races only main and release-*), and if `web/` changed, `npm run check` plus vitest on the changed specs. CI's Go and Web jobs run the full suites on every push, and its Go (PostgreSQL) job runs the packages listed in `scripts/ci-pg-packages.txt` on a PR (the whole suite on main and release-*), so a local full `./...` or `make test-pg` is for debugging a CI red. `make check` already runs `go test ./...`: never chain it with `make test`. Exit status is the verdict.
- Never mark ready or merge someone else's draft PR; leave it to its author.
- External-contributor PRs: read the full diff before approving any Actions run (workflows, action pins against upstream, `pull_request_target`, permissions, secrets). Never enable auto-approval for outside contributors; the manual gate is the security boundary.

## Store and server traps

- A schema change needs `internal/store/migrations/NNN` AND `internal/store/pgmigrations/MMM`, numbered independently; SQLite alone hides the miss.
- No apostrophe in a `--` comment in store SQL (breaks Postgres rebinding). Take a row's strongest lock first (FOR SHARE then FOR NO KEY UPDATE deadlocks). `testServer` is SQLite even under `make test-pg`.
- No response may be reachable only for rows that exist: byte-identical 404s, gates before lookups. Workspace-global MCP reads filter with `TokenAllowedWorkspaceSet` (the OAuth allow-list covers only `{slug}` routes).
- A new refusal message must map to the same code on MCP stdio (`internal/mcp/errors.go`) as on HTTP.
- Change the pad-cloud `internal` subnet and `PAD_TRUSTED_PROXIES` together, or rate limits and audit IPs collapse onto the router.

## Tests

- Never `http.DefaultClient`, `http.Get` or a nil-Transport client against httptest servers (`Close()` poisons the shared transport); use `srv.Client()` or `isolatedTestClient()`.
- One Playwright run per worktree. Never aim it at :7777; boot a throwaway instance (own HOME, `PAD_DATA_DIR`, port).

## Svelte 5 traps

- An `$effect` that reads and writes the same `$state` self-invalidates; prod silently wedges all effects. Edge trackers are plain `let`; wrap effect-called loaders in `untrack`.

## Deps and release

- Any `go.mod` change moves `nix/package.nix` vendorHash; commit the `got:` hash from the PR's Nix job. Never `npm audit fix`; bump the direct dep or add an `overrides` floor.
- Cut releases with a bare `git tag vX.Y.Z`, never annotated.

## Pointers for removed detail
- Self-host config, including the email env vars (`PAD_MAILEROO_API_KEY`, `PAD_EMAIL_FROM`, `PAD_EMAIL_FROM_NAME`): docs/deployment.md.
- When collab content is refused on save, recover with `pad item set-aside` (docs/api.md). Never force-overwrite, because that loses data.
