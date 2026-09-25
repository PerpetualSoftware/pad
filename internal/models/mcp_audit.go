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
	ID          string
	Timestamp   time.Time
	UserID      string
	WorkspaceID *string
	TokenKind   TokenKind
	TokenRef    string
	ToolName    string
	// ToolNameSource says where ToolName came from (BUG-2819).
	ToolNameSource MCPToolNameSource
	ArgsHash       string
	ResultStatus   MCPAuditResultStatus
	ErrorKind      *string
	LatencyMs      int
	RequestID      string
}

// MCPToolNameSource says where an audit row's tool_name came from
// (BUG-2819). The stored name cannot say on its own: a caller can name a tool
// "(unknown)", which is also what the server records for an unparseable body.
// The values match mcp_audit_log.tool_name_source (migration 096), which has a
// CHECK constraint on exactly this set. The "(sanitised) " / "(unknown)"
// namespace reservation in the audit middleware stays as defence in depth; it
// is no longer the only way to tell the two apart.
type MCPToolNameSource string

const (
	// MCPToolNameFromCaller is the caller's tool (or method) name as sent.
	MCPToolNameFromCaller MCPToolNameSource = "caller"
	// MCPToolNameSanitised is the caller's name with the "(sanitised) " mark:
	// cleaning changed it, or it entered the reserved leading-"(" namespace.
	MCPToolNameSanitised MCPToolNameSource = "sanitised"
	// MCPToolNameSynthesised is a placeholder the server chose because the
	// request named nothing usable ("(unknown)", "tools/call").
	MCPToolNameSynthesised MCPToolNameSource = "synthesised"
	// MCPToolNameSourceUnknown marks a row written before provenance was
	// recorded.
	MCPToolNameSourceUnknown MCPToolNameSource = "unknown"
)

// Valid reports whether v is one of the stored values.
func (v MCPToolNameSource) Valid() bool {
	switch v {
	case MCPToolNameFromCaller, MCPToolNameSanitised, MCPToolNameSynthesised, MCPToolNameSourceUnknown:
		return true
	}
	return false
}

// MCPAuditEntryInput is the write-side shape for InsertMCPAuditEntry.
// Distinct from MCPAuditEntry so the store can mint the ID + accept a
// caller's already-set timestamp (the middleware records the pre-
// handler instant for accurate latency, then writes async).
type MCPAuditEntryInput struct {
	Timestamp   time.Time
	UserID      string
	WorkspaceID string // empty → NULL
	TokenKind   TokenKind
	TokenRef    string
	ToolName    string
	// ToolNameSource is where ToolName came from; empty is stored as
	// "unknown", never as "caller" (BUG-2819).
	ToolNameSource MCPToolNameSource
	ArgsHash       string
	ResultStatus   MCPAuditResultStatus
	ErrorKind      string // empty → NULL
	LatencyMs      int
	RequestID      string
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
