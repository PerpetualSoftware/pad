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
//   - "0.2" — historical. Hand-curated resource × action catalog
//     (PLAN-969, TASK-981). The cmdhelp leaf walker retired; tools/list
//     advertises only the catalog (~7 tools + pad_set_workspace).
//   - "0.3" — historical. PLAN-1377 / TASK-1380:
//   - pad_meta gains an action: bootstrap that returns the
//     AgentBootstrap blob (and pad_meta.Schema.Workspace flipped
//     to true so the workspace param is available to that action).
//   - pad_set_workspace's response shape extends from
//     {workspace, status} to {workspace, status, bootstrap?} —
//     the embedded blob lets one call hand the agent full session
//     context. Purely additive; older clients that ignore unknown
//     keys keep working.
//   - pad://workspace/{ws}/bootstrap resource added.
//   - "0.5" — historical. PLAN-1560 / TASK-1563: adds `pad_library` to
//     the catalog as the ninth resource × action tool. Three actions —
//     list / get / activate — surface the global convention + playbook
//     library (previously CLI-only) to MCP callers. Pure addition; no
//     existing tool/action/param/bootstrap shapes changed. Backwards-
//     compatible for any v0.4 consumer that doesn't enumerate the new
//     tool.
//   - "0.6" — historical. PLAN-1593 / TASK-1596: adds `backlinks` action
//     to `pad_item` so MCP callers can answer "what mentions X?"
//     without scanning the full content corpus. Adds `offset` to the
//     param vocabulary, extends `limit` to cover the backlinks
//     pagination. Pure addition; existing pad_item actions unchanged.
//     Backwards-compatible for v0.5 consumers that don't enumerate
//     the new action.
//   - "0.7" — current. Artifact export/import (Phase 5): adds two
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
//   - "0.4" — PLAN-1410: comprehensive bootstrap-payload
//     trim, cutting ~40% of bytes off the AgentBootstrap response.
//     Same tool catalog (still eight resource × action tools +
//     pad_set_workspace); the shape changes are entirely inside the
//     bootstrap JSON those tools/resources return:
//   - Slim BootstrapCollection projection (TASK-1412): drops `id`,
//     `workspace_id`, `created_at`, `updated_at`, `settings`;
//     `schema` is now a nested JSON object rather than an
//     escaped JSON-encoded string.
//   - Slim BootstrapRole projection (TASK-1423): drops `id`,
//     `workspace_id`, `tools`, `created_at`, `updated_at`.
//   - Convention `slug` dropped (TASK-1413) — agent addresses by ref.
//   - Top-level `recent_activity` removed (TASK-1413) — was a
//     bit-for-bit duplicate of `dashboard.recent_activity`.
//   - BootstrapDashboard wrapper caps five dashboard sub-arrays
//     (attention, recent_activity, active_items, active_plans,
//     by_role) at 5 entries each, with parallel
//     `<name>_overflow_count` fields surfaced when truncation
//     fired. TASK-1413 added the first two; TASK-1422 added the
//     remaining three. suggested_next deliberately excluded —
//     already capped to 3 upstream in buildDashboardResponse.
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
const ToolSurfaceVersion = "0.7"

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
