package models

import "time"

// Connected-apps models (PLAN-943 TASK-954).
//
// A "connected app" is one OAuth client (Claude Desktop, Cursor, …)
// that the user has authorized to act on their behalf via the MCP
// surface. Each user-authorized OAuth flow produces one connection,
// identified by the OAuth grant chain's request_id (preserved across
// refresh-token rotations — see internal/store/oauth.go for the
// chain semantics). Revoking a connection invalidates every token in
// that chain so the next /mcp call from that client gets 401.

// CapabilityTier classifies the granted scopes into a coarse,
// user-readable bucket. Used by the connected-apps page so users
// don't have to read raw OAuth scope strings to understand what a
// given client can do.
type CapabilityTier string

const (
	CapabilityTierReadOnly   CapabilityTier = "read_only"
	CapabilityTierReadWrite  CapabilityTier = "read_write"
	CapabilityTierFullAccess CapabilityTier = "full_access" // includes pad:admin
	CapabilityTierUnknown    CapabilityTier = "unknown"     // empty / unparseable scopes
)

// OAuthConnection is one row of the connected-apps page. Joins
// oauth_access_tokens (the active grant) → oauth_clients (the DCR
// metadata). The audit-log enrichments (LastUsedAt + Calls30d) come
// from a separate aggregate query against mcp_audit_log so the page
// loads in two queries instead of N+1.
//
// One value per OAuth grant chain — the connections list deduplicates
// across refresh-token rotations because every chain member shares
// the same RequestID.
type OAuthConnection struct {
	// RequestID is the OAuth grant chain identifier — preserved
	// across refresh-token rotations and used as the public ID for
	// revoke + audit drilldown ("connection_id" on the wire).
	RequestID string

	// ClientID / ClientName / LogoURL / RedirectURIs come from the
	// DCR registration metadata (oauth_clients table). ClientName is
	// what shows on the consent screen + connected-apps cards
	// (e.g. "Claude Desktop").
	ClientID     string
	ClientName   string
	LogoURL      string
	RedirectURIs []string

	// AllowedWorkspaces is the workspace allow-list. Post-TASK-1522
	// it's the projection of oauth_connection_workspaces rows joined
	// to workspaces by ID (slug list, sorted). Two shapes the UI
	// renders against:
	//   - nil — the connection's all_current_workspaces flag is on
	//     (covers every workspace the user is a member of). UI shows
	//     an "Any workspace" badge.
	//   - explicit slugs — all_current_workspaces=false; UI shows the
	//     slug list as chips.
	// (Pre-TASK-1522 the same field also held ["*"] for wildcard
	// tokens; the backfill normalizes those to the flag and the field
	// is nil for them now. The wire DTO's nullable shape is unchanged
	// so frontend code keeps working.)
	AllowedWorkspaces []string

	// Connection-level scope flags + name from oauth_connections
	// (PLAN-1519 / TASK-1520 / IDEA-1517 §2). Pre-TASK-1522 these
	// were unsourced (the field didn't exist); post-backfill every
	// active connection has a row and the values mean what the page
	// should render. Phase D's mutation UI reads + writes these.
	//
	// Name is empty string for backfilled rows that haven't been
	// renamed yet — UI prompts on first connections-page visit.
	Name                    string
	MayCreateWorkspaces     bool
	AllCurrentWorkspaces    bool
	IncludeFutureWorkspaces bool

	// GrantedScopes is the raw space-separated scope string fosite
	// recorded at grant time. Surfaced in the per-connection expand
	// drilldown alongside CapabilityTier so power users can see the
	// exact policy.
	GrantedScopes string

	// CapabilityTier is the human-readable bucket derived from
	// GrantedScopes (read_only / read_write / full_access / unknown).
	CapabilityTier CapabilityTier

	// ConnectedAt is the earliest requested_at across the chain —
	// when the user originally authorized this connection. Survives
	// refresh-token rotation because every chain member shares the
	// same request_id and the MIN over the chain stays anchored.
	ConnectedAt time.Time

	// LastUsedAt + Calls30d come from MCPConnectionStatsForUser
	// (TASK-960's audit-log aggregate). Nil LastUsedAt means "no
	// audit entries yet" — the page renders "—" instead of a date.
	LastUsedAt *time.Time
	Calls30d   int
}
