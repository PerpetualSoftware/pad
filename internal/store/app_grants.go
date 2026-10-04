package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// App grants (SPEC-6 U5b-1, TASK-3399): the delegated grants a person gave
// installed apps. They are not MCP connections, and have no oauth_connections
// row (lead ruling Q1): the install fixes the workspace and the binding the
// issuance barrier wrote carries the kind, epoch and access. The console
// lists them read-only and revokes them, through the binding.

// AppGrant is one delegated grant, as the console shows it.
type AppGrant struct {
	RequestID     string
	InstallID     string
	AppName       string
	Origin        string
	WorkspaceID   string
	WorkspaceName string
	WorkspaceSlug string
	Access        string // "read" or "write", as the person consented
	GrantedAt     time.Time
}

// ErrAppGrantNotFound is a grant that does not exist or is not the user's;
// the two are indistinguishable on purpose.
var ErrAppGrantNotFound = errors.New("app grant not found")

// ListUserAppGrants returns the user's live delegated app grants: a binding
// of kind delegated whose grant still has an active access or refresh token
// held by the user. Newest first.
func (s *Store) ListUserAppGrants(userID string) ([]AppGrant, error) {
	rows, err := s.db.Query(s.q(`
		SELECT b.request_id, b.install_id, i.origin, COALESCE(u.name, ''), b.workspace_id, w.name, w.slug,
		       COALESCE(b.delegated_access, ''), b.created_at
		FROM app_token_bindings b
		JOIN app_installs i ON i.id = b.install_id
		JOIN workspaces w ON w.id = b.workspace_id
		LEFT JOIN users u ON u.id = i.bot_user_id
		WHERE b.auth_kind = 'delegated' AND b.delegated_user_id = ? AND b.revoked_at IS NULL
		  AND (EXISTS (SELECT 1 FROM oauth_access_tokens t WHERE t.request_id = b.request_id AND t.active = ?)
		    OR EXISTS (SELECT 1 FROM oauth_refresh_tokens t WHERE t.request_id = b.request_id AND t.active = ?))
		ORDER BY b.created_at DESC`), userID, true, true)
	if err != nil {
		return nil, fmt.Errorf("list app grants: %w", err)
	}
	defer rows.Close()
	var out []AppGrant
	for rows.Next() {
		var g AppGrant
		var granted string
		if err := rows.Scan(&g.RequestID, &g.InstallID, &g.Origin, &g.AppName, &g.WorkspaceID, &g.WorkspaceName, &g.WorkspaceSlug,
			&g.Access, &granted); err != nil {
			return nil, fmt.Errorf("scan app grant: %w", err)
		}
		g.GrantedAt = parseTime(granted)
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].AppName == "" {
			if bot, err := s.AppPrincipalForInstall(out[i].InstallID); err == nil && bot != nil {
				out[i].AppName = bot.Name
			}
		}
		if out[i].AppName == "" {
			out[i].AppName = out[i].Origin
		}
	}
	return out, nil
}

