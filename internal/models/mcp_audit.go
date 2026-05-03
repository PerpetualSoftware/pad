package models

import "time"

// MCP audit log models (PLAN-943 TASK-960).
//
// Persistent record of every request that hits the /mcp endpoint —
// successful tool calls, denied calls, errors. Drives the connected-
// apps "last used" + "30-day calls" surfaces and gives forensics a
// per-user / per-connection trail.

// TokenKind discriminates the bearer that authenticated an MCP
// request. TASK-960's spec said `token_id REFERENCES oauth_tokens(id)`;
// pad has no oauth_tokens table — instead two separate token systems
// can authenticate /mcp:
//
//   - "oauth": fosite-issued access token. token_ref carries the
//     OAuth request_id (chain identifier preserved across rotations,
//     see internal/store/oauth.go's request_id column). One value per
//     "connection" the user authorized via consent.
//   - "pat":   personal access token from the api_tokens table.
//     token_ref is api_tokens.id. PATs predate the OAuth server but
//     are still a supported MCP transport for CLI / dev use.
//
// We accept both so the connected-apps page can filter to OAuth only
// (since "PATs" aren't third-party connections you'd revoke from a
// management UI), while the per-user audit query still covers every
// MCP call regardless of how it was authenticated.
type TokenKind string

const (
	TokenKindOAuth TokenKind = "oauth"
	TokenKindPAT   TokenKind = "pat"
)

// MCPAuditResultStatus is the outcome enum for an MCP request.
type MCPAuditResultStatus string

const (
	MCPAuditResultOK     MCPAuditResultStatus = "ok"
	MCPAuditResultError  MCPAuditResultStatus = "error"
	MCPAuditResultDenied MCPAuditResultStatus = "denied"
)

// MCPAuditEntry is one row of mcp_audit_log. Fields mirror the table
// 1:1 — see internal/store/migrations/049_mcp_audit.sql for the
// column-level documentation.
//
// Pointers (WorkspaceID, ErrorKind) are nullable in the DB. Empty-
// string semantics:
//
//   - ArgsHash == "": the request had no `params.arguments` (e.g.
//     `tools/list`, `initialize`). Audit-grouping queries that count
//     "distinct arg shapes" should treat empty as a sentinel rather
//     than as "all empty calls share one group".
//   - ToolName: for JSON-RPC methods that aren't tool calls
//     (`initialize`, `tools/list`, `resources/read`, etc.) we store
//     the JSON-RPC method itself ("initialize"), prefixed with no
//     namespace. For `tools/call`, we store `params.name`
//     (e.g. "pad_item"). The two namespaces are disjoint by design
//     — pad's tool catalog uses `pad_*` names, JSON-RPC methods use
//     `<group>/<verb>` — so a single column is safe.
type MCPAuditEntry struct {
	ID           string
	Timestamp    time.Time
	UserID       string
	WorkspaceID  *string
	TokenKind    TokenKind
	TokenRef     string
	ToolName     string
	ArgsHash     string
	ResultStatus MCPAuditResultStatus
	ErrorKind    *string
	LatencyMs    int
	RequestID    string
}

// MCPAuditEntryInput is the write-side shape for InsertMCPAuditEntry.
// Distinct from MCPAuditEntry so the store can mint the ID + accept a
// caller's already-set timestamp (the middleware records the pre-
// handler instant for accurate latency, then writes async).
type MCPAuditEntryInput struct {
	Timestamp    time.Time
	UserID       string
	WorkspaceID  string // empty → NULL
	TokenKind    TokenKind
	TokenRef     string
	ToolName     string
	ArgsHash     string
	ResultStatus MCPAuditResultStatus
	ErrorKind    string // empty → NULL
	LatencyMs    int
	RequestID    string
}

// MCPConnectionStats summarizes audit-log activity for one OAuth
// connection (request_id chain). Returned by the bulk-aggregate
// queries the connected-apps page reads — fetching last-used and
// 30-day-count per connection in two queries instead of N.
type MCPConnectionStats struct {
	TokenKind  TokenKind
	TokenRef   string
	LastUsedAt *time.Time // nil if no audit entries
	Calls30d   int
}
