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
//   - "0.2" — current. Hand-curated resource × action catalog (PLAN-969,
//     TASK-981). The cmdhelp leaf walker is retired; tools/list
//     advertises only the catalog (~7 tools + pad_set_workspace).
//
// Discovery surfaces:
//
//   - result.capabilities.experimental.padToolSurface.version (handshake).
//   - pad://_meta/version resource (queryable JSON document).
//   - pad_meta.action: tool-surface (full catalog introspection).
const ToolSurfaceVersion = "0.2"

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