// IsUserAppGrant reports whether requestID is a delegated app grant the
// user holds (active or not).
func (s *Store) IsUserAppGrant(userID, requestID string) (bool, error) {
	var n int
	err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM app_token_bindings b
		WHERE b.request_id = ? AND b.auth_kind = 'delegated' AND b.delegated_user_id = ?`),
		requestID, userID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check app grant: %w", err)
	}
	return n > 0, nil
}

// RevokeUserAppGrant ends a delegated app grant the user holds, in one
// transaction: its access and refresh tokens are deactivated, an unused
// code or PKCE row is deleted, and the binding is tombstoned (revoked_at),
// which introspection and the issuance barrier both refuse.
// Idempotent; a grant that is not the user's is ErrAppGrantNotFound.
func (s *Store) RevokeUserAppGrant(userID, requestID string) (installID string, err error) {
	ok, err := s.IsUserAppGrant(userID, requestID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrAppGrantNotFound
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRow(s.q(`SELECT install_id FROM app_token_bindings WHERE request_id = ?`), requestID).Scan(&installID); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read app grant: %w", err)
	}
	// The install row lock the issuance barrier takes, taken first (codex
	// U5b-1 r1): a refresh in flight either commits before this revoke, whose
	// updates below then see and deactivate its rows, or reads the tombstone
	// this writes and refuses. On SQLite the single writer orders them.
	if s.dialect.Driver() == DriverPostgres && installID != "" {
		var one int
		if err := tx.QueryRow(s.q(`SELECT 1 FROM app_installs WHERE id = ? FOR UPDATE`), installID).Scan(&one); err != nil &&
			!errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("lock install for app grant revoke: %w", err)
		}
	}
	// Child rows in teardown's order (binding, PKCE, code, refresh, access),
	// after the install lock teardown and issuance also take first.
	// A tombstone, not a delete: a persistence already past its own checks
	// would take a missing binding for a first issuance.
	if _, err := tx.Exec(s.q(`UPDATE app_token_bindings SET revoked_at = ? WHERE request_id = ? AND revoked_at IS NULL`), now(), requestID); err != nil {
		return "", fmt.Errorf("revoke app grant: %w", err)
	}
	for _, q := range []string{
		`DELETE FROM oauth_pkce_requests WHERE request_id = ?`,
		`DELETE FROM oauth_authorization_codes WHERE request_id = ?`,
	} {
		if _, err := tx.Exec(s.q(q), requestID); err != nil {
			return "", fmt.Errorf("revoke app grant: %w", err)
		}
	}
	for _, q := range []string{
		`UPDATE oauth_refresh_tokens SET active = ? WHERE request_id = ?`,
		`UPDATE oauth_access_tokens SET active = ? WHERE request_id = ?`,
	} {
		if _, err := tx.Exec(s.q(q), false, requestID); err != nil {
			return "", fmt.Errorf("revoke app grant: %w", err)
		}
	}
	return installID, tx.Commit()
}

// revokeDelegatedGrantsTx ends the delegated app grants a person holds, in
// one workspace (workspaceID) or in every one (""), inside the caller's
// transaction: their bindings tombstoned, their unexchanged PKCE rows and
// codes deleted, and their tokens deactivated. Called by member removal,
// account disable and account claim, each of which already holds the
// person's users row.
//
// LOCKS (codex U5b-1 r2 and r3). It takes no install lock: the issuance
// barrier share-locks a delegated grant's person BEFORE anything else, so
// holding the person's row already orders every issuance of their grants
// against this. An install lock here deadlocked against owner-account
// deletion, which reaches installs through their bots' foreign key in an
// order of its own. Against install teardown, which holds the install and
// then writes these same rows, both take the child rows in ONE order:
// binding, PKCE, code, refresh, access (DeleteInstallClientTx's), so the two
// cannot wait on each other in a cycle. Both lock their bindings first, in
// request_id order (lockBindingsInOrderTx), because one table order is not
// one row order.
func (s *Store) revokeDelegatedGrantsTx(tx *sql.Tx, userID, workspaceID string) error {
	scope := `SELECT request_id FROM app_token_bindings WHERE auth_kind = 'delegated' AND delegated_user_id = ?`
	args := []any{userID}
	if workspaceID != "" {
		scope += ` AND workspace_id = ?`
		args = append(args, workspaceID)
	}
	with := func(first any) []any { return append([]any{first}, args...) }
	// Every binding of the person in scope, tombstones included, locked in
	// request_id order before any child row, as install teardown does
	// (codex U5b-1 r4).
	where := `auth_kind = 'delegated' AND delegated_user_id = ?`
	if workspaceID != "" {
		where += ` AND workspace_id = ?`
	}
	if err := s.lockBindingsInOrderTx(tx, where, args...); err != nil {
		return fmt.Errorf("revoke delegated grants: %w", err)
	}
	for _, st := range []struct {
		q    string
		args []any
	}{
		{`UPDATE app_token_bindings SET revoked_at = ? WHERE revoked_at IS NULL AND auth_kind = 'delegated' AND delegated_user_id = ?` +
			map[bool]string{true: ` AND workspace_id = ?`, false: ``}[workspaceID != ""], with(now())},
		{`DELETE FROM oauth_pkce_requests WHERE request_id IN (` + scope + `)`, args},
		{`DELETE FROM oauth_authorization_codes WHERE request_id IN (` + scope + `)`, args},
		{`UPDATE oauth_refresh_tokens SET active = ? WHERE request_id IN (` + scope + `)`, with(false)},
		{`UPDATE oauth_access_tokens SET active = ? WHERE request_id IN (` + scope + `)`, with(false)},
	} {
		if _, err := tx.Exec(s.q(st.q), st.args...); err != nil {
			return fmt.Errorf("revoke delegated grants: %w", err)
		}
	}
	return nil
}
