Pad is a project tracker for developers and AI agents — issues (TASK, BUG), plans (PLAN), ideas (IDEA), docs (DOC), conventions, comments, and dependencies. Use this server when a user mentions:

- Issue refs like `TASK-5`, `BUG-12`, `PLAN-3`, `IDEA-8` — they are stable, human-readable IDs and the canonical way to address items.
- Tasks / issues / items / plans / progress / "what's on my plate" / "what to work on next" / standup / changelog / retrospective.
- Project conventions, decision records, or "how should this team do X."

If the user is asking general code questions with no project-management thread, you don't need this server.

## Tool surface (v0.3)

Nine tools, each with an `action` enum:

- `pad_item` — Items: create / update / delete / get / list / move / link / unlink / deps / star / unstar / starred / comment / list-comments / bulk-update / note / decide.
- `pad_workspace` — Workspaces: list / members / invite / storage / audit-log.
- `pad_collection` — Collections: list / create.
- `pad_project` — Project intelligence: dashboard / next / standup / changelog.
- `pad_role` — Agent roles: list / create / delete.
- `pad_search` — Full-text search across items: query.
- `pad_playbook` — Invokable procedures: list / get / run. Use `run` to bind args against a playbook's declared spec and get the rendered body back; side-effect-free.
- `pad_meta` — Server introspection: server-info / version / tool-surface / bootstrap. The `bootstrap` action returns one-shot workspace context (user + collections + always-on conventions + roles + playbook metadata + dashboard + recent activity).
- `pad_set_workspace` — Pin a session-default workspace for subsequent calls. Response embeds the bootstrap blob so you can pin + load context in one call.

Always pass `action` as a top-level field. Per-action required parameters are documented in each tool's description.

## Resources are cheaper than tool calls

Read these directly when you need workspace state:

- `pad://workspace/{ws}/dashboard` — computed project overview (active items, plans, attention, suggested next).
- `pad://workspace/{ws}/collections` — collection types + schemas.
- `pad://workspace/{ws}/items` — list of all items (use `pad_item.action: list` for filtering).
- `pad://workspace/{ws}/items/{ref}` — single item rendered as markdown.
- `pad://workspace/{ws}/bootstrap` — one-shot workspace context (same payload as `pad_meta.action: bootstrap` and `pad_set_workspace`'s embedded response).
- `pad://_meta/version` — server version + stability tiers.

Resources support host-side prefetch — if the host can fetch them once at session start, you don't pay per turn.

## Workspace context

Every action that operates within a workspace accepts an optional `workspace` parameter. Resolution order:

1. Explicit `workspace` argument on the call (highest priority).
2. Session default set via `pad_set_workspace`.
3. CWD-linked workspace from `.pad.toml` (when running locally).

If none resolves, the action returns a structured `no_workspace` error with `available_workspaces`. Pass `workspace` explicitly when working across multiple workspaces in one session.

## Always use issue refs

Items have refs like `TASK-5`, `IDEA-12`, `PLAN-3`. Use those — never slugs. Refs are short, stable, human-readable, and what appears in audit trails and PR titles.

## Update flow: read first, then patch

For `pad_item.action: update`, the server merges your patch with the item's current state. Pass only the fields you want to change. When changing `status`, ALWAYS include a `comment` explaining why — it builds the audit trail that helps the team understand history.

## Project conventions

Workspaces can declare conventions (e.g. "run `make test` before PR", "use conventional commit format"). Before performing meaningful work, you may want to read active conventions:

```
pad_item.action: list, collection: "conventions", status: "active"
```

Filter by trigger (`always`, `on-implement`, `on-task-complete`, etc.) when relevant.

## Multi-step workflows

Four prompts ship with the server: `pad_plan`, `pad_ideate`, `pad_retro`, `pad_onboard`. Use them when the user wants help planning, brainstorming, retrospecting, or onboarding into a workspace — they encode the multi-step Pad-aware playbook for each.
