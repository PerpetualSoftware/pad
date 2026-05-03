package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Connected-apps store layer (PLAN-943 TASK-954).
//
// Two operations the connected-apps page needs:
//
//   - ListUserOAuthConnections — every active OAuth grant chain the
//     user authorized, deduplicated by request_id, joined to the
//     DCR client metadata, enriched from the MCP audit log with
//     last-used + 30-day-count.
//   - RevokeUserOAuthConnection — verify the chain belongs to the
//     user, then call RevokeRefreshTokenFamily + RevokeAccessTokenFamily
//     so the next /mcp call from that client gets 401.
//
// Both methods read from the existing oauth_* tables (migration 048)
// and the mcp_audit_log table (migration 049 from TASK-960). No new
// schema lands here.

// ErrConnectionNotFound is returned by RevokeUserOAuthConnection when
// no active grant chain matches (userID, requestID). Either the
// requestID is wrong / fabricated, or it belongs to a different user.
// Handlers map to 404 — explicit owner-scoped 404 prevents a malicious
// caller from enumerating chain identifiers via 403-vs-404 distinction.
var ErrConnectionNotFound = errors.New("connected_apps: connection not found")

// ListUserOAuthConnections returns every OAuth connection the user
// has active. "Active" means at least one access OR refresh token
// in the chain still has active=TRUE — revoked chains drop off the
// list immediately rather than lingering.
//
// Implementation walks oauth_access_tokens grouped by request_id,
// taking MIN(requested_at) as ConnectedAt + the most recent row's
// session_data + granted_scopes (which carry the consent allow-list
// and scope policy). Joins to oauth_clients for the DCR metadata.
//
// The audit-log enrichments (LastUsedAt + Calls30d) come from the
// caller — we return MCPConnectionStatsForUser separately so the
// caller can call both methods in parallel if they want, and so the
// store layer doesn't require the audit table to be present (this
// method works on a fresh install with no audit rows yet).
//
// Sort: ConnectedAt DESC so newest authorizations land at the top of
// the page, which matches every other "list of things I've added"
// surface in pad.
func (s *Store) ListUserOAuthConnections(userID string) ([]models.OAuthConnection, error) {
	if userID == "" {
		return nil, fmt.Errorf("connected_apps: user_id required")
	}

	// Pull every active access token row for the user. We GROUP later
	// in Go because SQLite doesn't support DISTINCT ON and the
	// GROUP BY semantics for picking-the-newest-row-per-group differ
	// across SQLite + Postgres in ways that aren't worth abstracting.
	// The user's connection count is bounded (each user authorizes
	// a handful of clients, not thousands), so the in-memory dedup
	// is cheap.
	rows, err := s.db.Query(s.q(`
		SELECT request_id, client_id, requested_at, session_data, granted_scopes
		FROM oauth_access_tokens
		WHERE subject = ? AND active = ?
		ORDER BY request_id, requested_at DESC
	`), userID, true)
	if err != nil {
		return nil, fmt.Errorf("query user connections (access): %w", err)
	}
	defer rows.Close()

	type chainAgg struct {
		clientID      string
		earliest      time.Time
		newest        time.Time
		newestSession string
		newestScopes  string
	}
	chains := make(map[string]*chainAgg)
	for rows.Next() {
		var (
			reqID, clientID, sessionData, scopes string
			requestedAt                          string
		)
		if err := rows.Scan(&reqID, &clientID, &requestedAt, &sessionData, &scopes); err != nil {
			return nil, fmt.Errorf("scan user connection (access): %w", err)
		}
		ts := parseTime(requestedAt)
		c, ok := chains[reqID]
		if !ok {
			c = &chainAgg{
				clientID:      clientID,
				earliest:      ts,
				newest:        ts,
				newestSession: sessionData,
				newestScopes:  scopes,
			}
			chains[reqID] = c
			continue
		}
		if ts.Before(c.earliest) {
			c.earliest = ts
		}
		if ts.After(c.newest) {
			c.newest = ts
			c.newestSession = sessionData
			c.newestScopes = scopes
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user connections (access): %w", err)
	}

	// A grant chain may exist in oauth_refresh_tokens with no
	// surviving access row (e.g. the access token expired but the
	// refresh hasn't been used yet). Walk refresh too so the page
	// shows those connections — otherwise a user who hasn't made a
	// call in a while would see an empty list right after a token
	// expiry.
	rrows, err := s.db.Query(s.q(`
		SELECT request_id, client_id, requested_at, session_data, granted_scopes
		FROM oauth_refresh_tokens
		WHERE subject = ? AND active = ?
		ORDER BY request_id, requested_at DESC
	`), userID, true)
	if err != nil {
		return nil, fmt.Errorf("query user connections (refresh): %w", err)
	}
	defer rrows.Close()
	for rrows.Next() {
		var (
			reqID, clientID, sessionData, scopes string
			requestedAt                          string
		)
		if err := rrows.Scan(&reqID, &clientID, &requestedAt, &sessionData, &scopes); err != nil {
			return nil, fmt.Errorf("scan user connection (refresh): %w", err)
		}
		ts := parseTime(requestedAt)
		c, ok := chains[reqID]
		if !ok {
			c = &chainAgg{
				clientID:      clientID,
				earliest:      ts,
				newest:        ts,
				newestSession: sessionData,
				newestScopes:  scopes,
			}
			chains[reqID] = c
			continue
		}
		if ts.Before(c.earliest) {
			c.earliest = ts
		}
		if ts.After(c.newest) {
			c.newest = ts
			c.newestSession = sessionData
			c.newestScopes = scopes
		}
	}
	if err := rrows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user connections (refresh): %w", err)
	}

	if len(chains) == 0 {
		return []models.OAuthConnection{}, nil
	}

	// Hydrate the client metadata in one batch per unique client_id.
	// Most users have <10 distinct clients so the per-row roundtrip
	// is fine; if it ever isn't we can switch to an IN clause.
	clientCache := make(map[string]*models.OAuthClient)
	for _, c := range chains {
		if _, hit := clientCache[c.clientID]; hit {
			continue
		}
		client, err := s.GetOAuthClient(c.clientID)
		if err != nil {
			if errors.Is(err, ErrOAuthNotFound) {
				clientCache[c.clientID] = nil // sentinel: client deleted
				continue
			}
			return nil, fmt.Errorf("hydrate client %q: %w", c.clientID, err)
		}
		clientCache[c.clientID] = client
	}

	out := make([]models.OAuthConnection, 0, len(chains))
	for reqID, c := range chains {
		client := clientCache[c.clientID]
		conn := models.OAuthConnection{
			RequestID:         reqID,
			ClientID:          c.clientID,
			ConnectedAt:       c.earliest,
			GrantedScopes:     c.newestScopes,
			CapabilityTier:    classifyCapabilityTier(c.newestScopes),
			AllowedWorkspaces: parseAllowedWorkspacesFromSession(c.newestSession),
		}
		if client != nil {
			conn.ClientName = client.Name
			conn.LogoURL = client.LogoURL
			conn.RedirectURIs = append([]string(nil), client.RedirectURIs...)
		} else {
			// Client row was deleted but the grant chain survived
			// (DeleteOAuthClient cascades, so this is rare). Surface
			// a placeholder name so the row is still revokable from
			// the UI rather than invisible.
			conn.ClientName = "(deleted client)"
		}
		out = append(out, conn)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].ConnectedAt.After(out[j].ConnectedAt)
	})
	return out, nil
}

