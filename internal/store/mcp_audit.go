package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// MCP audit log store (PLAN-943 TASK-960).
//
// Five public methods:
//
//   - InsertMCPAuditEntry — single-row insert, called from the audit
//     middleware's async writer goroutine.
//   - ListMCPAuditByUser — paginated per-user view used by both
//     /api/v1/connected-apps/{id}/audit (filter by token_ref) and the
//     admin surface (filter by user).
//   - ListMCPAuditByConnection — paginated per-connection view used
//     by the connected-apps detail drilldown.
//   - MCPConnectionStats — bulk last-used + 30-day-count aggregator
//     keyed by (token_kind, token_ref). One query per fetch instead
//     of N.
//   - SweepMCPAuditOlderThan — retention sweep, called from a
//     periodic goroutine in Server.Start. 90-day default per
//     TASK-960's spec.
//
// All methods are dialect-agnostic via s.q — same pattern as the
// rest of internal/store. Timestamps go in/out as time.Time and are
// serialized to RFC3339 in the DB to match every other timestamp
// column in pad (see migrations/048_oauth.sql for the same shape).

// ErrMCPAuditNotFound is returned by per-row lookups when no row
// matches. Currently no callers — listing returns empty slices
// rather than this error — but kept for symmetry with ErrOAuthNotFound
// in case a future single-row accessor lands.
var ErrMCPAuditNotFound = errors.New("mcp_audit: not found")

