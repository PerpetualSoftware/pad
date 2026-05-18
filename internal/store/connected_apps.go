package store

import (
	"database/sql"
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
// Post-TASK-1522 read path: connection-level state (name, scope flags,
// workspace allow-list) now reads from oauth_connections +
// oauth_connection_workspaces. We still walk oauth_access_tokens +
// oauth_refresh_tokens to know which chains are active and to pull
// ConnectedAt + GrantedScopes (those live per-token, not per-grant).
// The backfill in BackfillOAuthConnections seeds the new tables once
// at startup so this read path sees every legacy chain too — no
// dual-shape fork inside the read loop.
//
// Two queries: one chain scan, one batch-hydrate of the connection
// rows. Slug projection lives in a per-chain helper because the join
// shape is small (a handful of slugs per chain at most) and stays
// readable.
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
		SELECT request_id, client_id, requested_at, granted_scopes
		FROM oauth_access_tokens
		WHERE subject = ? AND active = ?
		ORDER BY request_id, requested_at DESC
	`), userID, true)
	if err != nil {
		return nil, fmt.Errorf("query user connections (access): %w", err)
	}
	defer rows.Close()

	type chainAgg struct {
		clientID     string
		earliest     time.Time
		newest       time.Time
		newestScopes string
	}
	chains := make(map[string]*chainAgg)
	absorb := func(reqID, clientID, requestedAt, scopes string) {
		ts := parseTime(requestedAt)
		c, ok := chains[reqID]
		if !ok {
			chains[reqID] = &chainAgg{
				clientID:     clientID,
				earliest:     ts,
				newest:       ts,
				newestScopes: scopes,
			}
			return
		}
		if ts.Before(c.earliest) {
			c.earliest = ts
		}
		if ts.After(c.newest) {
			c.newest = ts
			c.newestScopes = scopes
		}
	}
	for rows.Next() {
		var reqID, clientID, scopes, requestedAt string
		if err := rows.Scan(&reqID, &clientID, &requestedAt, &scopes); err != nil {
			return nil, fmt.Errorf("scan user connection (access): %w", err)
		}
		absorb(reqID, clientID, requestedAt, scopes)
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
		SELECT request_id, client_id, requested_at, granted_scopes
		FROM oauth_refresh_tokens
		WHERE subject = ? AND active = ?
		ORDER BY request_id, requested_at DESC
	`), userID, true)
	if err != nil {
		return nil, fmt.Errorf("query user connections (refresh): %w", err)
	}
	defer rrows.Close()
	for rrows.Next() {
		var reqID, clientID, scopes, requestedAt string
		if err := rrows.Scan(&reqID, &clientID, &requestedAt, &scopes); err != nil {
			return nil, fmt.Errorf("scan user connection (refresh): %w", err)
		}
		absorb(reqID, clientID, requestedAt, scopes)
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
			RequestID:      reqID,
			ClientID:       c.clientID,
			ConnectedAt:    c.earliest,
			GrantedScopes:  c.newestScopes,
			CapabilityTier: classifyCapabilityTier(c.newestScopes),
		}

		// Hydrate from oauth_connections + oauth_connection_workspaces.
		// Three branches:
		//   - access lookup errored      → surface (Codex #583 round 4:
		//                                  silently broadening to "any
		//                                  workspace" on a real store
		//                                  failure could expose
		//                                  workspaces the user explicitly
		//                                  scoped out via the Phase D
		//                                  mutation UI).
		//   - HasConnection == true      → project flags + slugs.
		//   - HasConnection == false     → defensive fallback (legacy
		//                                  "any workspace, default-on
		//                                  flags"); reachable only if a
		//                                  chain somehow exists without
		//                                  a backfilled connection row,
		//                                  which startup backfill should
		//                                  prevent in production.
		access, accessErr := s.GetOAuthConnectionAccess(reqID)
		if accessErr != nil {
			return nil, fmt.Errorf("hydrate connection access %s: %w", reqID, accessErr)
		}
		if access.HasConnection {
			conn.AllCurrentWorkspaces = access.AllCurrentWorkspaces
			// Pre-Codex-#585-round-2 the wildcard case set
			// AllowedWorkspaces to nil because the slugs were
			// "irrelevant" (wildcard covers everything). But Phase D's
			// mutation UI lets users PRE-STAGE workspaces while
			// wildcard is on, so the join table can hold rows that
			// the UI needs to render even when the flag is on. Always
			// populate from the join table; the UI uses the flag (not
			// the slug list emptiness) to decide whether to render
			// the "Any workspace" badge. GetOAuthConnectionAccess
			// itself still short-circuits on wildcard for the hot
			// path; this read uses a direct lookup below so we get
			// the staged rows regardless.
			if access.AllCurrentWorkspaces {
				// Direct join-table read — bypasses the access
				// projection's wildcard short-circuit.
				slugs, slugErr := s.ListConnectionWorkspaceSlugs(reqID)
				if slugErr != nil {
					return nil, fmt.Errorf("list staged workspaces %s: %w", reqID, slugErr)
				}
				conn.AllowedWorkspaces = slugs
			} else {
				conn.AllowedWorkspaces = access.WorkspaceSlugs
			}
			// Pull the rest of the connection metadata (name + the
			// other two scope flags) with one PK lookup. Same cost
			// shape as GetOAuthConnectionAccess; combined this is two
			// indexed reads per chain, which on user-scale lists
			// (~handful) is well under the audit-log enrichment cost.
			meta, metaErr := s.GetOAuthConnection(reqID)
			if metaErr != nil {
				// HasConnection said the row exists; a follow-up
				// lookup failing is a genuine store error — same
				// rationale as the access lookup above.
				return nil, fmt.Errorf("hydrate connection metadata %s: %w", reqID, metaErr)
			}
			conn.Name = meta.Name
			conn.MayCreateWorkspaces = meta.MayCreateWorkspaces
			conn.IncludeFutureWorkspaces = meta.IncludeFutureWorkspaces
		} else {
			// No oauth_connections row — treat as the legacy "any
			// workspace, default-on flags" shape so existing UI keeps
			// rendering. ListUserOAuthConnections returns the same
			// nil AllowedWorkspaces semantic the pre-TASK-1522 read
			// path used for pre-TASK-952 / wildcard tokens, so the
			// DTO + frontend behave identically.
			conn.AllCurrentWorkspaces = true
			conn.MayCreateWorkspaces = true
			conn.IncludeFutureWorkspaces = true
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

// parseAllowedWorkspacesFromSession was retired in TASK-1522 when the
// connected-apps read path switched to consuming oauth_connections +
// oauth_connection_workspaces instead of parsing session.Extra strings.
// The session.Extra producer side still exists (oauth.Session.
// SetAllowedWorkspaces) until Phase C2 retires it from
// /authorize/decide; the dual-read introspection gate in
// internal/server/middleware_mcp_auth.go consults both shapes during
// transition. After the soak period (tracked in PLAN-1519 follow-ups)
// the producer + dual-read can come out together. See
// internal/store/oauth_connections_backfill.go for the migration that
// seeded the new tables from the existing session.Extra payloads.