// RevokeUserOAuthConnection invalidates every token in the OAuth
// grant chain identified by requestID, after verifying the chain
// belongs to userID.
//
// Implementation:
//  1. Look up any access OR refresh row in the chain whose subject
//     matches userID. If none, return ErrConnectionNotFound — either
//     the requestID is bogus or it's not theirs to revoke.
//  2. Call RevokeRefreshTokenFamily + RevokeAccessTokenFamily —
//     both walk the request_id index and flip every chain row
//     inactive. Handlers wrapping this call write an audit_trail
//     row recording who revoked what.
//
// Idempotent: revoking an already-revoked chain returns nil (the
// ownership check matches the inactive rows too because we don't
// filter on active=TRUE there). That's the right shape for a
// destructive UX — a user double-clicking "Revoke" shouldn't see
// "already revoked" errors.
//
// Side effect note: the chain rows stay in the DB with active=FALSE
// after this call. They're not deleted because audit / introspection
// surfaces still want to see "this token existed and was revoked at
// time X." Cleanup of long-revoked rows is a separate concern.
func (s *Store) RevokeUserOAuthConnection(userID, requestID string) error {
	if userID == "" || requestID == "" {
		return fmt.Errorf("connected_apps: user_id and request_id required")
	}

	// Belt-and-suspenders ownership check across BOTH token tables.
	// We don't use ListUserOAuthConnections because that filters to
	// active=TRUE — we want to allow re-revoking an already-revoked
	// chain idempotently.
	var subject string
	err := s.db.QueryRow(s.q(`
		SELECT subject FROM oauth_access_tokens
		WHERE request_id = ?
		LIMIT 1
	`), requestID).Scan(&subject)
	if err == sql.ErrNoRows {
		// Try refresh tokens — chain might exist there only.
		err = s.db.QueryRow(s.q(`
			SELECT subject FROM oauth_refresh_tokens
			WHERE request_id = ?
			LIMIT 1
		`), requestID).Scan(&subject)
	}
	if err == sql.ErrNoRows {
		return ErrConnectionNotFound
	}
	if err != nil {
		return fmt.Errorf("verify connection ownership: %w", err)
	}
	if subject != userID {
		// Don't leak "this exists but isn't yours" — return the same
		// not-found error so a malicious caller probing requestIDs
		// can't enumerate chains by 403-vs-404 distinction.
		return ErrConnectionNotFound
	}

	if err := s.RevokeRefreshTokenFamily(requestID); err != nil {
		return fmt.Errorf("revoke refresh family: %w", err)
	}
	if err := s.RevokeAccessTokenFamily(requestID); err != nil {
		return fmt.Errorf("revoke access family: %w", err)
	}
	return nil
}

