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
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("oauth_connections: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// No grant for a disabled account (BUG-3349, requireActiveUserTx).
	if err := s.requireActiveUserTx(tx, c.UserID); err != nil {
		return err
	}
	_, err = tx.Exec(s.q(`
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
	return tx.Commit()
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

// ErrConnectionNoWorkspaces reports that LimitConnectionToCurrentWorkspaces
// found no live workspace the user is a member of, so it changed nothing:
// a specific-list connection must name at least one workspace.
var ErrConnectionNoWorkspaces = errors.New("oauth_connections: user is a member of no workspace")

// LimitConnectionToCurrentWorkspaces turns a wildcard connection into a
// specific list of the workspaces the user is a member of right now
// (BUG-3338). It is the honest form of "my current workspaces": the
// wildcard is live and covers workspaces joined later, and the old
// include_future_workspaces checkbox never narrowed it.
//
// One transaction: the flags UPDATE takes the connection row first (and
// its row lock on Postgres), then every live membership is inserted with
// added_by='user', keeping any rows already staged. Both flags are
// cleared, matching the consent screen's "Only specific workspaces".
// It acts only while the wildcard is on: on a connection that is already
// a specific list it changes nothing (a repeated press from a stale tab
// must not add workspaces joined since the first one).
//
// The snapshot is every live workspace the wildcard reaches for the user
// today: workspaces they are a member of, and workspaces they reach only
// as a guest through a collection or item grant.
//
// Returns how many rows it added; ErrOAuthConnectionNotFound when the
// connection is not the user's; ErrConnectionNoWorkspaces, with nothing
// written, when the list would be empty.
func (s *Store) LimitConnectionToCurrentWorkspaces(requestID, userID string) (int, error) {
	if requestID == "" || userID == "" {
		return 0, fmt.Errorf("oauth_connections: request_id and user_id required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(s.q(`
        UPDATE oauth_connections
           SET all_current_workspaces = ?,
               include_future_workspaces = ?,
               updated_at = `+s.dialect.NowRFC3339()+`
         WHERE request_id = ? AND user_id = ? AND all_current_workspaces = ?
    `), s.dialect.BoolToInt(false), s.dialect.BoolToInt(false), requestID, userID, s.dialect.BoolToInt(true))
	if err != nil {
		return 0, fmt.Errorf("oauth_connections: limit to current: flags: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, fmt.Errorf("oauth_connections: limit to current: rows affected: %w", err)
	} else if n == 0 {
		var one int
		err := tx.QueryRow(s.q(`SELECT 1 FROM oauth_connections WHERE request_id = ? AND user_id = ?`), requestID, userID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrOAuthConnectionNotFound
		}
		if err != nil {
			return 0, fmt.Errorf("oauth_connections: limit to current: lookup: %w", err)
		}
		return 0, nil // already a specific list
	}

	res, err = tx.Exec(s.q(s.snapshotCurrentWorkspacesSQL()), requestID, AddedByUser, userID, userID, userID)
	if err != nil {
		return 0, fmt.Errorf("oauth_connections: limit to current: insert: %w", err)
	}
	added, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("oauth_connections: limit to current: rows affected: %w", err)
	}

	var total int
	if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM oauth_connection_workspaces WHERE request_id = ?`), requestID).Scan(&total); err != nil {
		return 0, fmt.Errorf("oauth_connections: limit to current: count: %w", err)
	}
	if total == 0 {
		return 0, ErrConnectionNoWorkspaces
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(added), nil
}

// snapshotCurrentWorkspacesSQL inserts, for one connection, a row per
// live workspace the wildcard reaches for a user today: membership, or a
// LIVE collection or item grant (the filter UserHasGrantsInWorkspace uses:
// a grant on a soft-deleted item or collection confers nothing, so it must
// not put its workspace in the list either). Args: request_id, added_by,
// then the user id three times. Shared by the limit action and the startup narrowing so
// the two can never snapshot different sets (BUG-3338).
func (s *Store) snapshotCurrentWorkspacesSQL() string {
	sel := `
        SELECT ?, w.id, ?
          FROM workspaces w
         WHERE w.deleted_at IS NULL
           AND (w.id IN (SELECT workspace_id FROM workspace_members WHERE user_id = ?)
                OR w.id IN (SELECT cg.workspace_id FROM collection_grants cg
                              JOIN collections c ON c.id = cg.collection_id
                             WHERE cg.user_id = ? AND c.deleted_at IS NULL)
                OR w.id IN (SELECT ig.workspace_id FROM item_grants ig
                              JOIN items i ON i.id = ig.item_id
                              JOIN collections c ON c.id = i.collection_id
                             WHERE ig.user_id = ? AND i.deleted_at IS NULL AND c.deleted_at IS NULL))`
	if s.dialect.Driver() == DriverPostgres {
		return `INSERT INTO oauth_connection_workspaces (request_id, workspace_id, added_by)` + sel +
			` ON CONFLICT (request_id, workspace_id) DO NOTHING`
	}
	return `INSERT OR IGNORE INTO oauth_connection_workspaces (request_id, workspace_id, added_by)` + sel
}

// ErrLastConnectionWorkspace reports that a change would leave a
// specific-list connection with no workspace, so it was refused.
var ErrLastConnectionWorkspace = errors.New("oauth_connections: would leave the connection with no workspace")

// lockConnectionTx takes the connection row's write lock inside tx (a
// no-op UPDATE: a row lock on Postgres, the database write lock on
// SQLite), so a guard read after it cannot be invalidated by a concurrent
// flags change, limit or removal before tx commits (BUG-3338).
func (s *Store) lockConnectionTx(tx *sql.Tx, requestID string) error {
	res, err := tx.Exec(s.q(`UPDATE oauth_connections SET updated_at = updated_at WHERE request_id = ?`), requestID)
	if err != nil {
		return fmt.Errorf("oauth_connections: lock: %w", err)
	}
	return assertRowAffected(res, ErrOAuthConnectionNotFound)
}

// RemoveConnectionWorkspaceUnlessLast deletes one allow-list row, unless
// the connection is a specific list and that row is its last one, in
// which case it returns ErrLastConnectionWorkspace and changes nothing.
// The check and the delete hold the connection's lock, so a concurrent
// limit or flags change cannot slip between them.
func (s *Store) RemoveConnectionWorkspaceUnlessLast(requestID, workspaceID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.lockConnectionTx(tx, requestID); err != nil {
		return err
	}
	var allCurrent interface{}
	if err := tx.QueryRow(s.q(`SELECT all_current_workspaces FROM oauth_connections WHERE request_id = ?`), requestID).Scan(&allCurrent); err != nil {
		return fmt.Errorf("oauth_connections: remove: read flags: %w", err)
	}
	if !scanBool(allCurrent) {
		var inList, total int
		if err := tx.QueryRow(s.q(`
            SELECT COALESCE(SUM(CASE WHEN workspace_id = ? THEN 1 ELSE 0 END), 0), COUNT(*)
              FROM oauth_connection_workspaces WHERE request_id = ?`), workspaceID, requestID).Scan(&inList, &total); err != nil {
			return fmt.Errorf("oauth_connections: remove: count: %w", err)
		}
		if inList > 0 && total <= 1 {
			return ErrLastConnectionWorkspace
		}
	}
	if _, err := tx.Exec(s.q(`DELETE FROM oauth_connection_workspaces WHERE request_id = ? AND workspace_id = ?`), requestID, workspaceID); err != nil {
		return fmt.Errorf("oauth_connections: remove workspace: %w", err)
	}
	return tx.Commit()
}

// SetScopeFlagsGuarded is SetScopeFlags with the empty-list rule checked
// under the connection's lock: turning the wildcard off is refused with
// ErrLastConnectionWorkspace when the list is empty, so a concurrent
// removal cannot empty it between the check and the write.
func (s *Store) SetScopeFlagsGuarded(requestID string, mayCreate, allCurrent, includeFuture bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.lockConnectionTx(tx, requestID); err != nil {
		return err
	}
	if !allCurrent {
		var total int
		if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM oauth_connection_workspaces WHERE request_id = ?`), requestID).Scan(&total); err != nil {
			return fmt.Errorf("oauth_connections: set flags: count: %w", err)
		}
		if total == 0 {
			return ErrLastConnectionWorkspace
		}
	}
	if _, err := tx.Exec(s.q(`
        UPDATE oauth_connections
           SET may_create_workspaces = ?,
               all_current_workspaces = ?,
               include_future_workspaces = ?,
               updated_at = `+s.dialect.NowRFC3339()+`
         WHERE request_id = ?`),
		s.dialect.BoolToInt(mayCreate), s.dialect.BoolToInt(allCurrent), s.dialect.BoolToInt(includeFuture), requestID); err != nil {
		return fmt.Errorf("oauth_connections: set flags: %w", err)
	}
	return tx.Commit()
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

// AddCreatedWorkspaceIfPermitted inserts a (request_id, workspace_id) row
// with added_by='agent-create', but ONLY while the connection still carries
// may_create_workspaces. The check and the insert are one statement, so the
// grant the decision reads is the grant in force when the row is written
// (BUG-2792). A connection that is missing or has the flag off is a silent
// no-op, as is a pair that is already present.
//
// Why one statement rather than GetOAuthConnection followed by
// AddConnectionWorkspace: those are two unconditional statements, and a
// revocation (SetScopeFlags) committing between them used to add the
// workspace to a connection whose creation power had just been withdrawn.
//
// SQLite: the statement runs under the database write lock, so no
// SetScopeFlags can commit between its read and its write.
//
// Postgres needs FOR SHARE on the connection row, and the single statement
// alone is NOT enough. Under READ COMMITTED an INSERT ... SELECT decides from
// the snapshot taken at statement start, and the only lock it takes on the
// parent is the foreign key's FOR KEY SHARE, which does not conflict with
// SetScopeFlags' non-key UPDATE. A revocation can therefore commit after the
// snapshot and before the insert commits, and the row still lands.
// FOR SHARE conflicts with that UPDATE, which gives two orderings, both
// correct:
//   - A revocation that holds the row first makes this statement wait. The
//     WHERE clause is then re-evaluated against the committed row, which no
//     longer matches, so nothing is inserted.
//   - This statement holds the row first, so the revocation waits for it to
//     commit. The row exists before the revocation returns, and the user
//     removes it from the same page.
//
// Both orderings are pinned in oauth_connections_autoadd_test.go.
func (s *Store) AddCreatedWorkspaceIfPermitted(requestID, workspaceID string) error {
	if requestID == "" || workspaceID == "" {
		return fmt.Errorf("oauth_connections: request_id and workspace_id required")
	}
	var stmt string
	switch s.dialect.Driver() {
	case DriverPostgres:
		stmt = `
            INSERT INTO oauth_connection_workspaces (request_id, workspace_id, added_by)
            (SELECT c.request_id, ?, ?
               FROM oauth_connections c
              WHERE c.request_id = ? AND c.may_create_workspaces = ?
                FOR SHARE)
            ON CONFLICT (request_id, workspace_id) DO NOTHING
        `
	default:
		stmt = `
            INSERT OR IGNORE INTO oauth_connection_workspaces (request_id, workspace_id, added_by)
            SELECT c.request_id, ?, ?
              FROM oauth_connections c
             WHERE c.request_id = ? AND c.may_create_workspaces = ?
        `
	}
	if _, err := s.db.Exec(s.q(stmt),
		workspaceID, AddedByAgentCreate, requestID, s.dialect.BoolToInt(true),
	); err != nil {
		return fmt.Errorf("oauth_connections: add created workspace: %w", err)
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

// ConnectionWorkspaceCount returns the raw row count from
// oauth_connection_workspaces for a given request_id — regardless of
// the parent connection's all_current_workspaces flag.
//
// Why a separate method when GetOAuthConnectionAccess already exists:
// GetOAuthConnectionAccess intentionally short-circuits when
// all_current_workspaces=true and returns an empty WorkspaceSlugs
// list (the wildcard skips the join). That's correct for the
// introspection hot path — when wildcard is on, slugs are irrelevant
// — but the connections-page mutation UI needs to know whether the
// join table actually has rows so a wildcard→specific toggle can
// verify the user has pre-staged at least one workspace. Codex
// review #585 round 1 caught the bug where the flags handler called
// GetOAuthConnectionAccess on a wildcard connection and always saw
// zero slugs, making the toggle impossible.
//
// Returns (count, nil) for any connection state; (0, err) on I/O
// failure. Zero requestID returns (0, nil) — same defensive shape
// as the other request_id-keyed helpers.
func (s *Store) ConnectionWorkspaceCount(requestID string) (int, error) {
	if requestID == "" {
		return 0, nil
	}
	var n int
	err := s.db.QueryRow(s.q(`
        SELECT COUNT(*) FROM oauth_connection_workspaces
         WHERE request_id = ?
    `), requestID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("oauth_connections: count workspaces: %w", err)
	}
	return n, nil
}

// ListConnectionWorkspaceSlugs returns the join table's slug list
// for requestID, regardless of the parent connection's wildcard
// flag. Companion to ConnectionWorkspaceCount — same rationale
// (the connections-page UI needs to render pre-staged workspaces
// while wildcard is on; GetOAuthConnectionAccess's hot-path
// short-circuit hides them). Sorted lexicographically so the UI
// renders chips in a stable order across calls.
//
// Unexported because the only consumer is ListUserOAuthConnections
// in connected_apps.go; if other paths ever need it, hoist to an
// exported method.
func (s *Store) ListConnectionWorkspaceSlugs(requestID string) ([]string, error) {
	rows, err := s.db.Query(s.q(`
        SELECT w.slug
          FROM oauth_connection_workspaces cw
          JOIN workspaces w ON w.id = cw.workspace_id
         WHERE cw.request_id = ?
    `), requestID)
	if err != nil {
		return nil, fmt.Errorf("oauth_connections: list workspace slugs: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0, 4)
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("oauth_connections: scan slug: %w", err)
		}
		if slug != "" {
			out = append(out, slug)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("oauth_connections: rows: %w", err)
	}
	sort.Strings(out)
	return out, nil
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

// IsWorkspaceCoveredForUser reports whether the given workspace is
// already covered by any of the user's active OAuth connections —
// either by the connection's wildcard flag (all_current_workspaces=1)
// or by an explicit row in oauth_connection_workspaces.
//
// "Active" follows the same definition ListUserOAuthConnections uses
// (`internal/store/connected_apps.go`): at least one access OR refresh
// token in the chain still has active=TRUE. Revoked chains are
// invisible here so a dangling oauth_connections row from a revoked
// chain doesn't suppress the claim-code modal.
//
// Used by the claim-code generation endpoint (PLAN-1519 / TASK-1525)
// to power the modal's "smart suppression" UI: if the workspace is
// already covered, the modal replaces the code display with a hint
// that points at /console/connected-apps instead of generating a
// code the user would have nothing to do with.
//
// Returns the matching connection's human-readable Name (may be empty
// — name is user-supplied at /authorize and many existing connections
// don't have one yet). On no match, returns ("", false, nil). Limits
// the underlying query to one row — the modal only needs "is there
// any?" and the first match is sufficient.
func (s *Store) IsWorkspaceCoveredForUser(userID, workspaceID string) (string, bool, error) {
	if userID == "" || workspaceID == "" {
		return "", false, nil
	}
	trueVal := s.dialect.BoolToInt(true)
	var name string
	err := s.db.QueryRow(s.q(`
        SELECT oc.name
          FROM oauth_connections oc
         WHERE oc.user_id = ?
           AND (
             EXISTS (
               SELECT 1 FROM oauth_access_tokens t
                WHERE t.request_id = oc.request_id AND t.active = ?
             )
             OR EXISTS (
               SELECT 1 FROM oauth_refresh_tokens t
                WHERE t.request_id = oc.request_id AND t.active = ?
             )
           )
           AND (
             oc.all_current_workspaces = ?
             OR EXISTS (
               SELECT 1 FROM oauth_connection_workspaces cw
                WHERE cw.request_id = oc.request_id
                  AND cw.workspace_id = ?
             )
           )
         LIMIT 1
    `), userID, trueVal, trueVal, trueVal, workspaceID).Scan(&name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("oauth_connections: coverage check: %w", err)
	}
	return name, true, nil
}

// HasActiveConnectionForUser reports whether the user has ANY active
// OAuth connection at all, regardless of which workspaces it covers.
//
// The claim-code endpoint uses this to disambiguate the two "not
// covered" cases the modal must render differently (IDEA-1517 §4
// follow-up):
//
//   - The user has a connection that just doesn't cover THIS workspace
//     (e.g. a grant scoped to specific workspaces at consent time) →
//     the claim code is exactly the right tool; render it.
//   - The user has NO connection at all → the claim code is inert,
//     because nothing exists to redeem it. Handing over a live-looking
//     code here is the dead end this whole reshuffle set out to kill;
//     the modal steers the user to set up an agent (MCP) first instead.
//
// "Active" matches IsWorkspaceCoveredForUser / ListUserOAuthConnections:
// at least one access OR refresh token in the chain still has
// active=TRUE. The query short-circuits at the first match (LIMIT 1) —
// the caller only needs the boolean.
func (s *Store) HasActiveConnectionForUser(userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}
	trueVal := s.dialect.BoolToInt(true)
	var one int
	err := s.db.QueryRow(s.q(`
        SELECT 1
          FROM oauth_connections oc
         WHERE oc.user_id = ?
           AND (
             EXISTS (
               SELECT 1 FROM oauth_access_tokens t
                WHERE t.request_id = oc.request_id AND t.active = ?
             )
             OR EXISTS (
               SELECT 1 FROM oauth_refresh_tokens t
                WHERE t.request_id = oc.request_id AND t.active = ?
             )
           )
         LIMIT 1
    `), userID, trueVal, trueVal).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("oauth_connections: has-connection check: %w", err)
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
