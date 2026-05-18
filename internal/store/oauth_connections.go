package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// OAuth connection-level state (PLAN-1519 / TASK-1520 — Phase A foundation).
//
// IDEA-1517 §2 promotes connection-level state — name, scope flags, mutable
// allow-list — out of `session.Extra["allowed_workspaces"]` (per-token,
// re-minted on every refresh-token rotation) into dedicated tables keyed
// by `request_id` (the OAuth grant chain identifier preserved across
// rotations). See migrations/059_oauth_connections.sql and
// pgmigrations/038_oauth_connections.sql.
//
// Phase A ships the empty tables and the CRUD primitives below. Write-path
// wiring (consent screen → INSERT) lands in Phase C; mutation UI in Phase D.
// The only Phase-A behavioural change is the dual-read gate in
// internal/server/middleware_mcp_auth.go that consults OAuthConnectionAccess
// alongside the legacy session.Extra path.
//
// Update lifecycle: created in /authorize/decide (Phase C); deleted when
// the connection is fully revoked. The per-token `oauth_*_tokens.active`
// flag flips at revocation time but the connection-level row stays around
// only as long as the user wants to manage it — Phase D's connections-page
// "Revoke" button does DELETE FROM oauth_connections WHERE request_id=?
// after the family revocation, which cascades into
// oauth_connection_workspaces via the FK.

// AddedBy values for oauth_connection_workspaces.added_by. Exposed as
// typed constants so call sites in Phase B (agent-create side-effect),
// Phase C (consent screen), Phase D (user edit), and Phase E (claim-code
// redemption) all use the same vocabulary without typos.
const (
	AddedByUser          = "user"
	AddedByAgentCreate   = "agent-create"
	AddedByIncludeFuture = "include-future"
	AddedByClaim         = "claim"
)

// ErrOAuthConnectionNotFound is returned by mutation methods when the
// (request_id) lookup misses. Distinct from ErrOAuthNotFound (which
// covers the underlying token tables) so callers can distinguish
// "connection metadata missing" (caller should fall back to legacy
// session.Extra path) from "no token exists" (auth failure).
var ErrOAuthConnectionNotFound = errors.New("oauth_connections: connection not found")

// OAuthConnection mirrors a single oauth_connections row. Returned by
// GetOAuthConnection and used by Phase D's mutation handlers as the
// payload shape. JSON tags omitted — this type is internal to the
// store/server boundary; the HTTP layer projects onto its own DTO.
type OAuthConnection struct {
	RequestID               string
	UserID                  string
	Name                    string
	MayCreateWorkspaces     bool
	AllCurrentWorkspaces    bool
	IncludeFutureWorkspaces bool
	CreatedAt               string
	UpdatedAt               string
}

// OAuthConnectionAccess is the projected shape the introspection gate
// (internal/server/middleware_mcp_auth.go) needs to decide whether a
// connection-level allow-list applies to the current request.
//
// Three states matter to the caller:
//
//   - HasConnection == false → no row in oauth_connections for this
//     request_id. Pre-Phase-C tokens fall here (write path not yet
//     wired) AND any post-Phase-C tokens that never went through the
//     new consent flow. Caller MUST fall back to the legacy
//     session.Extra allow-list to keep behaviour identical.
//
//   - HasConnection == true && AllCurrentWorkspaces == true → wildcard.
//     Connection covers any workspace the user is a member of; no
//     per-workspace gate to apply.
//
//   - HasConnection == true && AllCurrentWorkspaces == false →
//     WorkspaceSlugs holds the explicit allow-list (slugs derived
//     from oauth_connection_workspaces JOIN workspaces). An empty
//     slice here is fail-closed: connection exists, user explicitly
//     scoped to "specific workspaces," none picked → no workspace
//     allowed.
//
// WorkspaceSlugs is always sorted lexicographically so the value is
// stable across calls (useful for tests and for any future caller that
// wants to cache the projection).
type OAuthConnectionAccess struct {
	HasConnection        bool
	AllCurrentWorkspaces bool
	WorkspaceSlugs       []string
}

