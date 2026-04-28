# Contributing to Pad

Thanks for your interest in contributing to Pad! This guide will help you get set up and familiar with how we work.

## Getting Started

### Prerequisites

- [Go 1.26+](https://go.dev/dl/)
- [Node.js 22+](https://nodejs.org/)
- Make

### Setup

```bash
git clone https://github.com/PerpetualSoftware/pad
cd pad
make build    # Build web UI + Go binary
```

This produces a `./pad` binary in the project root.

### Development

```bash
make build      # Full build: web UI + Go binary
make build-go   # Go only (skip web — faster for backend changes)
make dev-web    # SvelteKit dev server with hot reload (localhost:5173)
make test       # Run Go tests
make lint       # Run go vet
make install    # Build, install to ~/.local/bin/pad, restart server
```

**Typical workflow:**

1. Make your changes
2. `make build` to verify everything compiles
3. `make test` to run tests
4. `make install` to test the full binary locally
5. Open http://localhost:7777 to verify the web UI

### Project Structure

```
cmd/pad/main.go          — CLI entry point (Cobra commands)
internal/
  server/                — HTTP API handlers, SSE, middleware
  store/                 — SQLite CRUD, migrations, FTS
  models/                — Go types
  items/                 — Field validation
  collections/           — Default schemas, templates
  cli/                   — HTTP client, formatting
  events/                — EventBus for real-time SSE
  config/                — Workspace detection
web/src/
  routes/                — SvelteKit pages
  lib/api/client.ts      — TypeScript API client
  lib/types/index.ts     — TypeScript types
  lib/components/        — Reusable UI components
skills/pad/SKILL.md      — Claude Code agent skill
```

## Making Changes

### Branch Naming

Use descriptive branch names:

- `feat/relation-field-picker` — new features
- `fix/dashboard-progress-bar` — bug fixes
- `docs/update-api-reference` — documentation
- `refactor/store-interface` — refactoring

### Commit Messages

Write clear, concise commit messages that explain **why**, not just what:

```
Add phase relation field to Tasks collection

Tasks can now be linked to phases via a relation field,
enabling progress tracking on the phase detail page.
```

### Pull Requests

- Keep PRs focused — one feature or fix per PR
- Include a description of what changed and why
- Add tests for new backend functionality
- Verify `make build` and `make test` pass before opening

### Code Style

- **Go:** Standard `gofmt` formatting. Run `go vet ./...` to catch issues.
- **Svelte:** Follow existing component patterns. Use Svelte 5 runes (`$state`, `$derived`, `$effect`).
- **TypeScript:** Types live in `web/src/lib/types/index.ts`.

### Quality Gates

PR CI runs two security gates that block merging on regressions:

- **`npm audit --audit-level=high --omit=dev`** — fails on any HIGH or CRITICAL advisory in production frontend deps. Run from `web/` locally to catch findings before opening a PR.
- **`govulncheck ./...`** — fails on any known vulnerability in a Go package the binary actually reaches via the call graph. Pinned to a specific version in `.github/workflows/ci.yml`; bump intentionally rather than tracking `@latest`.

Both are fast enough to run locally:

```bash
cd web && npm audit --audit-level=high --omit=dev
go install golang.org/x/vuln/cmd/govulncheck@v1.2.0 && govulncheck ./...
```

## Adding Features

### New API Endpoint

1. Add handler in `internal/server/handlers_*.go`
2. Register route in `internal/server/server.go` (`setupRouter()`)
3. Add store method in `internal/store/` if needed
4. Add CLI client method in `internal/cli/client.go`
5. Add TypeScript type in `web/src/lib/types/index.ts`
6. Add API method in `web/src/lib/api/client.ts`

### New CLI Command

1. Add function in `cmd/pad/main.go`
2. Register in `rootCmd.AddCommand()`

### Database Migration

1. Add migration in `internal/store/migrations/`
2. Update models in `internal/models/`
3. Migrations run automatically on server start

## Reporting Issues

- **Bugs:** Use the bug report template — include steps to reproduce
- **Features:** Use the feature request template — describe the problem first, then your proposed solution
- **Questions:** Open a discussion or issue

## Contributor License Agreement

By submitting a pull request, you agree to the terms of our [Contributor License Agreement](.github/CLA.md). This is a lightweight CLA that preserves your rights while granting the project maintainers flexibility for future licensing decisions.

## License

Pad is licensed under [Apache 2.0](LICENSE). By contributing, your code is released under the same license.
