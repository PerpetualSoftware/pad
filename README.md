<p align="center">
  <h1 align="center">Pad</h1>
  <p align="center"><strong>Project management for developers and AI agents.</strong></p>
  <p align="center">
    <a href="https://github.com/xarmian/pad/actions/workflows/ci.yml"><img src="https://github.com/xarmian/pad/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
    <a href="https://github.com/xarmian/pad/releases"><img src="https://img.shields.io/github/v/release/xarmian/pad" alt="Release"></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache%202.0-blue" alt="License"></a>
  </p>
</p>

---

> One binary. Local-first. No accounts. Pad gives you a CLI, a web UI, and an AI agent skill — all backed by SQLite, all running on your machine. Your project data never leaves your laptop.

<!-- Screenshots: dashboard, board view, CLI output -->
<!-- TODO: Replace with actual screenshots -->
<!--
<p align="center">
  <img src="docs/screenshots/dashboard.png" width="800" alt="Pad dashboard showing collection overview, active phases, and activity feed" />
</p>
-->

## Quick Start

```bash
brew install xarmian/tap/pad
cd your-project
pad auth configure
pad workspace init
pad server open
```

For a local install, choose `Local` in `pad auth configure`. Pad will remember that this client manages a local server, auto-start it when needed, and open the web UI at `localhost:7777`.

## Why Pad?

Tools like Linear, Jira, and Notion are built for teams on the cloud. Pad is built for **developers on their machine** — and for the AI agents working alongside them.

| | Pad | Linear / Jira | Notion |
|---|---|---|---|
| **Setup** | `pad auth configure` + `pad workspace init` | Create account, invite team, configure | Create account, pick template |
| **AI agents** | Native `/pad` skill for 7+ tools | Third-party integrations | Third-party integrations |
| **Data** | Local SQLite, you own it | Their cloud | Their cloud |
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
```

**Web UI that stays out of your way.** A clean, dark-themed interface at `localhost:7777` with:

- **Board, list, and table views** — drag-and-drop between status columns
- **Keyboard navigation** — `j`/`k` to move, `Enter` to open, `Esc` to go back, `Cmd+K` to search
- **Rich text editor** — Tiptap-based with markdown, formatting toolbar, and auto-save
- **Wiki-links** — type `[[Title]]` to link between items
- **Real-time updates** — agent creates a task in the terminal, it appears in the browser instantly (via SSE)
- **Dashboard** — collection overview, active work, phase tracking, activity feed

<!-- TODO: Screenshot of board view with drag-and-drop -->

### For AI Agents

**Your agent becomes a project partner.** Install the `/pad` skill once, and your AI coding tool can read, create, and update project items through natural language.

```bash
pad agent install        # Auto-detects your tools and installs the skill
```

Works with **Claude Code**, **Cursor**, **Windsurf**, **Codex**, **GitHub Copilot**, **Amazon Q**, and **JetBrains Junie**.

Then just talk to your project:

```
> /pad what should I work on next?
> /pad I finished the OAuth fix
> /pad create a task to add rate limiting
> /pad let's brainstorm about the API redesign
```

**Conventions and playbooks** teach agents how your project works:

- **Conventions** — trigger-based rules like "run tests before marking a task done" or "use conventional commits"
- **Playbooks** — multi-step workflows like "when implementing a feature: read the spec, create a branch, write tests first, then implement"

```bash
pad item create convention "Run tests before completing tasks" \
  --field trigger=on-task-complete \
  --field scope=all \
  --field priority=must
```

Agents load relevant conventions automatically. All agent actions are attributed in the activity feed, so you always know what the AI changed.

**Onboard agents to a new codebase:**

```bash
pad workspace onboard    # Analyzes project structure and suggests conventions
```

### Collections & Custom Fields

Pad organizes work into **collections** — typed containers with structured fields.

**Built-in collections:**

| Collection | Purpose |
|---|---|
| **Tasks** | Work items with status, priority, assignee, effort, due date |
| **Ideas** | Feature ideas with impact and category |
| **Phases** | Project milestones with progress tracking |
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
brew install xarmian/tap/pad
```