// GetOAuthConnection fetches the connection row by request_id. Returns
// ErrOAuthConnectionNotFound if no row exists; any other error is a
// real I/O failure the caller should surface.
//
// Read path only — no mutation. Used by Phase D's UI handlers to render
// the connection edit form. Not on the MCP hot path (the hot path uses
// GetOAuthConnectionAccess, which inlines the join).
func (s *Store) GetOAuthConnection(requestID string) (*OAuthConnection, error) {
	if requestID == "" {
		return nil, fmt.Errorf("oauth_connections: request_id required")
	}
	var c OAuthConnection
	var mayCreate, allCurrent, includeFuture interface{}
	row := s.db.QueryRow(s.q(`
        SELECT request_id, user_id, name,
               may_create_workspaces, all_current_workspaces, include_future_workspaces,
               created_at, updated_at
          FROM oauth_connections
         WHERE request_id = ?
    `), requestID)
	if err := row.Scan(
		&c.RequestID, &c.UserID, &c.Name,
		&mayCreate, &allCurrent, &includeFuture,
		&c.CreatedAt, &c.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOAuthConnectionNotFound
		}
		return nil, fmt.Errorf("oauth_connections: get: %w", err)
	}
	c.MayCreateWorkspaces = scanBool(mayCreate)
	c.AllCurrentWorkspaces = scanBool(allCurrent)
	c.IncludeFutureWorkspaces = scanBool(includeFuture)
	return &c, nil
}

// GetOAuthConnectionAccess is the hot-path projection used by the MCP
// introspection middleware. Runs at most two cheap indexed queries
// (one PK lookup, one indexed scan + tiny join) per /mcp request.
//
// Why not return the full OAuthConnection struct: the middleware only
// needs the access projection — pulling name + flags + timestamps just
// to drop them on the floor would waste row decode on the hot path.
// GetOAuthConnection covers the management-UI side where the full row
// matters.
//
// Performance note: this method is called on every authenticated /mcp
// request once Phase A ships. Both queries are PK / indexed lookups so
// each call is sub-millisecond. The dual-read overhead (vs. the
// pre-TASK-1520 single-source session.Extra read) is bounded by these
// two round-trips; PLAN-1519's Risks section calls for a benchmark
// before/after — see bench_oauth_connections_test.go.
func (s *Store) GetOAuthConnectionAccess(requestID string) (OAuthConnectionAccess, error) {
	if requestID == "" {
		return OAuthConnectionAccess{}, fmt.Errorf("oauth_connections: request_id required")
	}
	var allCurrent interface{}
	err := s.db.QueryRow(s.q(`
        SELECT all_current_workspaces
          FROM oauth_connections
         WHERE request_id = ?
    `), requestID).Scan(&allCurrent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Caller falls back to legacy session.Extra allow-list.
			return OAuthConnectionAccess{HasConnection: false}, nil
		}
		return OAuthConnectionAccess{}, fmt.Errorf("oauth_connections: access lookup: %w", err)
	}
	access := OAuthConnectionAccess{HasConnection: true, AllCurrentWorkspaces: scanBool(allCurrent)}
	if access.AllCurrentWorkspaces {
		// Wildcard — no per-slug gate to compute. Skip the join.
		return access, nil
	}
	rows, err := s.db.Query(s.q(`
        SELECT w.slug
          FROM oauth_connection_workspaces cw
          JOIN workspaces w ON w.id = cw.workspace_id
         WHERE cw.request_id = ?
    `), requestID)
	if err != nil {
		return OAuthConnectionAccess{}, fmt.Errorf("oauth_connections: workspace slugs: %w", err)
	}
	defer rows.Close()
	slugs := make([]string, 0, 4)
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return OAuthConnectionAccess{}, fmt.Errorf("oauth_connections: scan slug: %w", err)
		}
		if slug != "" {
			slugs = append(slugs, slug)
		}
	}
	if err := rows.Err(); err != nil {
		return OAuthConnectionAccess{}, fmt.Errorf("oauth_connections: rows: %w", err)
	}
	sort.Strings(slugs)
	access.WorkspaceSlugs = slugs
	return access, nil
}

