Pad is a project tracker for developers and AI agents — issues (TASK, BUG), plans (PLAN), ideas (IDEA), docs (DOC), conventions, comments, and dependencies. Use this server when a user mentions:

- Issue refs like `TASK-5`, `BUG-12`, `PLAN-3`, `IDEA-8` — they are stable, human-readable IDs and the canonical way to address items.
- Tasks / issues / items / plans / progress / "what's on my plate" / "what to work on next" / standup / changelog / retrospective.
- Project conventions, decision records, or "how should this team do X."

If the user is asking general code questions with no project-management thread, you don't need this server.

## Tool surface (v0.4)

Eight resource × action tools, plus `pad_set_workspace` (which takes a `workspace` slug only — no action enum). Nine tools total.

- `pad_item` — Items: create / update / delete / get / list / move / link / unlink / deps / star / unstar / starred / comment / list-comments / bulk-update / note / decide.
- `pad_workspace` — Workspaces: list / members / invite / storage / audit-log.
- `pad_collection` — Collections: list / create / update / delete.
- `pad_project` — Project intelligence: dashboard / next / standup / changelog.
- `pad_role` — Agent roles: list / create / update / delete.
- `pad_search` — Full-text search across items: query.
- `pad_playbook` — Invokable procedures: list / get / run. Use `run` to bind args against a playbook's declared spec and get the rendered body back; side-effect-free.
- `pad_meta` — Server introspection: server-info / version / tool-surface / bootstrap. The `bootstrap` action returns one-shot workspace context (user + collections + always-on conventions + roles + playbook metadata + dashboard + recent activity).
- `pad_set_workspace` — Load workspace context; response embeds the bootstrap blob so you load context in one call. On a single-user local server it also pins the workspace as the session default for subsequent calls; a multi-user/remote server does **not** persist it — pass `workspace` explicitly on each call. Takes `workspace: <slug>` only (no `action`).

For the eight resource × action tools, always pass `action` as a top-level field. Per-action required parameters are documented in each tool's description.

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

1. Explicit `workspace` argument on the call (always wins).
2. On a single-user local server only: the session default set via `pad_set_workspace`.
3. On a single-user local server only: the CWD-linked workspace from `.pad.toml`.

A multi-user/remote server does **not** persist a session default — pass `workspace` explicitly on every call. If none resolves, the action returns a structured `no_workspace` error with `available_workspaces`.

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

## Adding a workspace to this connection

If the user references a workspace this connection can't see (you'll get a 403 from workspace tools, or the workspace won't appear in `pad_workspace.list`), tell the user you can't see that workspace with your current permissions, then walk them through how to grant access: open Pad in their browser → switch to that workspace → avatar menu → "Connect project..." A 6-digit claim code will appear. Have them paste it back in chat, then call `pad_workspace.claim` with `{workspace: "<slug>", code: "<6 digits>"}`. The workspace joins this connection's allow-list and stays until the user revokes it via `/console/connected-apps`. No re-auth required.

For brand-new workspaces, `pad_workspace.create` with `{name: "<name>"}` (and optional `template`) creates the workspace AND auto-adds it to this connection's allow-list in one call — no claim code needed. Only works when the user granted "may create workspaces" at consent time; if that scope was declined the create call still succeeds but the workspace doesn't auto-join — direct the user to the claim flow above to bring it in.

## New workspace: offer to set it up

The bootstrap blob (from `pad_meta.action: bootstrap`, `pad_set_workspace`, or the `pad://workspace/{ws}/bootstrap` resource) carries `needs_onboarding: bool` — true when the workspace has zero user-created items (template seeds don't count). When it's true, **lead with an active offer before anything else**: *"This workspace is brand new and isn't set up yet. Want me to set it up? I'll ask a few quick questions and adapt it to your project."*

This is an **offer, not an auto-run** — wait for the user to say yes before running onboarding. If they accept, run the `onboard` playbook (use the `pad_onboard` prompt, or load the body via `pad_playbook` `action: get`, `ref: onboard`). If they decline, or already declined earlier in the session, respect that and skip the offer. The flag flips to false the moment any item exists, so it won't nag past first setup.

## Multi-step workflows

Four prompts ship with the server: `pad_plan`, `pad_ideate`, `pad_retro`, `pad_onboard`. Use them when the user wants help planning, brainstorming, retrospecting, or onboarding into a workspace — they encode the multi-step Pad-aware playbook for each.
