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

	// AllowedWorkspaces is the workspace allow-list set at consent
	// time (TASK-952). Three shapes:
	//   - nil — pre-TASK-952 token (no consent payload). Treated as
	//     "any workspace the user belongs to."
	//   - ["*"] — wildcard (user explicitly granted any).
	//   - explicit slugs — the user's selection.
	// The page renders chips per slug, or an "Any workspace" badge
	// for nil / wildcard.
	AllowedWorkspaces []string

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