// CreateOAuthConnection inserts a new connection row. Idempotent in the
// sense that callers should INSERT once per /authorize/decide; if the
// same request_id arrives twice (shouldn't happen — fosite mints a
// fresh ID per consent), we return an error so the caller can decide
// (Phase C will treat it as a programmer bug and 500). No UPSERT here —
// the mutation methods below cover the post-create edit path.
//
// Scope flags are written verbatim from the caller — the schema-level
// DEFAULT TRUE on each column is unreachable through this code path
// because all three values are always supplied. The Phase C handler is
// responsible for translating the consent UI's two-section radio +
// checkbox interface (IDEA-1517 §2a) into the three Go bool fields
// before calling here; the "default on" semantic lives at the form-
// rendering layer, not in the store. Codex review #581 round 1 caught
// the docstring/behaviour mismatch.
func (s *Store) CreateOAuthConnection(c OAuthConnection) error {
	if c.RequestID == "" {
		return fmt.Errorf("oauth_connections: request_id required")
	}
	if c.UserID == "" {
		return fmt.Errorf("oauth_connections: user_id required")
	}
	_, err := s.db.Exec(s.q(`
        INSERT INTO oauth_connections (
            request_id, user_id, name,
            may_create_workspaces, all_current_workspaces, include_future_workspaces
        ) VALUES (?, ?, ?, ?, ?, ?)
    `),
		c.RequestID, c.UserID, c.Name,
		s.dialect.BoolToInt(c.MayCreateWorkspaces),
		s.dialect.BoolToInt(c.AllCurrentWorkspaces),
		s.dialect.BoolToInt(c.IncludeFutureWorkspaces),
	)
	if err != nil {
		return fmt.Errorf("oauth_connections: insert: %w", err)
	}
	return nil
}

// RenameConnection updates the human-readable name. Touches updated_at.
// Returns ErrOAuthConnectionNotFound if the row doesn't exist.
//
// Trim is the caller's responsibility (the Phase D handler will strip
// whitespace + cap length); the store accepts the value verbatim so
// tests can pump arbitrary content through without re-validating.
func (s *Store) RenameConnection(requestID, name string) error {
	if requestID == "" {
		return fmt.Errorf("oauth_connections: request_id required")
	}
	res, err := s.db.Exec(s.q(`
        UPDATE oauth_connections
           SET name = ?, updated_at = `+s.dialect.NowRFC3339()+`
         WHERE request_id = ?
    `), name, requestID)
	if err != nil {
		return fmt.Errorf("oauth_connections: rename: %w", err)
	}
	return assertRowAffected(res, ErrOAuthConnectionNotFound)
}

// SetScopeFlags updates the three boolean scope flags atomically. All
// three values are written every call — there's no PATCH semantic at
// the store layer; the handler reads-modify-writes if it only wants to
// touch one flag.
//
// Why atomic on all three: avoids interleaving anomalies if two browser
// tabs race the connections page (last-write-wins on the full triple is
// less surprising than per-field interleave).
func (s *Store) SetScopeFlags(requestID string, mayCreate, allCurrent, includeFuture bool) error {
	if requestID == "" {
		return fmt.Errorf("oauth_connections: request_id required")
	}
	res, err := s.db.Exec(s.q(`
        UPDATE oauth_connections
           SET may_create_workspaces = ?,
               all_current_workspaces = ?,
               include_future_workspaces = ?,
               updated_at = `+s.dialect.NowRFC3339()+`
         WHERE request_id = ?
    `),
		s.dialect.BoolToInt(mayCreate),
		s.dialect.BoolToInt(allCurrent),
		s.dialect.BoolToInt(includeFuture),
		requestID,
	)
	if err != nil {
		return fmt.Errorf("oauth_connections: set flags: %w", err)
	}
	return assertRowAffected(res, ErrOAuthConnectionNotFound)
}

// AddConnectionWorkspace inserts a (request_id, workspace_id) row in
// the allow-list join table. Idempotent: re-adding an existing pair is
// a no-op (INSERT OR IGNORE on SQLite, ON CONFLICT DO NOTHING on PG).
// added_by must be one of the AddedBy* constants — empty strings get
// rejected at the caller boundary; the store stores what it's given so
// tests can drive whatever value they want.
//
// Does NOT verify the parent oauth_connections row exists — the FK
// constraint will reject inserts that reference a missing connection.
// We surface the raw error so the caller sees the FK violation clearly
// rather than a confusing "missing parent" abstraction.
func (s *Store) AddConnectionWorkspace(requestID, workspaceID, addedBy string) error {
	if requestID == "" || workspaceID == "" {
		return fmt.Errorf("oauth_connections: request_id and workspace_id required")
	}
	if addedBy == "" {
		addedBy = AddedByUser
	}
	var stmt string
	switch s.dialect.Driver() {
	case DriverPostgres:
		stmt = `
            INSERT INTO oauth_connection_workspaces (request_id, workspace_id, added_by)
            VALUES (?, ?, ?)
            ON CONFLICT (request_id, workspace_id) DO NOTHING
        `
	default:
		stmt = `
            INSERT OR IGNORE INTO oauth_connection_workspaces (request_id, workspace_id, added_by)
            VALUES (?, ?, ?)
        `
	}
	if _, err := s.db.Exec(s.q(stmt), requestID, workspaceID, addedBy); err != nil {
		return fmt.Errorf("oauth_connections: add workspace: %w", err)
	}
	return nil
}