// InsertMCPAuditEntry persists one audit row. Called from the audit
// middleware's async writer; the caller owns the buffered channel
// + the ordering, so this method is intentionally synchronous.
//
// Validation: UserID + TokenKind + TokenRef + ToolName + RequestID
// are all required (the middleware fills them from the request
// context). Empty fields fail loudly rather than silently writing
// garbage rows that would later confuse forensics.
//
// WorkspaceID and ErrorKind map to NULL when empty so the column
// reflects "not applicable" rather than "" — distinguishable on read
// via *string.
func (s *Store) InsertMCPAuditEntry(in models.MCPAuditEntryInput) error {
	if in.UserID == "" {
		return fmt.Errorf("mcp_audit: user_id required")
	}
	if in.TokenKind == "" {
		return fmt.Errorf("mcp_audit: token_kind required")
	}
	if in.TokenRef == "" {
		return fmt.Errorf("mcp_audit: token_ref required")
	}
	if in.ToolName == "" {
		return fmt.Errorf("mcp_audit: tool_name required")
	}
	if in.RequestID == "" {
		return fmt.Errorf("mcp_audit: request_id required")
	}

	ts := in.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	var workspaceID interface{}
	if in.WorkspaceID != "" {
		workspaceID = in.WorkspaceID
	}
	var errorKind interface{}
	if in.ErrorKind != "" {
		errorKind = in.ErrorKind
	}

	_, err := s.db.Exec(s.q(`
		INSERT INTO mcp_audit_log (
			id, timestamp, user_id, workspace_id,
			token_kind, token_ref, tool_name, args_hash,
			result_status, error_kind, latency_ms, request_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), newID(), ts.UTC().Format(time.RFC3339), in.UserID, workspaceID,
		string(in.TokenKind), in.TokenRef, in.ToolName, in.ArgsHash,
		string(in.ResultStatus), errorKind, in.LatencyMs, in.RequestID)
	if err != nil {
		return fmt.Errorf("insert mcp audit: %w", err)
	}
	return nil
}

// ListMCPAuditByUser returns the user's most recent audit rows in
// reverse-chronological order. limit caps the page; offset paginates.
// Caller must enforce a sensible upper bound on limit (handlers cap
// at 200 — large enough for an "explore the table" UX, small enough
// to keep Postgres scans cheap).
func (s *Store) ListMCPAuditByUser(userID string, limit, offset int) ([]models.MCPAuditEntry, error) {
	if userID == "" {
		return nil, fmt.Errorf("mcp_audit: user_id required")
	}
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(s.q(`
		SELECT id, timestamp, user_id, workspace_id,
		       token_kind, token_ref, tool_name, args_hash,
		       result_status, error_kind, latency_ms, request_id
		FROM mcp_audit_log
		WHERE user_id = ?
		ORDER BY timestamp DESC
		LIMIT ? OFFSET ?
	`), userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query mcp audit by user: %w", err)
	}
	defer rows.Close()
	return scanMCPAuditRows(rows)
}

// ListMCPAuditByConnection returns the audit rows for one OAuth
// connection (or one PAT). Used by the per-connection drilldown the
// connected-apps page exposes via GET /api/v1/connected-apps/{id}/audit.
//
// userID is REQUIRED in addition to the (token_kind, token_ref) pair
// so a malicious caller can't read another user's audit by guessing
// a request_id. The handler verifies ownership first; the store
// enforces it as belt-and-suspenders.
func (s *Store) ListMCPAuditByConnection(userID string, kind models.TokenKind, ref string, limit, offset int) ([]models.MCPAuditEntry, error) {
	if userID == "" || kind == "" || ref == "" {
		return nil, fmt.Errorf("mcp_audit: user_id, token_kind, token_ref required")
	}
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(s.q(`
		SELECT id, timestamp, user_id, workspace_id,
		       token_kind, token_ref, tool_name, args_hash,
		       result_status, error_kind, latency_ms, request_id
		FROM mcp_audit_log
		WHERE user_id = ? AND token_kind = ? AND token_ref = ?
		ORDER BY timestamp DESC
		LIMIT ? OFFSET ?
	`), userID, string(kind), ref, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query mcp audit by connection: %w", err)
	}
	defer rows.Close()
	return scanMCPAuditRows(rows)
}

// ListAllMCPAudit is the admin-only full-table view used by the
// /console/admin/mcp-audit page. limit + offset paginate; caller
// must restrict access at the route level (admin role).
func (s *Store) ListAllMCPAudit(limit, offset int) ([]models.MCPAuditEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(s.q(`
		SELECT id, timestamp, user_id, workspace_id,
		       token_kind, token_ref, tool_name, args_hash,
		       result_status, error_kind, latency_ms, request_id
		FROM mcp_audit_log
		ORDER BY timestamp DESC
		LIMIT ? OFFSET ?
	`), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query mcp audit (all): %w", err)
	}
	defer rows.Close()
	return scanMCPAuditRows(rows)
}

// MCPConnectionStatsForUser returns last-used + 30-day-count keyed
// by (token_kind, token_ref) for every connection the user has any
// audit-log activity for. The connected-apps page calls this once
// per render and joins client-side onto its connection list.
//
// Why a single query rather than per-connection lookups: the page
// renders N connection cards; with a per-connection query the page
// load grows O(N) DB roundtrips. One aggregate query stays O(1)
// regardless of how many connections the user has.
//
// Window: "30 days" is computed at call time as time.Now().Add(-30d)
// — caller doesn't pass a since because the only consumer wants the
// canonical 30-day window from the spec. If a future surface needs
// a different window we'll add an MCPConnectionStatsForUserSince
// variant rather than overloading.
func (s *Store) MCPConnectionStatsForUser(userID string) (map[string]models.MCPConnectionStats, error) {
	if userID == "" {
		return nil, fmt.Errorf("mcp_audit: user_id required")
	}
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)

	// One query that simultaneously computes MAX(timestamp) (all-time
	// last-used) and SUM(timestamp >= cutoff) (30-day call count).
	// The cutoff comparison runs as TEXT lex compare — safe because
	// every timestamp is RFC3339 with the same length + Z suffix.
	rows, err := s.db.Query(s.q(`
		SELECT token_kind, token_ref,
		       MAX(timestamp) AS last_used,
		       SUM(CASE WHEN timestamp >= ? THEN 1 ELSE 0 END) AS calls_30d
		FROM mcp_audit_log
		WHERE user_id = ?
		GROUP BY token_kind, token_ref
	`), cutoff, userID)
	if err != nil {
		return nil, fmt.Errorf("query mcp connection stats: %w", err)
	}
	defer rows.Close()

	out := make(map[string]models.MCPConnectionStats)
	for rows.Next() {
		var (
			kind, ref string
			lastUsed  sql.NullString
			calls30d  int64
		)
		if err := rows.Scan(&kind, &ref, &lastUsed, &calls30d); err != nil {
			return nil, fmt.Errorf("scan mcp connection stats: %w", err)
		}
		stat := models.MCPConnectionStats{
			TokenKind: models.TokenKind(kind),
			TokenRef:  ref,
			Calls30d:  int(calls30d),
		}
		if lastUsed.Valid && lastUsed.String != "" {
			t := parseTime(lastUsed.String)
			stat.LastUsedAt = &t
		}
		out[mcpConnectionStatsKey(models.TokenKind(kind), ref)] = stat
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mcp connection stats: %w", err)
	}
	return out, nil
}

// mcpConnectionStatsKey is the canonical map key for the result of
// MCPConnectionStatsForUser. Exposed via a helper so callers
// constructing lookup keys don't accidentally drift from the
// concatenation format and silently miss stats.
func mcpConnectionStatsKey(kind models.TokenKind, ref string) string {
	return string(kind) + ":" + ref
}

// MCPConnectionStatsKey is the exported wrapper for callers in
// other packages (notably internal/server/handlers_connected_apps.go
// in TASK-954) that need to look up stats by the same key shape.
func MCPConnectionStatsKey(kind models.TokenKind, ref string) string {
	return mcpConnectionStatsKey(kind, ref)
}

// SweepMCPAuditOlderThan deletes audit rows older than cutoff and
// returns the number of rows removed. Called from a periodic
// goroutine in Server.Start (24h tick); 90-day retention per
// TASK-960's spec is enforced by the caller passing the right
// cutoff.
//
// Safe to call concurrently with InsertMCPAuditEntry — SQLite
// serializes writes globally; Postgres uses row-level locks. Either
// way the DELETE doesn't block in-flight inserts because they target
// disjoint rows (yesterday's writes vs. >90d-old reads).
func (s *Store) SweepMCPAuditOlderThan(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(s.q(`DELETE FROM mcp_audit_log WHERE timestamp < ?`),
		cutoff.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("sweep mcp audit: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// scanMCPAuditRows is the shared row-decoder for every list method.
// Pulls out the nullable workspace_id + error_kind into *string,
// parses the timestamp, and produces a slice of MCPAuditEntry.
func scanMCPAuditRows(rows *sql.Rows) ([]models.MCPAuditEntry, error) {
	var out []models.MCPAuditEntry
	for rows.Next() {
		var (
			e            models.MCPAuditEntry
			tsStr, kind  string
			workspaceID  sql.NullString
			errorKind    sql.NullString
			resultStatus string
			latencyMs    int
		)
		err := rows.Scan(&e.ID, &tsStr, &e.UserID, &workspaceID,
			&kind, &e.TokenRef, &e.ToolName, &e.ArgsHash,
			&resultStatus, &errorKind, &latencyMs, &e.RequestID)
		if err != nil {
			return nil, fmt.Errorf("scan mcp audit row: %w", err)
		}
		e.Timestamp = parseTime(tsStr)
		e.TokenKind = models.TokenKind(kind)
		e.ResultStatus = models.MCPAuditResultStatus(resultStatus)
		e.LatencyMs = latencyMs
		if workspaceID.Valid && workspaceID.String != "" {
			ws := workspaceID.String
			e.WorkspaceID = &ws
		}
		if errorKind.Valid && errorKind.String != "" {
			ek := errorKind.String
			e.ErrorKind = &ek
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mcp audit rows: %w", err)
	}
	return out, nil
}