### Build from Source

```bash
git clone https://github.com/xarmian/pad
cd pad
make build
cp pad ~/.local/bin/   # or /usr/local/bin/
```

Requires Go 1.25+ and Node.js 22+.

The `go install github.com/xarmian/pad/cmd/pad@latest` path is not supported for the full Pad binary, because the web UI must be built and embedded during the source build.

### Docker

```bash
docker run -p 127.0.0.1:7777:7777 -v pad-data:/data ghcr.io/xarmian/pad
```

This publishes Pad to `localhost:7777` on the host machine, which is the recommended default for local use.

To expose Pad beyond localhost intentionally, publish the port more broadly:

```bash
docker run -p 7777:7777 -v pad-data:/data ghcr.io/xarmian/pad
```

Use broader publishing only when you intend to make Pad reachable from other machines or interfaces.

### Binary Download

Pre-built binaries for macOS, Linux, and Windows are available on the [releases page](https://github.com/xarmian/pad/releases).

## Getting Started

### 1. Configure this Pad client

```bash
pad auth configure
```

For most local installs, choose `Local`. If you're connecting to another Pad server, choose `Remote` or `Docker` and enter its base URL.

### 2. Initialize a workspace

```bash
cd ~/projects/myapp
pad workspace init "My App"
```

This creates a `.pad.toml` file linking your project directory to a Pad workspace with default collections. Choose a template to start with pre-configured collections:

```bash
pad workspace init "My App" --template scrum     # Scrum-style with sprints
pad workspace init "My App" --template product   # Product management focused
```

### 3. Install the AI skill

```bash
pad agent install            # Auto-detect and install for all found tools
pad agent install claude     # Or install for a specific tool
pad agent install cursor
pad agent install copilot
```

### 4. Start working

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

### 5. Teach your agents the rules

```bash
pad workspace onboard        # Auto-analyze project and suggest conventions
# Or browse the convention library
pad library list --type conventions  # Pre-built conventions you can adopt
pad library list --type playbooks    # Pre-built multi-step workflows
```

## CLI Reference

```
pad auth configure                    Configure how this client connects to Pad
pad auth setup                        Initialize the first admin account
pad auth login                        Sign in
pad auth whoami                       Show current user

pad server start                      Start the Pad API server
pad server stop                       Stop the Pad server
pad server open                       Open web UI in browser

pad workspace init [name]             Initialize workspace in current directory
pad workspace link <workspace>        Link current directory to an existing workspace
pad workspace list                    List all workspaces
pad workspace switch <workspace>      Switch active workspace
pad workspace onboard                 Analyze project and suggest conventions
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

pad item create <coll> "title"        Create item (task, idea, phase, doc, ...)
pad item list [collection]            List items (filters: --status, --priority, --all)
pad item show <ref>                   Show item detail
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

pad github link [item-ref]   Link current branch's PR to item
pad github status [item-ref] Show PR status for linked items
pad github unlink <item-ref> Remove PR link from item

pad webhook list             List workspace webhooks
pad webhook create <url>     Create webhook
```

All commands accept `--format json` for machine-readable output and `--workspace` to target a specific workspace.

### Authentication

Pad runs without authentication by default for frictionless local use. On a fresh instance, run `pad auth setup` on the server host to create the first admin account:

```bash
pad auth setup         # Initialize the first admin account
pad auth login         # Sign in
pad auth whoami        # Show current user
pad auth logout        # Sign out
```

Once a user exists, all API requests and web UI access require authentication. Credentials are stored in `~/.pad/credentials.json`. Multiple users can be invited to workspaces with role-based access control (`owner`, `editor`, `viewer`).

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

All data lives in `~/.pad/pad.db`. Your data. Your machine. No telemetry, no cloud, no accounts.

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

## License

[Apache License 2.0](LICENSE)
