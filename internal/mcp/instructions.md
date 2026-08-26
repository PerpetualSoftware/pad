Pad is a project tracker for developers and AI agents — issues (TASK, BUG), plans (PLAN), ideas (IDEA), docs (DOC), conventions, comments, and dependencies. Use this server when a user mentions:

- Issue refs like `TASK-5`, `BUG-12`, `PLAN-3`, `IDEA-8` — they are stable, human-readable IDs and the canonical way to address items.
- Tasks / issues / items / plans / progress / "what's on my plate" / "what to work on next" / standup / changelog / retrospective.
- Project conventions, decision records, or "how should this team do X."

If the user is asking general code questions with no project-management thread, you don't need this server.

## Tool surface (v0.26)

Ten resource × action tools, plus `pad_set_workspace` (which takes a `workspace` slug only — no action enum). Eleven tools total.

Inputs are validated strictly: an undeclared top-level key is rejected with a structured `validation_failed` naming it, never accepted and silently dropped.

- `pad_item` — Items: create / update / delete / get / list / move / restore / link / unlink / deps / star / unstar / starred / comment / list-comments / backlinks / bulk-update / note / decide / export / import / history. On create/update, field values may be passed as a `fields` OBJECT (the same shape reads return, e.g. `{"status":"done","effort":"l"}`) — it merges into the same path as the dedicated params and `field: ["key=value"]`; the same key in two places with CONFLICTING values is refused, not silently resolved. `list` accepts `unparented: true` to keep items with no parent or implements relationship (mutually exclusive with `parent`). `list` results are SUMMARY-shaped by default on both transports — no content bodies; pass `full: true` for complete bodies (token-expensive), or prefer `get` for a single item's body. `update` field writes are a server-side field-level merge (only the keys you set change); pass `expected_updated_at` for optimistic concurrency (a stale value fails with a structured 409 `update_conflict`). `move` changes an item's COLLECTION within its workspace: system metadata (implementation notes, decision log, linked PR) survives it, and any field value the target schema has no home for is dropped AND reported in the move's activity entry — check there rather than assuming a move is lossless. Three system keys — `implementation_notes`, `decision_log`, `convention` — cannot be set through `field` on update or move; the call is refused with `validation_failed`, naming the key and the write path that does maintain it (`note`, `decide`, and library activation respectively). `github_pr` is the exception on UPDATE only — a move or copy still refuses it — because `pad github link` needs a local git checkout you don't have and an update would be your only way in. Be aware that it does not currently work either: a `field` value arrives as a string, so the PR data is stored double-encoded and no link appears (BUG-2696). Treat linking a PR as something to hand to a human for now, rather than a call to retry. On `create` none of them are blocked, since that door is shared with Pad's own writers — but don't hand-write `implementation_notes` / `decision_log` there either. Doing so does not merely fail to help: it stores something Pad cannot read back, which hides the existing entries on every surface and makes `note` / `decide` refuse on that item until it is repaired. `history` returns read-only item version metadata (newest-first), bounded to the NEWEST 50 versions by default (max 300 — pass `limit` to change the window); pass `full: true` to include each version's resolved content body (token-expensive). There is no `offset`: versions are stored as reverse patches, so only a newest-end window is cheap to reconstruct. To UNASSIGN an item, pass `clear_assigned_user: true` (or `clear_agent_role: true`) — the canonical form, works on both transports. To DETACH an item from its parent, pass `clear_parent: true` — same canonical shape, works on both transports. Setting and clearing the same field in one call is refused, not silently resolved, so don't pair `clear_assigned_user`/`clear_agent_role`/`clear_parent` with `assign`/`role`/`parent` respectively. An empty `assign` / `role` / `parent` does NOT clear: those name a person, a slug, or a ref, so an empty value reads as "not provided", exactly like every other optional string here. (Two older forms still work and are not deprecated: `field: ["assigned_user_id="]` on either transport, and a direct `assigned_user_id: ""` param over remote `/mcp` only — prefer the boolean, which is the only one this schema advertises.)
- `pad_workspace` — Workspaces: list / members / invite / storage / audit-log / create / claim / deleted / restore.
- `pad_collection` — Collections: list / create / update / delete.
- `pad_project` — Project intelligence: dashboard / next / ready / stale / standup / changelog / report / activity. Use `ready` for the actionable backlog and `stale` for items needing attention; `activity` to catch up on what other agents/users changed since you last worked (non-streaming feed with item refs + change details).
- `pad_role` — Agent roles: list / create / update / delete.
- `pad_search` — Full-text search across items: query.
- `pad_playbook` — Invokable procedures: list / get / run. Use `run` to bind args against a playbook's declared spec and get the rendered body back; side-effect-free. `run` refuses a playbook whose status isn't `active` (a draft still being authored) with a `playbook_not_active` error — pass `allow_draft: true` to override. Both `run` and `get` echo the playbook's `status`.
- `pad_library` — Convention + playbook library (the global catalog of pre-built entries workspaces activate): list / get / activate.
- `pad_attachment` — Read-only attachment metadata: list / show. `list` enumerates a workspace's attachments (filter by item / category / collection / attached / unattached); `show` returns one attachment's MIME, size, filename, and ETag via a HEAD request. Uploading and general file downloads stay CLI-only; bounded image bytes are available through the attachment resource below.
- `pad_meta` — Server introspection: server-info / version / tool-surface / bootstrap. The `bootstrap` action returns one-shot workspace context (user + collections + always-on conventions + a metadata-only `convention_index` of every active convention + roles + playbook metadata + dashboard + recent activity).
- `pad_set_workspace` — Load workspace context; response embeds the bootstrap blob so you load context in one call. On a single-user local server it also pins the workspace as the session default for subsequent calls; a multi-user/remote server does **not** persist it — pass `workspace` explicitly on each call. Takes `workspace: <slug>` only (no `action`).

