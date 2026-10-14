package store

import (
	"strings"
	"testing"
)

// BUG-3668: api_tokens.token_hash had no index, so every token-authenticated
// request scanned the table, and a well-formed invalid token had to exhaust
// the scan. This is the census of the class: every column a request-path
// credential lookup filters on, each checked on the fully migrated schema
// for an index search. The question is whether the column is indexed, so
// each query is the lookup's predicate alone, not the full statement.
//
// SQLite: EXPLAIN QUERY PLAN must not report a SCAN of the table. Postgres
// plans a seq scan on a tiny table even with an index, so the plan is read
// with enable_seqscan off: an index scan is then chosen iff one exists.
func TestBUG3668_CredentialLookupsAreIndexed(t *testing.T) {
	s := testStore(t)
	census := []struct{ lookup, table, query string }{
		{"API token (REST and MCP auth)", "api_tokens", `SELECT id FROM api_tokens WHERE token_hash = ?`},
		{"session cookie", "sessions", `SELECT user_id FROM sessions WHERE token_hash = ?`},
		{"password reset", "password_reset_tokens", `SELECT user_id FROM password_reset_tokens WHERE token_hash = ?`},
		{"email verification and account claim", "email_verification_tokens", `SELECT user_id FROM email_verification_tokens WHERE token_hash = ?`},
		{"share link", "share_links", `SELECT id FROM share_links WHERE token_hash = ?`},
		{"invitation by code hash", "workspace_invitations", `SELECT id FROM workspace_invitations WHERE code_hash = ?`},
		{"invitation by code", "workspace_invitations", `SELECT id FROM workspace_invitations WHERE code = ?`},
		{"CLI auth session", "cli_auth_sessions", `SELECT code FROM cli_auth_sessions WHERE code = ?`},
		{"OAuth authorization code", "oauth_authorization_codes", `SELECT signature FROM oauth_authorization_codes WHERE signature = ?`},
		{"OAuth access token", "oauth_access_tokens", `SELECT signature FROM oauth_access_tokens WHERE signature = ?`},
		{"OAuth refresh token", "oauth_refresh_tokens", `SELECT signature FROM oauth_refresh_tokens WHERE signature = ?`},
		{"OAuth PKCE request", "oauth_pkce_requests", `SELECT signature FROM oauth_pkce_requests WHERE signature = ?`},
	}
	for _, c := range census {
		plan := explainPlan(t, s, c.query, "x")
		scanned := false
		if s.dialect.Driver() == DriverPostgres {
			scanned = strings.Contains(plan, "Seq Scan on "+c.table)
		} else {
			scanned = strings.Contains(plan, "SCAN "+c.table)
		}
		if scanned {
			t.Errorf("%s: the lookup on %s scans the table:\n%s", c.lookup, c.table, plan)
		} else {
			t.Logf("%s: %s", c.lookup, strings.ReplaceAll(strings.TrimSpace(plan), "\n", " | "))
		}
	}
}

func explainPlan(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	prefix := "EXPLAIN QUERY PLAN "
	if s.dialect.Driver() == DriverPostgres {
		if _, err := tx.Exec(`SET LOCAL enable_seqscan = off`); err != nil {
			t.Fatal(err)
		}
		prefix = "EXPLAIN "
	}
	rows, err := tx.Query(s.q(prefix+query), args...)
	if err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var lines []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		// The plan text is the last column on both backends.
		switch v := vals[len(vals)-1].(type) {
		case string:
			lines = append(lines, v)
		case []byte:
			lines = append(lines, string(v))
		}
	}
	return strings.Join(lines, "\n")
}