// RemoveConnectionWorkspace deletes one (request_id, workspace_id) row.
// Idempotent: removing a pair that isn't present is a no-op (no error).
// Phase D's UI uses this for the "X" affordance on each chip in the
// allow-list editor.
func (s *Store) RemoveConnectionWorkspace(requestID, workspaceID string) error {
	if requestID == "" || workspaceID == "" {
		return fmt.Errorf("oauth_connections: request_id and workspace_id required")
	}
	_, err := s.db.Exec(s.q(`
        DELETE FROM oauth_connection_workspaces
         WHERE request_id = ? AND workspace_id = ?
    `), requestID, workspaceID)
	if err != nil {
		return fmt.Errorf("oauth_connections: remove workspace: %w", err)
	}
	return nil
}

// IsConnectionWorkspaceAllowed reports whether workspace_id appears in
// the connection's allow-list. Used internally by GetOAuthConnectionAccess
// when callers want a single-pair check rather than the slug projection
// (e.g. a future code path that already knows the workspace ID).
//
// Returns (false, nil) when the row is missing — the caller distinguishes
// "not allowed" from "lookup failed" via the error.
func (s *Store) IsConnectionWorkspaceAllowed(requestID, workspaceID string) (bool, error) {
	if requestID == "" || workspaceID == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRow(s.q(`
        SELECT 1
          FROM oauth_connection_workspaces
         WHERE request_id = ? AND workspace_id = ?
    `), requestID, workspaceID).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("oauth_connections: is allowed: %w", err)
	}
	return true, nil
}

// DeleteOAuthConnection removes the row + cascades to
// oauth_connection_workspaces. Phase D's "Revoke" handler calls this
// after RevokeRefreshTokenFamily + RevokeAccessTokenFamily so the
// connection-level state doesn't linger past the token revocation.
//
// Idempotent: removing a non-existent connection is a no-op.
func (s *Store) DeleteOAuthConnection(requestID string) error {
	if requestID == "" {
		return fmt.Errorf("oauth_connections: request_id required")
	}
	if _, err := s.db.Exec(s.q(`
        DELETE FROM oauth_connections WHERE request_id = ?
    `), requestID); err != nil {
		return fmt.Errorf("oauth_connections: delete: %w", err)
	}
	return nil
}

// ---- helpers ----

// scanBool normalizes the dialect-specific bool encoding (SQLite INTEGER
// 0/1; Postgres BOOLEAN true/false) into a Go bool. Mirrors the pattern
// used in connected_apps.go for the active flag.
func scanBool(v interface{}) bool {
	switch t := v.(type) {
	case bool:
		return t
	case int64:
		return t != 0
	case int:
		return t != 0
	case []byte:
		s := strings.ToLower(strings.TrimSpace(string(t)))
		return s == "1" || s == "true" || s == "t"
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "1" || s == "true" || s == "t"
	}
	return false
}

// assertRowAffected returns notFound if the statement didn't touch any
// rows, the underlying RowsAffected error if the driver can't report,
// and nil otherwise. Used by UPDATE methods that want a clean
// "no such connection" signal without a separate SELECT.
func assertRowAffected(res sql.Result, notFound error) error {
	n, err := res.RowsAffected()
	if err != nil {
		// Both SQLite and Postgres drivers always report RowsAffected
		// successfully for UPDATE — this branch is defensive.
		return fmt.Errorf("oauth_connections: rows affected: %w", err)
	}
	if n == 0 {
		return notFound
	}
	return nil
}