For the ten resource × action tools, always pass `action` as a top-level field. Per-action required parameters are documented in each tool's description.

## Resources are cheaper than tool calls

Read these directly when you need workspace state:

- `pad://workspace/{ws}/dashboard` — computed project overview (active items, plans, attention, suggested next).
- `pad://workspace/{ws}/collections` — collection types + schemas.
- `pad://workspace/{ws}/items` — list of all items (use `pad_item.action: list` for filtering).
- `pad://workspace/{ws}/items/{ref}` — single item rendered as markdown.
- `pad://workspace/{ws}/attachments/{id}` — image attachment as a bounded base64 `thumb-md` resource; rejects non-images and image bytes over 1 MiB (pre-base64).
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

## One error code you must not retry

Errors come back as a structured envelope with a `code`. Most failures are worth a retry after you fix something, and a `server_error` may be transient. `stored_state_unreadable` is neither.

It means the ITEM'S STORED DATA cannot be decoded — your input was fine. Today it fires when `note` / `decide` would append to an `implementation_notes` / `decision_log` value that is not a list of entries. The operation is refused precisely because completing it would overwrite that value. The refusal is fully deterministic, so retrying — immediately or after a backoff — fails identically every time and changes nothing.

When you see it: tell the user which item is affected, and stop. Whether you can inspect the bad value depends on which layer is broken — if the item's whole `fields` blob fails to parse, `action: get` hands it back as a raw string; if one structured key is at fault, you won't see it at all, because the blob is normalized on this surface and a value that doesn't decode is dropped from the top-level arrays. A human can always read it with `pad item show <ref> --format json`. Do not route around the refusal by writing the field some other way; that is the write that caused it.

## Project conventions

Workspaces can declare conventions (e.g. "run `make test` before PR", "use conventional commit format"). The bootstrap blob gives you two views:

- `conventions` — full bodies of the always-on (`trigger=always`) rules. Follow these unconditionally.
- `convention_index` — METADATA ONLY (`ref`, `title`, `trigger`, `role`; no bodies) for **every** active convention, including the triggered ones whose bodies are NOT in `conventions`. This is your map of what triggered rules exist.

Before performing meaningful work with a specific trigger (e.g. `on-implement` before writing code), consult `convention_index`: if it lists entries for that trigger, pull their bodies on demand; if it lists none, skip the query.

```
pad_item.action: list, collection: "conventions", status: "active"
```

Filter by trigger (`always`, `on-implement`, `on-task-complete`, etc.) when relevant — the `convention_index` triggers tell you which filters are worth running.

`"conventions"` above is the DEFAULT collection slug and a workspace may have renamed it, in which case that literal returns nothing while `convention_index` still lists entries — the bootstrap payload resolves by declaration, not by name. When the two disagree, address the items by the `ref`s the index gave you, or find the collection with `pad_collection.action: list`. The same applies to `"playbooks"`; `pad_playbook` resolves by declaration and is unaffected.

## Adding a workspace to this connection

If the user references a workspace this connection can't see (you'll get a 403 from workspace tools, or the workspace won't appear in `pad_workspace.list`), tell the user you can't see that workspace with your current permissions, then walk them through how to grant access: open Pad in their browser → switch to that workspace → avatar menu → "Connect project..." A 6-digit claim code will appear. Have them paste it back in chat, then call `pad_workspace.claim` with `{workspace: "<slug>", code: "<6 digits>"}`. The workspace joins this connection's allow-list and stays until the user revokes it via `/console/connected-apps`. No re-auth required.

For brand-new workspaces, `pad_workspace.create` with `{name: "<name>"}` (and optional `template`) creates the workspace AND auto-adds it to this connection's allow-list in one call — no claim code needed. Only works when the connection currently carries the "may create workspaces" grant — granted at consent time, or enabled later on the connections page. If the grant is present and set to false, the create is **refused with a 403 and no workspace is made** — retrying won't help and neither will the claim flow, since there is nothing to claim. Tell the user this connection isn't permitted to create workspaces, and offer them the two ways forward: create the workspace themselves in the browser and grant access with a claim code, or re-authorize this connection with "Let this app create new workspaces" enabled (`/console/connected-apps` can also flip it on an existing connection).

## New workspace: offer to set it up

The bootstrap blob (from `pad_meta.action: bootstrap`, `pad_set_workspace`, or the `pad://workspace/{ws}/bootstrap` resource) carries `needs_onboarding: bool` — true when the workspace has zero user-created items (template seeds don't count). When it's true, **lead with an active offer before anything else**: *"This workspace is brand new and isn't set up yet. Want me to set it up? I'll ask a few quick questions and adapt it to your project."*

This is an **offer, not an auto-run** — wait for the user to say yes before running onboarding. If they accept, run the `onboard` playbook (use the `pad_onboard` prompt, or load the body via `pad_playbook` `action: get`, `ref: onboard`). If they decline, or already declined earlier in the session, respect that and skip the offer. The flag flips to false the moment any item exists, so it won't nag past first setup.

## Multi-step workflows

Four prompts ship with the server: `pad_plan`, `pad_ideate`, `pad_retro`, `pad_onboard`. Use them when the user wants help planning, brainstorming, retrospecting, or onboarding into a workspace — they encode the multi-step Pad-aware playbook for each.
