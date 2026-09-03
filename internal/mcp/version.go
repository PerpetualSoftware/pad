// Package mcp implements pad's Model Context Protocol server.
//
// Layered build (PLAN-942):
//   - TASK-944 (this file + server.go) — handshake skeleton.
//   - TASK-945 — cmdhelp-derived tool registry + shell-out dispatch.
//   - TASK-946 — MCP resources (items, dashboard, collections).
//   - TASK-947 — MCP prompts (planning / ideation / retro).
//   - TASK-948 — `pad mcp install <agent>` client config writer.
//   - TASK-963 — cmdhelp_version handshake metadata + pad://_meta/version.
package mcp

// ServerName is the canonical name pad's MCP server advertises in the
// initialize handshake's serverInfo.name field. Stable across versions —
// MCP clients (Claude Desktop, Cursor, Windsurf) display it verbatim,
// so changing it would break user-visible installations.
const ServerName = "pad-mcp"

// FallbackVersion populates serverInfo.version when NewServer is
// constructed without an explicit Options.Version. Production callers
// (the cobra `pad mcp serve` command) always pass pad's runtime
// fullVersion(); this fallback covers tests, embedders, and `dev`
// builds where the version string is empty.
const FallbackVersion = "0.0.0-dev"

// CmdhelpVersion is the cmdhelp CLI-help-tree stability contract this
// MCP server advertises. cmdhelp is the source of truth for individual
// CLI command schemas (args, flags, types) consumed at MCP dispatch
// time by BuildCLIArgs. Bump the major when those CLI-side schemas
// change incompatibly:
//
//   - "0.1" — initial cmdhelp surface from PLAN-942.
//
// This is independent of ToolSurfaceVersion below — cmdhelp owns the
// CLI's help-tree contract; ToolSurfaceVersion owns the MCP tool
// catalog's contract. Two contracts, two version constants.
//
// Discovery surfaces (paths into the JSON-RPC envelope):
//
//   - result.capabilities.experimental.padCmdhelp.version (handshake).
//   - pad://_meta/version resource (queryable JSON document).
const CmdhelpVersion = "0.1"

