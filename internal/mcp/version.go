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

// CmdhelpVersion is the tool-surface stability contract this MCP server
// advertises. External agents (Claude Desktop, Cursor, ChatGPT
// connectors, future Pad Cloud remote MCP) depend on tool names,
// argument shapes, and resource URIs being stable across pad releases.
// Bump the major when those change incompatibly:
//
//   - "0.1" — initial cmdhelp-derived surface from PLAN-942.
//
// Discovery surfaces (paths into the JSON-RPC envelope):
//
//   - result.capabilities.experimental.padCmdhelp.version (handshake).
//   - pad://_meta/version resource (queryable JSON document).
const CmdhelpVersion = "0.1"

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