// classifyCapabilityTier maps a fosite-style space-separated scope
// string into a coarse user-readable bucket. Currently three tiers:
//
//   - read_only:    only `pad:read` (or no write/admin scopes).
//   - read_write:   `pad:write` granted (regardless of read).
//   - full_access:  `pad:admin` granted.
//   - unknown:      empty / no recognized pad: scopes.
//
// Any future scope vocabulary additions land here.
func classifyCapabilityTier(scopes string) models.CapabilityTier {
	if scopes == "" {
		return models.CapabilityTierUnknown
	}
	parts := strings.Fields(scopes)
	var hasRead, hasWrite, hasAdmin bool
	for _, p := range parts {
		switch p {
		case "pad:read":
			hasRead = true
		case "pad:write":
			hasWrite = true
		case "pad:admin":
			hasAdmin = true
		}
	}
	switch {
	case hasAdmin:
		return models.CapabilityTierFullAccess
	case hasWrite:
		return models.CapabilityTierReadWrite
	case hasRead:
		return models.CapabilityTierReadOnly
	}
	return models.CapabilityTierUnknown
}

// parseAllowedWorkspacesFromSession extracts the workspace allow-list
// from a serialized fosite session. The producer side
// (oauth.Session.SetAllowedWorkspaces in /authorize/decide) stashes
// the list under the "allowed_workspaces" key in DefaultSession.Extra.
// JSON round-trip mangles []string into []interface{}, so we accept
// both shapes here — same as oauth.Session.AllowedWorkspaces.
//
// Returns nil for missing key (pre-TASK-952 token); empty []string
// for an explicit empty list (would be unusual but distinguishable
// from "unset"); the slug list otherwise. The handler surfaces
// nil + ["*"] as "Any workspace"; anything else as chips.
//
// Defensive: any parse failure returns nil rather than propagating
// — a malformed session payload shouldn't 500 the connected-apps
// page. Worst case the user sees "Any workspace" instead of the
// actual chip list, which is a less-bad failure mode than a broken
// page.
func parseAllowedWorkspacesFromSession(sessionData string) []string {
	if sessionData == "" {
		return nil
	}
	// fosite serializes the session as JSON. We only care about
	// session.Extra.allowed_workspaces — peek into that path
	// without depending on the fosite types.
	var outer struct {
		Extra map[string]interface{} `json:"extra"`
	}
	if err := json.Unmarshal([]byte(sessionData), &outer); err != nil || outer.Extra == nil {
		return nil
	}
	raw, ok := outer.Extra["allowed_workspaces"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		out := make([]string, len(v))
		copy(out, v)
		return out
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