// ToolSurfaceVersion is the MCP tool catalog stability contract this
// server advertises. External agents (Claude Desktop, Cursor, ChatGPT
// connectors, future Pad Cloud remote MCP) pin against it so a future
// tool rename, action enum change, or parameter reshape doesn't
// silently break consumers. Bump the major when the catalog shape
// changes incompatibly:
//
//   - "0.1" — historical. cmdhelp-derived ~85 flat verb tools
//     (PLAN-942). Lived from PLAN-942 through TASK-980 of PLAN-969's
//     3-stage rollout; never bumped during rollout because the
//     user-visible surface was a transitional mix of v0.1 walker
//     output + the partial v0.2 catalog.
//
//   - "0.2" — historical. Hand-curated resource × action catalog
//     (PLAN-969, TASK-981). The cmdhelp leaf walker retired; tools/list
//     advertises only the catalog (~7 tools + pad_set_workspace).
//
//   - "0.3" — historical. PLAN-1377 / TASK-1380:
//
//   - pad_meta gains an action: bootstrap that returns the
//     AgentBootstrap blob (and pad_meta.Schema.Workspace flipped
//     to true so the workspace param is available to that action).
//
//   - pad_set_workspace's response shape extends from
//     {workspace, status} to {workspace, status, bootstrap?} —
//     the embedded blob lets one call hand the agent full session
//     context. Purely additive; older clients that ignore unknown
//     keys keep working.
//
//   - pad://workspace/{ws}/bootstrap resource added.
//
//   - "0.5" — historical. PLAN-1560 / TASK-1563: adds `pad_library` to
//     the catalog as the ninth resource × action tool. Three actions —
//     list / get / activate — surface the global convention + playbook
//     library (previously CLI-only) to MCP callers. Pure addition; no
//     existing tool/action/param/bootstrap shapes changed. Backwards-
//     compatible for any v0.4 consumer that doesn't enumerate the new
//     tool.
//
//   - "0.6" — historical. PLAN-1593 / TASK-1596: adds `backlinks` action
//     to `pad_item` so MCP callers can answer "what mentions X?"
//     without scanning the full content corpus. Adds `offset` to the
//     param vocabulary, extends `limit` to cover the backlinks
//     pagination. Pure addition; existing pad_item actions unchanged.
//     Backwards-compatible for v0.5 consumers that don't enumerate
//     the new action.
//
//   - "0.7" — historical. Artifact export/import (Phase 5): adds two
//     actions to `pad_item` mirroring the CLI `pad item export` /
//     `pad item import`. `export` (read-only) takes `ref` and returns
//     the portable artifact TEXT (YAML frontmatter + Markdown body) —
//     it forces the CLI's stdout sink (`-o -`) so the bytes come back
//     as the tool result rather than being written to a file the MCP
//     host can't see. `import` (mutating, not destructive — creates a
//     draft like create) takes a new `artifact` param (the full
//     artifact text) and returns {ref, slug, warnings}; because the
//     ExecDispatcher doesn't pipe stdin, it spills the artifact to a
//     temp file and dispatches `item import <tmpfile>`. Both cover
//     playbooks AND conventions (the server gates by collection). Adds
//     the `artifact` param to the vocabulary. Pure addition; existing
//     pad_item actions unchanged. Backwards-compatible for v0.6
//     consumers that don't enumerate the new actions.
//
//   - "0.21" — BUG-2608: bounds `pad_item.action=history`, which
//     was unbounded on every surface. Extends the `limit` param's
//     vocabulary to cover history (default 50, max 300 — the same pair
//     list and backlinks already use, so an agent has no third set of
//     numbers to remember) and applies it in the CATALOG action rather
//     than either dispatcher, so it lands on BOTH transports: the HTTP
//     path reads it off the input, the exec path receives it as the
//     CLI's new --limit through BuildCLIArgs.
//
//     ADDITIVE param bump on the v0.6/v0.18/v0.19 pattern: `limit`
//     already existed, no tool, action enum, or param SHAPE changed,
//     and a v0.20 consumer that sends no limit keeps working — it now
//     receives the newest 50 versions instead of all of them, which is
//     the point of the fix rather than a break in it.
//
//     The window is the NEWEST N and there is deliberately no `offset`.
//     Versions are stored as REVERSE patches, so reconstructing any
//     version means walking back from the item's current content
//     through everything newer: a newest-end window is the cheap prefix
//     of that walk, while an older window would still pay for
//     everything above it. Offering an offset would advertise a
//     pagination whose later pages cost the same as no bound at all.
//
//     Behaviour change worth stating even though the shape is stable:
//     summary mode now asks the SERVER to skip patch resolution
//     (`?summary=true`) instead of resolving every body and discarding
//     it in the dispatcher. Same result payload, minus a full chain
//     walk per call.
//
//   - "0.20" — BUG-2302: every advertised tool now carries an
//     EXPLICIT annotation block derived from readOnlyActions (the same
//     single source the tool-surface serializer uses) instead of
//     inheriting mcp-go's NewTool defaults, which stamped
//     ReadOnlyHint:false + DestructiveHint:true + OpenWorldHint:true on
//     every tool — including pure reads like pad_search and pad_project,
//     training users to click through host confirmation prompts.
//     Policy: fully-read-only tools (every action in readOnlyActions)
//     advertise ReadOnlyHint:true, DestructiveHint:false,
//     IdempotentHint:true; tools whose writes are all purely ADDITIVE
//     (additiveWriteActions — pad_workspace's invite/create/claim/
//     restore, pad_library's activate) advertise ReadOnlyHint:false +
//     DestructiveHint:false; tools with any overwrite/delete-capable
//     action (pad_item, pad_collection, pad_role) keep the
//     conservative ReadOnlyHint:false + DestructiveHint:true —
//     unchanged on the wire from the old defaults for exactly those;
//     OpenWorldHint:false everywhere (pad tools are closed-world).
//     pad_set_workspace gets its own block (write, non-destructive,
//     idempotent, closed-world). Also adds the missing `history` entry
//     to readOnlyActions — pad_item.history was documented read-only
//     since v0.14 but reported read_only:false on the tool-surface
//     descriptor and would have kept `pad_item` annotations honest but
//     the descriptor wrong. BEHAVIOR bump (v0.9/v0.16 precedent): no
//     tool names, action enums, or parameter shapes changed — the
//     advertised annotation metadata and one read_only flag did.
//
//     Also in 0.20 (BUG-2305, same window — one bump, appended entry):
//     `pad_item.list` is summary-shaped on the REMOTE HTTP transport
//     too. The exec path always projected through cli.ToItemSummaries
//     (the CLI default TASK-2000 relied on); the HTTP path forwarded
//     raw handler responses, so a bare list returned up to 50 FULL
//     content bodies. A hand-written dispatchItemList (routeTable
//     can't transform responses; the server has no projection param)
//     now applies the same projection, and a new declared `full`
//     boolean on pad_item is the discoverable opt-in for complete
//     bodies on BOTH transports (stdio forwards it as the CLI's
//     --full). Additive param + a shape fix restoring transport
//     symmetry (v0.16/v0.17 precedent).
//
//     Post-0.20, deliberately NO bump (BUG-2304): `item backlinks`,
//     `item history`, and `project report` gained HTTP transport
//     routes — they were advertised in the catalog on both transports
//     but answered "not yet implemented over HTTP transport" on the
//     remote /mcp path. No tool names, action enums, or parameter
//     shapes changed; advertised actions now behave as already
//     documented, which is a defect fix inside the existing contract,
//     not a contract change. `history` defaults to the CLI's
//     itemVersionSummary projection over HTTP too (full=true opts
//     into complete rows, mirroring stdio's --full). A catalog↔route
//     parity test (dispatch_http_parity_test.go) now fails on any
//     future advertised-but-unrouted action.
//
//   - "0.19" — BUG-2078: adds a `clear_parent` boolean to
//     `pad_item`, the canonical and DISCOVERABLE way to detach an item from
//     its parent. Additive param bump, same grounds as v0.18 — no existing
//     tool, action, or param changed shape.
//
//     What this closes: the server has supported clearing a parent since
//     BUG-2013 (extractParentLink treats a PRESENT-but-empty "parent" key in
//     fields_patch as "detach"), but neither client surface could reach it.
//     `pad item update --parent ""` was a silent no-op (the CLI's
//     `if parentRef != ""` guard drops it before it ever becomes a key), and
//     the MCP `parent` param — a plain string — has the same ambiguity every
//     other schema-declared string on this tool has: empty reads as "not
//     provided", so `parent: ""` cannot be given a destructive meaning
//     without becoming a trap for a client that pads unused params with "".
//
//     Same two reasons as `clear_assigned_user` / `clear_agent_role` (v0.18)
//     for using a boolean rather than overloading the empty string:
//     1. Keeps the "empty declared string = not provided" invariant intact
//     for every other param on this tool.
//     2. Only a boolean reaches LOCAL STDIO — BuildCLIArgs emits the CLI's
//     real flags, so a catalog param with no flag behind it is dropped
//     before dispatch. `clear_parent` maps to a new `--clear-parent`
//     bareword flag on `pad item update`, exactly as `clear_assigned_user`
//     maps to `--clear-assigned-user`.
//
//     UPDATE ONLY, same asymmetry as v0.18 and for the same reason: an item
//     has no parent unless one is given at create, so a create-time clear
//     would be a no-op teaching a wrong affordance.
//
//     A simultaneous set-and-clear (`parent` + `clear_parent`, including via
//     `field: ["parent=..."]`) is REFUSED on both transports, not silently
//     resolved — mirrors the v0.18 conflict checks.
//
//     Also refused, not silently applied: `clear_parent` against a collection
//     whose schema declares its own "parent"/"plan" field — extractParentLink
//     skips hierarchy handling entirely for a schema-shadowed key and lets it
//     fall through as an ordinary field write, so the wire shape
//     {"parent":""} can no longer distinguish clear-hierarchy intent from a
//     legitimate blank-a-real-field write once it reaches the server; the
//     ambiguity is created at the client surface that accepted `clear_parent`,
//     so that surface refuses rather than guessing (codex round 2).
//
//   - "0.18" — historical. IDEA-2584: `clear_assigned_user` / `clear_agent_role`
//     booleans on `pad_item`, the canonical and DISCOVERABLE way to unassign.
//     Additive param bump, v0.5 / v0.6 precedent — no existing tool, action or
//     param changed shape, and v0.16/v0.17's empty-string forms keep working
//     and are NOT deprecated.
//
//     What this closes: v0.16 and v0.17 made the clear WORK, but the params
//     that do it (`assigned_user_id` / `agent_role_id`) were never in the
//     catalog. An agent reading the schema to find out how to unassign saw
//     only `assign` (a name) and reached for `assign: ""`, which is a no-op
//     and stays one. The capability existed; nothing advertised it.
//
//     WHY BOOLEANS, not the empty-string params declared as-is. Two reasons,
//     and the second is decisive:
//     1. An empty DECLARED string is inert everywhere else on this tool
//     (title, content, comment, tags), so a client that pads optional
//     params with "" is harmless today. Giving one a destructive meaning
//     would turn that client into one that silently unassigns everything
//     it touches. A boolean carries its meaning in its name.
//     2. Only a boolean can reach LOCAL STDIO. BuildCLIArgs emits the CLI's
//     REAL flags, so a catalog param with no flag behind it is dropped
//     before dispatch — declaring `assigned_user_id` would have left the
//     direct form remote-only, i.e. would not have closed the gap it
//     exists to close. These map to `--clear-assigned-user` /
//     `--clear-agent-role`, new bareword flags on `pad item update`,
//     exactly as `allow_draft` maps to `--allow-draft`.
//
//     UPDATE ONLY, deliberately asymmetric with create: clearing at create is
//     a request to not-set something never set, so the only honest behaviour
//     is a no-op — which teaches a wrong affordance and pads every create
//     call's schema. `item create` has no such flags and a test fails if
//     someone adds them. An item is created unassigned unless `assign` is
//     given.
//
//     A simultaneous set-and-clear is REFUSED on both transports rather than
//     silently resolved: the store's branch order sets before it clears, so
//     sending both would make the clear a no-op and the assignment win. The
//     check runs AFTER --field lifting, since that is a second route to a
//     competing value.
//
//     Server-side this is WIRING, not new semantics:
//     models.ItemUpdate.ClearAssignedUser / ClearAgentRole already existed and
//     the store has honoured them since BUG-2566, on the same branch as the
//     empty-string form.
//
//   - "0.17" — historical. BUG-2583: unassign now works on the LOCAL STDIO
//     transport too, closing the gap v0.16 documented. No tool, action, or
//     parameter shape changed — another BEHAVIOUR bump, same grounds as
//     v0.16 and v0.9.
//
//     THE FORM MATTERS, and only one works everywhere:
//     `field: ["assigned_user_id="]` clears on BOTH transports. The direct
//     `assigned_user_id: ""` param clears on REMOTE ONLY — it is not
//     declared in the pad_item schema, so it survives to the remote mapper
//     by riding the verbatim input map, while BuildCLIArgs drops it on
//     stdio (verified: the stdio call is a clean no-op, not a corrupting
//     one). IDEA-2584 tracks declaring the params properly; until then the
//     `field` form is the one to document.
//
//     v0.16 fixed the two filters in the remote dispatcher. Local stdio MCP
//     goes nowhere near them: ExecDispatcher shells out to the `pad` CLI,
//     and the CLI wrote `--field assigned_user_id=<uuid>` into the item's
//     FIELDS JSON BLOB while the column stayed stale — then printed
//     "Updated TASK-9". A success message for a write that did nothing the
//     caller asked for, and a blob key shadowing a real column's name.
//     `cmd/pad/cmd_item.go` now lifts columnFieldKeys onto the columns on
//     both create and update, mirroring liftFieldsToColumns (and its
//     INVARIANT) on the MCP side, so the two surfaces stop diverging.
//
//     TWO compat changes, ruled separately: (1) NON-empty
//     `--field assigned_user_id=<uuid>` now writes the COLUMN and no longer
//     writes the blob key — accepted on the grounds that relying on the old
//     behaviour is relying on a shadowing defect; (2) EMPTY clears the
//     column, which falls out of the lift and inherits BUG-2566's store
//     semantics. `agent_role_id` gets identical treatment. Existing stray
//     blob keys are left alone — this stops minting new ones; a cleanup
//     sweep would be its own change.
//
//     Still true from v0.16: an empty `assign` / `role` does NOT clear on
//     either transport, deliberately (IDEA-2584) — see resolveAssignName.
//
//   - "0.16" — historical. TASK-2571: an MCP agent can now UNASSIGN an item.
//     No tool, action, or parameter shape changes — this is a BEHAVIOUR
//     change, bumped on the same grounds as v0.9 (which moved for a return
//     shape with an unchanged signature): an empty-string
//     `assigned_user_id` / `agent_role_id`, whether passed at the top level
//     or via `field: ["assigned_user_id="]`, was silently dropped by the
//     dispatcher and is now forwarded as a clear-to-NULL.
//
//     Compat posture: ACCEPTED, not worked around. A caller sending `""`
//     today gets a no-op; after this they get a clear. That is the correct
//     reading of the input — nobody sends an empty assignment ID meaning
//     "leave it alone" — and the no-op was the surprising half. The store
//     has had defined clear-to-NULL semantics for exactly these two columns
//     since BUG-2566 and the HTTP surface inherited them, so this is
//     uniformity restoration: MCP was the only surface with no way to
//     unassign.
//
//     Deliberately NOT changed: the empty-string filter on `tags` at the
//     same call site (codex #547 r3 P2). `tags: ""` is not a clear, it is a
//     corrupt write into a JSONB/TEXT column. Same-looking guard, opposite
//     justification.
//
//     Also not done: `clear_assigned_user` / `clear_agent_role` schema flags
//     mirroring the HTTP body (option (b) on the task) — deferred by the
//     lead as additive sugar, then reopened by codex review and filed as
//     IDEA-2584: the catalog exposes `assign` / `role`, NOT the ID params,
//     so an agent reading the schema still cannot discover the clear.
//
//     TRANSPORT SCOPE, verified live rather than assumed. This fixes the
//     REMOTE /mcp transport (HTTPHandlerDispatcher), which is where both
//     filters lived. LOCAL STDIO MCP — `pad mcp serve`, i.e. Claude
//     Desktop / Cursor / Windsurf — still cannot clear an assignment,
//     because ExecDispatcher shells out to the CLI and the CLI has no
//     unassign at all: `--assign` / `--role` skip on empty, and
//     `--field assigned_user_id=` lands in the item's fields JSON blob
//     (observed: fields became {"assigned_user_id":"",...} while the
//     column stayed set) instead of being lifted onto the column the way
//     liftFieldsToColumns does for HTTP. That is a separate defect with a
//     CLI-wide blast radius, tracked as BUG-2583 — do not read this
//     entry as covering it. CLOSED IN v0.17 BELOW; this paragraph
//     describes v0.16 only. The schema-declared `assign` / `role`
//     aliases also still no-op on both transports (IDEA-2584); see
//     resolveAssignName for why an empty alias is deliberately NOT a
//     clear.
//
//   - "0.15" — historical. TASK-2096: adds the `unparented` boolean parameter
//     to `pad_item.list`, mutually exclusive with `parent`, and forwards it
//     through both local exec and remote HTTP dispatchers. The parameter
//     selects items with neither the legacy parent_id column nor an outgoing
//     parent/implements relationship.
//
//   - "0.14" — historical. TASK-2022: adds a `history` action to `pad_item`
//     (read-only item version history — newest-first metadata: id,
//     created_at, created_by, source, change_summary; the resolved
//     content body is omitted for token thrift). Also adds an
//     `expected_updated_at` param to `pad_item` for optimistic-
//     concurrency on `update`: round-trip the updated_at you last read
//     and the update fails with a structured 409 (code=update_conflict)
//     if the item changed since. The `update` action's field writes are
//     now a server-side field-level MERGE (only the keys you set change)
//     rather than a full-blob replace, closing the concurrent-update
//     lost-write race (IDEA-1480) — a behavior change to the update
//     path plus a new action and a new param, hence the version bump.
//     Pure addition to the action enum + param vocabulary; existing
//     pad_item actions/params are unchanged and backwards-compatible.
//
//   - "0.13" — TASK-2019: agent-oriented backlog queries.
//     Adds `ready` + `stale` actions to `pad_project`, mirroring the
//     existing CLI `pad project ready` / `pad project stale`. `ready`
//     (read-only) returns the current actionable backlog — the
//     query-oriented counterpart to `pad project next`, reusing the
//     dashboard's suggested-next logic. `stale` (read-only) lists items
//     needing attention (stalled, blocked, overdue, or otherwise out of
//     the active workflow). Both HTTP dispatchers already existed
//     (dispatch_http_project.go); this bump wires them onto the catalog
//     surface. `pad project reconcile` stays CLI-only — it shells out to
//     `gh` to compare stored PR metadata against live GitHub state, a
//     local-git dependency an MCP agent lacks. Pure addition of two
//     read-only actions; existing pad_project actions unchanged.
//     Backwards-compatible for v0.12 consumers that don't enumerate the
//     new actions.
//
//   - "0.12" — TASK-2018: agent-accessible activity feed.
//     Adds an `activity` action to `pad_project` mirroring the new CLI
//     `pad project activity [--limit N] [--actor user|agent] [--since DATE]`.
//     It's the non-streaming, bounded query counterpart to
//     `pad project watch` (the live SSE stream, which stays CLI-only):
//     a read-only snapshot of the workspace's enriched activity feed —
//     item refs, titles, and field-level change details — so an agent
//     can catch up on what OTHER agents/users did since it last worked.
//     Backed by the existing `GET /workspaces/{ws}/activity` endpoint
//     (previously web-UI-only), extended with a server-side `since`
//     date filter so `limit`, `actor`, and `since` behave identically
//     across the CLI, local stdio MCP, and cloud HTTP transports. Adds
//     `actor` + `limit` params to the pad_project vocabulary (`since`
//     already existed for changelog). Pure addition of one action +
//     params; existing pad_project actions unchanged. Backwards-
//     compatible for v0.11 consumers that don't enumerate the new
//     action.
//
//   - "0.11" — TASK-2017: read-only attachments surface.
//     Adds a new `pad_attachment` tool (the tenth resource × action
//     tool) with two read-only actions — `list` and `show` — mirroring
//     the CLI `pad attachment list` / `pad attachment show`. `list`
//     (read-only) enumerates a workspace's attachments with optional
//     filters (item, category, collection, attached/unattached, sort,
//     limit, offset); `show` (read-only) returns one attachment's
//     metadata (MIME, size, filename, ETag, last-modified) via a HEAD
//     request without transferring bytes. Both HTTP dispatchers already
//     existed (dispatch_http_attachments.go, TASK-871 era); this bump
//     just wires them onto the catalog surface. Upload / download / view
//     stay CLI-only (filesystem-bound) and are NOT exposed. Pure
//     addition of one tool + two read actions; existing tools/actions
//     unchanged. Backwards-compatible for v0.10 consumers that don't
//     enumerate the new tool. The base64 image RESOURCE for multimodal
//     agents was tracked separately (TASK-2076/2077) and shipped later
//     in PR #930; it was not part of this bump.
//
//   - "0.10" — BUG-2020: server-side draft-playbook gate.
//     `pad_playbook.run` now refuses a playbook whose status isn't
//     "active" (a draft still being authored) with a structured
//     `playbook_not_active` error, and adds an `allow_draft` boolean
//     param (escape hatch) that runs a draft anyway. Both the `run` and
//     `get` responses now echo the playbook's `status`. Pure addition of
//     one param + one echoed field + a new refusal path; existing active
//     playbooks run unchanged. Backwards-compatible for v0.9 consumers
//     that don't set allow_draft — except that running a draft (which
//     the skill already told agents not to do) now errors instead of
//     silently returning the body.
//
//   - "0.9" — TASK-2000: `pad_item.list` is now summary-shaped
//     and bounded. Two changes for agent-token thrift:
//
//   - The `list` action injects a default `limit` (50) and clamps an
//     oversized one (max 300), mirroring the backlinks default/max, so
//     a bare agent list can't dump the whole workspace into context.
//
//   - The list RESULT shape changed: `pad item list` (which the
//     ExecDispatcher shells out to) now defaults to a token-light
//     SUMMARY projection — the rich `content` body is replaced by a
//     short `content_preview`, UUID plumbing (id, workspace_id,
//     collection_id, *_user_id, parent_id, agent_role_id) and the
//     duplicate collection/parent join fields are dropped, and
//     `fields`/`tags` are emitted as nested JSON rather than escaped
//     strings. This is a BREAKING result-shape change for consumers
//     that read `content` or the dropped fields off a list row; the
//     full former shape is available via the CLI `--full` flag (at
//     v0.9 not yet surfaced as an MCP param; v0.20 declared it as the
//     `full` boolean and closed the HTTP transport's shape gap —
//     BUG-2305). No action-enum or param
//     removals; `limit` semantics unchanged for callers that pass one
//     under the max.
//
//   - "0.8" — historical. TASK-1973: workspace soft-delete recovery.
//     Adds two actions to `pad_workspace` mirroring the CLI
//     `pad workspace deleted` / `pad workspace restore` (TASK-1972):
//     `deleted` (read-only) lists the caller's soft-deleted workspaces
//     still inside the 30-day restore window; `restore` (mutating, not
//     destructive) un-soft-deletes a workspace by `slug` while it's
//     still restorable (owner-only). Both non-interactive. Reuses the
//     existing `slug` param (now also required for action=restore); no
//     new params. Pure addition; existing pad_workspace actions
//     unchanged. Backwards-compatible for v0.7 consumers that don't
//     enumerate the new actions.
//
//   - "0.4" — PLAN-1410: comprehensive bootstrap-payload
//     trim, cutting ~40% of bytes off the AgentBootstrap response.
//     Same tool catalog (still eight resource × action tools +
//     pad_set_workspace); the shape changes are entirely inside the
//     bootstrap JSON those tools/resources return:
//
//   - Slim BootstrapCollection projection (TASK-1412): drops `id`,
//     `workspace_id`, `created_at`, `updated_at`, `settings`;
//     `schema` is now a nested JSON object rather than an
//     escaped JSON-encoded string.
//
//   - Slim BootstrapRole projection (TASK-1423): drops `id`,
//     `workspace_id`, `tools`, `created_at`, `updated_at`.
//
//   - Convention `slug` dropped (TASK-1413) — agent addresses by ref.
//
//   - Top-level `recent_activity` removed (TASK-1413) — was a
//     bit-for-bit duplicate of `dashboard.recent_activity`.
//
//   - BootstrapDashboard wrapper caps five dashboard sub-arrays
//     (attention, recent_activity, active_items, active_plans,
//     by_role) at 5 entries each, with parallel
//     `<name>_overflow_count` fields surfaced when truncation
//     fired. TASK-1413 added the first two; TASK-1422 added the
//     remaining three. suggested_next deliberately excluded —
//     already capped to 3 upstream in buildDashboardResponse.
//
//   - Schema field `label` omitted when label == TitleCase(key)
//     (TASK-1424); custom labels preserved.
//
// Compatibility note: most v0.4 changes are subtractive (dropped
// fields) or additive (overflow counts), but ONE field had its
// JSON type change — collections[].schema went from a
// JSON-encoded string ("schema":"{\"fields\":[...]}") to a nested
// JSON object ("schema":{"fields":[...]}). This is a breaking
// change for any v0.3 consumer that read schema as a string and
// JSON.parse()'d it themselves. Agents now read it as a parsed
// object directly. Clients that relied on the dropped fields
// (UUIDs, timestamps, settings, the duplicate recent_activity,
// convention.slug) need to switch to the canonical alternatives
// (slugs for addressing; pad collection list / pad role list for
// the full models when needed).
//
// Discovery surfaces:
//
//   - result.capabilities.experimental.padToolSurface.version (handshake).
//   - pad://_meta/version resource (queryable JSON document).
//   - pad_meta.action: tool-surface (full catalog introspection).
//   - "0.22" — BUG-2674: `pad_item.action=move` no longer
//     destroys an item's system metadata, and now REFUSES a `field`
//     setter naming one of those keys.
//
//     BEHAVIOR bump on the v0.9/v0.16/v0.17 grounds — no tool, action
//     enum, or param shape changed. Two observable differences:
//
//     A move used to drop implementation_notes / decision_log /
//     github_pr / convention outright, because MigrateFields matched
//     every key against the destination schema and no schema declares
//     those. They now carry, and any field the target schema HAS no
//     home for is reported in the move's activity entry rather than
//     vanishing. github_pr is the one exception and only across
//     WORKSPACES (the copy path), where the repository it names
//     belongs to the source project — reported there as
//     `referent_not_portable`.
//
//     `field: ["implementation_notes=..."]` on a move or copy now
//     answers `malformed_override` instead of writing the key. That
//     write was never legitimate: it bypassed the append guard
//     (BUG-2627) and, on a cross-workspace copy, could reintroduce the
//     github_pr the migration had just dropped. Agents write these
//     through `action=note` / `action=decide` and the GitHub link flow.
//
//     Compat posture, stated deliberately: today's callers passing such
//     a setter get a 400 where they previously got a silent corrupt
//     write. Relying on the old behaviour is relying on a defect, the
//     same reading v0.17 took for the fields-blob shadowing.
//
//   - "0.23" — BUG-2627 part 2 + BUG-2675, one bump for two
//     changes at the same door. BEHAVIOR bump on the v0.9/v0.16/v0.17/
//     v0.22 grounds — no tool, action enum, or param shape changed.
//
//     (1) `pad_item.action=update` REFUSES a `field` setter naming
//     implementation_notes, decision_log or convention, where it
//     previously wrote it. HTTP answers 400 `validation_error`; MCP
//     clients see that as `validation_failed`, which is the code the
//     catalog and instructions.md name, since that is the one an agent
//     branches on.
//
//     `github_pr` is deliberately NOT refused, and the exception is
//     load-bearing rather than a soft edge. The rule being applied is
//     "a raw write is refused where a real writer exists"; for the
//     other three that writer reaches every surface, and for github_pr
//     it does not — `pad github link` needs a local git checkout and
//     the `gh` CLI, so it is excluded from remote MCP by name, and
//     noRemoteEquivalent points remote agents at
//     `item update --field github_pr=...` as the alternative. Refusing
//     it would have deleted the only door those agents have and
//     answered with a message naming a command they cannot run (Codex
//     round 3).
//
//     Round 4 then found that door does not actually work: a `field`
//     value is stored as a STRING on every surface, so the PR data
//     lands double-encoded and no link appears — the BUG-2627 shape one
//     key over, filed as BUG-2696. That does not change the exemption
//     (refusing would leave remote agents with strictly less), but it
//     does change what the catalog and instructions.md may PROMISE, so
//     both now say the door is open and broken rather than advertising
//     a capability that isn't there. v0.22 closed the same door on move/copy field
//     OVERRIDES; this closes it on the ordinary update, which is the
//     door agents actually reach for. Server-side in the fields_patch
//     gate, so it lands on BOTH transports at once — remote posts
//     fields_patch directly, and stdio's CLI (`pad item update
//     --field`) lowers into the same key. That was verified at both
//     call sites rather than assumed, because a fix that reaches one
//     transport is the shape v0.16 shipped and v0.17 had to finish.
//
//     Compat posture, same as v0.22's: today's callers passing such a
//     setter get a 400 where they previously got a write that made the
//     entries unreadable everywhere AND disabled `action=note` /
//     `action=decide` on that item until the row was repaired. Relying
//     on the old behaviour is relying on a defect.
//
//     Item CREATE is deliberately NOT covered: its full-`fields` door
//     is shared with Pad's own writers (convention activation lowers
//     into it), so it needs a different rule, tracked in BUG-2685.
//
//     (2) NEW ERROR CODE `stored_state_unreadable` (BUG-2675) — the
//     first addition to the closed ErrorCode set since v0.14's
//     update_conflict. Fires when an operation is refused because the
//     ITEM'S STORED value cannot be decoded: today, `action=note` /
//     `action=decide` against a field whose stored value is not a list
//     of entries (the v0.22-era guard from BUG-2627 part 3).
//
//     It replaces `server_error` for exactly that condition, which was
//     wrong in both halves — not our fault, and not transient. The
//     defining property is RETRY-HOSTILITY: the refusal is fully
//     deterministic, so an agent that backs off and retries burns
//     round-trips against a state only a repair can clear. Both
//     transports emit it: HTTP classifies the sentinel error directly,
//     stdio via the `pad-structured-error/v1:` marker the CLI now
//     writes for its own local refusal.
//
//     ADDITIVE for consumers that switch on code — an unknown code was
//     always possible and the envelope shape is unchanged — but a
//     client that pattern-matched `server_error` to detect this
//     condition would stop matching. That client was retrying a
//     permanent failure.

