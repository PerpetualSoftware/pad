<p align="center">
  <h1 align="center">Pad</h1>
  <p align="center">Project management for developers and AI agents.</p>
  <p align="center">
    <a href="https://github.com/xarmian/pad/actions/workflows/ci.yml"><img src="https://github.com/xarmian/pad/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
    <a href="https://github.com/xarmian/pad/releases"><img src="https://img.shields.io/github/v/release/xarmian/pad" alt="Release"></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache%202.0-blue" alt="License"></a>
  </p>
</p>

---

One binary. Local-first. No accounts. Pad gives you a web UI, a CLI, and an AI agent skill — all backed by SQLite, all in a single `~18MB` binary.

**For you:** A clean web interface to organize tasks, ideas, phases, docs, and custom collections — with a rich editor, board views, wiki-links, and full-text search.

**For your AI agent:** A `/pad` skill that lets your AI coding tool manage your project through natural language — create tasks, check status, follow conventions, and plan phases. Works with Claude Code, Cursor, Windsurf, Codex, GitHub Copilot, Amazon Q, and JetBrains Junie.

<!-- TODO: Add screenshot/GIF of the web UI here -->

## Quick Start

```bash
# Install via Homebrew
brew install xarmian/tap/pad

# Or build from source
git clone https://github.com/xarmian/pad
cd pad && make build
cp pad /usr/local/bin/

# Or run with Docker
docker run -p 7777:7777 -v pad-data:/data ghcr.io/xarmian/pad

# Initialize a workspace in your project
cd ~/projects/myapp
pad init "My App"

# Install the /pad skill for your AI tools
pad install

# Create your first task
pad create task "Set up CI pipeline" --priority high

# Open the web UI
pad open
```

The server auto-starts on first use. No configuration needed.

## Features

### Collections & Items

Pad organizes work into **collections** — typed containers with structured fields and optional rich content.

**Default collections:**

| Collection | Purpose |
|---|---|
| **Tasks** | Track work items with status, priority, assignee, effort, due date |
| **Ideas** | Capture feature ideas with impact and category |
| **Phases** | Plan and track project milestones with progress |
| **Docs** | Documentation, decisions, reference material |
| **Conventions** | Project rules that guide agent behavior |
| **Playbooks** | Multi-step workflows for agents to follow |

Create your own collections with custom fields:

```bash
pad collections create "Bug Reports" --fields "severity:select:low,medium,high,critical; browser:text; reproducible:checkbox"
```

### Web UI

A dark-themed web app at `http://localhost:7777`, embedded in the binary:

- **Board & list views** — drag-and-drop between status columns, group and sort by any field
- **Rich editor** — Tiptap-based with markdown support, formatting toolbar, and auto-save
- **Wiki-links** — `[[Title]]` links between items, rendered as clickable references
- **Full-text search** — instant search across all items and content
- **Dashboard** — collection summaries, phase progress, activity feed, and suggested next actions
- **Real-time updates** — changes from the CLI or agents appear instantly via SSE

### CLI

```bash
# Create items (accepts singular: task, idea, phase, doc)
pad create task "Fix OAuth redirect" --priority high
pad create idea "Real-time collaboration" --category infrastructure
pad create phase "API Redesign" --status active

# List and filter
pad list tasks                          # Open + in-progress tasks
pad list tasks --status done            # Completed tasks
pad list --all                          # Everything across all collections

# View and update
pad show fix-oauth                      # Item detail (by slug)
pad update fix-oauth --status done      # Update fields
pad edit fix-oauth                      # Open in $EDITOR

# Search and navigate
pad search "authentication"             # Full-text search with snippets

# Project intelligence
pad status                              # Dashboard: phases, attention items, suggestions
pad next                                # Recommended next task

# Collections
pad collections                         # List collections with item counts
pad collections create "Name" --fields "key:type[:options]; ..."
```

The CLI auto-detects your workspace by walking up the directory tree for a `.pad.toml` file.

### Agent Integration

Pad ships a `/pad` skill that works with any AI coding tool. Install it and your agent becomes a project partner:

```bash
pad install              # Auto-detect your tools and install
pad install claude       # Claude Code
pad install cursor       # Cursor (also covers Codex & Windsurf)
pad install copilot      # GitHub Copilot
pad install amazon-q     # Amazon Q
pad install junie        # JetBrains Junie
```

Then just talk to your project:

```
> /pad what should I work on next?
> /pad I finished the OAuth fix
> /pad let's brainstorm about the API redesign
> /pad create a task to add rate limiting
```

The agent reads your conventions and playbooks to follow project-specific rules. All agent actions are attributed in the activity feed, so you always know what the AI changed.

### Conventions & Playbooks

Teach your agents how your project works:

- **Conventions** — trigger-based rules like "run tests before marking a task done" or "use conventional commits"
- **Playbooks** — multi-step workflows like "when implementing a feature: read the spec, create a branch, write tests first, then implement"

```bash
pad create convention "Run tests before completing tasks" \
  --field trigger=on-task-complete \
  --field scope=all \
  --field priority=must
```

Agents load relevant conventions automatically based on what they're doing.

## Architecture

```
┌──────────────────────────────────────────────┐
│              pad (single binary)              │
│                                              │
│  ┌──────────┐  ┌──────────┐  ┌────────────┐ │
│  │   CLI    │  │  REST    │  │  Embedded  │ │
│  │ (Cobra)  │  │  API     │  │  Web UI    │ │
│  └────┬─────┘  └────┬─────┘  │ (SvelteKit)│ │
│       │    HTTP      │        └────────────┘ │
│       └──────────────┤                       │
│                ┌─────▼─────┐                 │
│                │  SQLite   │                 │
│                │  + FTS5   │                 │
│                └───────────┘                 │
└──────────────────────────────────────────────┘
```

- **Go backend** — chi router, SQLite via [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) (pure Go, no CGO), FTS5 full-text search, SSE for real-time updates
- **SvelteKit frontend** — Svelte 5, Tiptap editor, svelte-dnd-action, adapter-static, embedded via `go:embed`
- **Single binary** — serves the API + web UI, runs on macOS, Linux, and Windows
- **Workspace-per-project** — each project gets its own workspace via `pad init`, linked by a `.pad.toml` file

### Data

All data lives in `~/.pad/`:

```
~/.pad/
├── pad.db        # SQLite database (all items, versions, activity)
├── config.toml   # Optional config (host, port)
└── pad.pid       # Server PID file
```

Per-project workspace links:

```
~/projects/myapp/.pad.toml   # workspace = "my-app"
```

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full guide. Quick version:

```bash
# Prerequisites: Go 1.25+, Node.js 22+, Make

make build      # Build web UI + Go binary
make test       # Run Go tests
make dev-web    # SvelteKit dev server with hot reload
make install    # Build, install to ~/.local/bin, restart server
```

## Security

See [SECURITY.md](SECURITY.md) for reporting vulnerabilities.

## License

[Apache License 2.0](LICENSE) — © 2026 [Perpetual Software LLC](https://perpetualsoftware.org)
