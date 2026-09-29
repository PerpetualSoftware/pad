package store

import "fmt"

// Counts for the console's MCP readiness panel (PLAN-2310 DR-7). Turning
// MCP off revokes nothing; these are what would start working again if it
// were turned back on.

// CountLiveOAuthConnections counts grant chains with an active access or
// refresh token across the instance. A chain is a request_id, the unit the
// Connected Apps page lists (ListUserOAuthConnections walks both tables the
// same way), so the panel's number matches what users see there.
//
// It is an upper bound on what would resume, not an exact count. `active`
// is cleared on revocation, not on expiry, and nothing sweeps expired rows
// (BUG-3301), so a chain whose refresh token has expired is still counted,
// exactly as the Connected Apps page still lists it. The count follows the
// page's definition deliberately, so the two agree. BUG-3301's sweep fixes
// both at once; an expiry filter here alone would make them disagree.
func (s *Store) CountLiveOAuthConnections() (int, error) {
	var n int
	err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM (
			SELECT request_id FROM oauth_access_tokens WHERE active = ?
			UNION
			SELECT request_id FROM oauth_refresh_tokens WHERE active = ?
		) chains`), true, true).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count live oauth connections: %w", err)
	}
	return n, nil
}

// CountMCPUsablePATs counts personal access tokens that /mcp would accept.
// It applies the same three rules the request path does, in the same way:
// MCPBearerAuth refuses a token with no user_id (a legacy workspace token)
// and one whose user row no longer exists (middleware_mcp_auth.go), and
// ValidateToken refuses one whose parsed expires_at is before now
// (api_tokens.go). The user join is defensive: api_tokens.user_id carries a
// foreign key (migration 068), so no such row can exist today, but the
// count must not depend on that to agree with /mcp. Expiry is compared as a time
// here too, not as a string in SQL, so the two cannot disagree about a
// stored format. Revoking a token deletes its row.
func (s *Store) CountMCPUsablePATs() (int, error) {
	rows, err := s.db.Query(s.q(`
		SELECT t.expires_at FROM api_tokens t
		JOIN users u ON u.id = t.user_id
		WHERE t.user_id IS NOT NULL AND t.user_id <> ''`))
	if err != nil {
		return 0, fmt.Errorf("count mcp-usable personal access tokens: %w", err)
	}
	defer rows.Close()
	cutoff := parseTime(now())
	n := 0
	for rows.Next() {
		var expiresAt *string
		if err := rows.Scan(&expiresAt); err != nil {
			return 0, fmt.Errorf("count mcp-usable personal access tokens: %w", err)
		}
		if t := parseTimePtr(expiresAt); t != nil && t.Before(cutoff) {
			continue
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("count mcp-usable personal access tokens: %w", err)
	}
	return n, nil
}
