# Changelog

All notable changes to Pad will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/), and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- Collections with typed JSON schemas (select, text, date, number, checkbox, url, relation)
- Default collections: Tasks, Ideas, Phases, Docs, Conventions, Playbooks
- Workspace templates: startup, scrum, product
- Rich content editor (Tiptap) with markdown support
- Wiki-links (`[[Title]]`) across items with bidirectional resolution
- Full-text search across all items
- Board view with drag-and-drop (svelte-dnd-action)
- List view with grouping and sorting
- Real-time updates via Server-Sent Events (SSE)
- Item reference numbers (TASK-1, DOC-5, etc.)
- Convention and playbook system for agent behavior configuration
- Convention and playbook libraries with pre-built entries
- Relation fields linking items across collections
- Phase progress tracking with linked tasks
- Item comments
- Item version history with diffs
- Dashboard with collection summaries, phase progress, and activity feed
- CLI with full CRUD: create, list, show, update, delete, search
- `pad status` for project dashboard, `pad next` for recommended next task
- `pad edit` to open items in $EDITOR
- `pad init` with workspace templates
- Shell completions (bash, zsh, fish, powershell)
- Claude Code `/pad` skill for natural-language project management
- Embedded SvelteKit web UI served from the Go binary
- Single binary distribution — no external dependencies
- SQLite storage with automatic migrations
- Apache 2.0 license
- CI pipeline (GitHub Actions)
- GoReleaser configuration for cross-platform releases
- Homebrew tap support