//   - "0.26" — current. IDEA-2756: `pad_workspace.action=create` is now
//     REFUSED with a 403 when the calling OAuth connection's grant has
//     `may_create_workspaces=false`. Previously that flag gated only the
//     post-creation auto-add, so the create succeeded — and on a
//     connection with an EXPLICIT workspace allow-list the agent was
//     handed a workspace it could not then see. (Not universal: a
//     connection with `all_current_workspaces=true` is not gated per
//     slug, so it could see what it made. The consent mismatch is the
//     constant across both; the invisibility was only its most visible
//     symptom.)
//
//     BEHAVIOR bump, not a shape one — no tool name, action enum, or
//     parameter changed. Same grounds as v0.25 (activation destination),
//     v0.16 (empty-string clear) and v0.10, which is the closest
//     precedent: a server-side gate that starts refusing a call it used
//     to permit, with the refusal surfaced as a structured error.
//
//     Unlike v0.10 there is no `allow_draft`-style escape hatch, and
//     deliberately so: the gate expresses a decision the USER made on
//     the consent screen, so a parameter that let the caller bypass it
//     would be the app overriding its own grant. Both remedies are the
//     USER's and neither is the app's: re-authorize, or enable the flag
//     on the existing connection via PATCH /connected-apps/{id}/flags
//     (the console page). The refusal message names both, and
//     instructions.md tells the agent not to retry and not to reach for
//     the claim flow (there is nothing to claim when nothing was
//     created).
//
//     Ruled by Dave on IDEA-2756: the consent checkbox is a permission
//     on whether the connected token may CREATE, and has to be true to
//     what a user would honestly expect from the option. Applied to
//     `POST /workspaces/import` as well as `POST /workspaces` — import
//     mints a workspace through store.ImportWorkspace, so it is the
//     same permission at a second door — though import has no MCP
//     action today and so is invisible from this surface.
//
//   - "0.25" — TASK-2657: `pad_library.activate` resolves its
//     DESTINATION collection from the target's declared artifact kind
//     (SPEC-5 collection traits) rather than from the literal slugs
//     "conventions" / "playbooks".
//
//     BEHAVIOR bump, not a shape one — no tool name, action enum, or
//     parameter changed. Same grounds as v0.9 (list return shape) and
//     v0.16 (empty-string clear semantics): what the tool DOES changed
//     while its signature did not, and a consumer reasoning about where
//     an activation lands needs to know.
//
//     Before: activation posted to the literal slug, so a workspace that
//     had renamed either collection got a not-found with the collection
//     sitting right there (BUG-2702). After: it posts to whichever
//     collection declares that artifact kind, falling back to the
//     canonical slug ONLY when the lookup SUCCEEDS and finds no
//     declaration — the genuine pre-backfill case. A lookup ERROR is now
//     surfaced rather than silently falling back, because falling back on
//     an error means writing to a slug nothing was confirmed about.
//
//     Also in this change, not itself a surface bump: `pad_item.list`
//     against a renamed conventions/playbooks collection still needs the
//     current slug — the trait moves the KERNEL behaviors, not the
//     addressing of an explicit collection query. instructions.md says so
//     now, at the point an agent would otherwise trust the literal.
//
//   - "0.24" — #1066: the pad_item `fields` OBJECT is now a
//     real write form, and undeclared input keys fail loudly. Two
//     halves, one contract change:
//
//     (1) `fields` alias. Since the BUG-991 normalization, reads return
//     `fields` as a native object, so writing back the structure you
//     just read was the obvious call — and it was a silent no-op: not a
//     declared param, nothing set additionalProperties, so the object
//     was accepted, never mapped by BuildCLIArgs, and dropped while the
//     PATCH still ran (success + bumped updated_at + unchanged value).
//     create/update now fold `fields` into the same path as `field` /
//     the dedicated params (catalog_item_fields.go). The same key with
//     CONFLICTING values in two places is REFUSED with a structured
//     error — refuse-on-ambiguity, same disposition as clear_parent /
//     clear_assigned_user; equal duplicates collapse to one write.
//     Non-writer actions refuse a `fields` param loudly rather than
//     letting the now-declared key be dropped at dispatch.
//
//     (2) Strict input validation. The fan-out handler now rejects any
//     top-level key outside the tool's declared schema (action,
//     workspace, declared params, and a small documented compat list —
//     pad_item's v0.16 assigned_user_id / agent_role_id clear form)
//     with a structured validation_failed naming the offending keys.
//     This turns every future undeclared-param variant of this bug into
//     a loud error instead of a silent no-op, across ALL catalog tools.
//
//     Bump rationale: (1) is additive (new param), but (2) changes the
//     contract for inputs that previously "succeeded" — any consumer
//     relying on an undeclared key being ignored now gets an error.
//     That reliance was indistinguishable from a bug in the caller
//     (the key never did anything), so the break is the fix. Single
//     bump covers both halves; they are one contract change.
//
//     0.27 — BUG-2850. Field values are TYPED SERVER-SIDE, the `fields`
//     object reaches the remote door with its JSON types intact, and
//     the fields/field merge refuses several ambiguities it used to
//     resolve silently.
//
//     (1) Coercion. A string arriving for a declared number/json field
//     is coerced against the collection schema before validation, at
//     all eight Validate* call sites. The remote /mcp transport builds
//     its field map in ingestFieldKVP, so EVERY value reached the
//     server as a string and a declared number or json field was
//     unwritable from that transport — a 400 on a correct value in the
//     wrong clothes. Uncoercible values are still refused with the
//     validator's existing message.
//
//     (2) The `fields` OBJECT carries native types. The catalog no
//     longer stringifies it; the HTTP mappers apply it last so the
//     types survive. The key=value doors (CLI --field, stdio MCP via
//     the CLI, remote field:[…]) are string-by-construction and
//     unchanged — the fidelity is available wherever the encoding
//     carries it, not everywhere.
//
//     (3) Undeclared keys are NAMED. Item create/update responses may
//     carry `warnings.undeclared_fields`; the CLI prints the same list
//     to stderr, never stdout. Additive and omitempty, so a clean
//     write is byte-identical to before. Keys are ACCEPTED, not
//     refused: a census of 1012 items found 14 undeclared keys across
//     168 live values (priority alone at 127), so refusing would have
//     broken read-modify-write on items nobody had edited wrongly.
//
//     (4) ONE conflict check over a canonical view of every source.
//     The `fields` object, the `field:[]` entries, the promoted named
//     params and the v0.16 compat IDs are resolved to a canonical key
//     and adjudicated once. What this REFUSES that 0.26 permitted:
//
//   - two names for one target in a single call — parent/plan,
//     assign/assigned_user_id, role/agent_role_id — refused even
//     when the values match, because the names address one thing
//     through different vocabularies (a slug and a UUID are not
//     comparable), and the doors resolved them differently;
//
//   - the same key supplied through the `fields` object and
//     another source with DIFFERING values (equal ones collapse to
//     one write);
//
//   - a non-string `assign` / `role`, which one door silently
//     dropped and the other rejected;
//
//   - an empty hierarchy value inside `fields`, which promoted
//     onto a param both doors read as "not supplied" and so
//     reported success having detached nothing. Use clear_parent.
//
//     NOT refused, deliberately — but NARROWER than it first reads. A
//     SCHEMA-DECLARED param colliding with a `field:[]` entry under the
//     same name, with no `fields` object, still resolves: the CLI has a
//     real flag for such a param, so stdio receives BOTH forms and its
//     overlay order resolves them exactly as the HTTP mapper does. It
//     is visibly a duplicate, so last-write-wins is a resolution the
//     caller can predict. Pinned per door so they cannot drift apart.
//
//     The v0.16 compat IDs are NOT exempt, and being undeclared is
//     precisely why. BuildCLIArgs emits the CLI's real flags and there
//     is none behind `assigned_user_id`, so the top-level value is
//     DROPPED: stdio sees only the field entry while HTTP reads the
//     param. Two different people assigned from one call, with no
//     `fields` object anywhere. Last-write-wins cannot be the answer
//     when the doors do not receive the same writes, so that pair
//     refuses.
//
//     Bump rationale: (1)-(3) are additive or fix outright breakage,
//     but (4) refuses calls 0.26 accepted. Same grounds as 0.26, 0.25,
//     0.16, 0.10 and 0.9 — no tool name, action enum or parameter
//     shape changed, and the behaviour did. Every refusal added here
//     replaced a call that SUCCEEDED while doing something other than
//     what it said, so the break is the fix in each case.
const ToolSurfaceVersion = "0.27"

// MetaVersionURI is the canonical URI of the queryable version document.
// Lives outside the pad://workspace/{ws}/... namespace because it's a
// server-wide attribute, not a workspace-scoped resource.
const MetaVersionURI = "pad://_meta/version"

// The MCP wire protocol revision this server speaks isn't a constant
// owned by pad — it's whatever mcp-go's `LATEST_PROTOCOL_VERSION`
// resolves to at build time, since that's what NewMCPServer will
// negotiate with clients that request the latest. The meta resource
// reads it dynamically (see meta.go) so the value never drifts from
// what the library actually advertises.

// experimentalCapabilityKey is the JSON object key under
// capabilities.experimental that carries the cmdhelp tier in the
// initialize handshake. Namespaced so other servers' experimental
// capabilities don't collide.
const experimentalCapabilityKey = "padCmdhelp"

// experimentalToolSurfaceKey is the JSON object key under
// capabilities.experimental that carries the MCP tool-catalog tier in
// the initialize handshake. Distinct from experimentalCapabilityKey so
// the cmdhelp and tool-surface contracts can version independently.
const experimentalToolSurfaceKey = "padToolSurface"
