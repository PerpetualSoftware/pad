package mcp

import _ "embed"

// Instructions is the server-level instructions string advertised to
// MCP clients in the initialize response. Tells agents WHEN to reach
// for pad and gives a quick orientation to the tool surface and
// resources — same role the description text plays in tools/list,
// but at the server level so a host can show it before the first
// tool call.
//
// The Svelte MCP server in the dogfooding session that triggered
// PLAN-969 explicitly told the model "use this whenever Svelte
// development is involved." Pad does the same: a short, MCP-aware
// adaptation of skills/pad/SKILL.md's opener, embedded at build
// time so there's a single source of truth.
//
// Both ExecDispatcher (stdio) and HTTPHandlerDispatcher (HTTP) read
// this same string — the local handshake passes it via
// server.WithInstructions in NewServer (server.go); the future
// remote handshake in PLAN-943 will do the same.
//
//go:embed instructions.md
var Instructions string
