package store

import "fmt"

// Counts for the console's MCP readiness panel (PLAN-2310 DR-7). Turning
// MCP off revokes nothing; these are what would start working again if it
// were turned back on.

// CountLiveOAuthConnections counts grant chains with an active access or
// refresh token across the instance. A chain is a request_id, the unit the
// Connected Apps page lists (ListUserOAuthConnections walks both tables the
// same way), so the panel's number matches what users see there.
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
// It applies the same two rules the request path does, in the same way:
// MCPBearerAuth refuses a token with no user_id (a legacy workspace token,
// middleware_mcp_auth.go), and ValidateToken refuses one whose parsed
// expires_at is before now (api_tokens.go). Expiry is compared as a time
// here too, not as a string in SQL, so the two cannot disagree about a
// stored format. Revoking a token deletes its row.
func (s *Store) CountMCPUsablePATs() (int, error) {
	rows, err := s.db.Query(s.q(`
		SELECT expires_at FROM api_tokens
		WHERE user_id IS NOT NULL AND user_id <> ''`))
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
