package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// ErrClaimNotEligible: the token is invalid, spent or expired, or its account
// is already verified, disabled or gone. Nothing was changed.
var ErrClaimNotEligible = errors.New("account claim: not eligible")

// ErrClaimNeedsSupport: the account has a billing customer, which a claim
// must not transfer or cancel silently (TASK-3351 ruling (4)). Nothing was
// changed; the token is still unspent.
var ErrClaimNeedsSupport = errors.New("account claim: needs support")

// AccountClaim reports what a claim changed.
type AccountClaim struct {
	UserID string
	// Epoch is the account's credential_epoch after the claim, for minting
	// the claimant's first session.
	Epoch int64
	// StrippedWorkspaces names the workspaces the account was a member of but
	// did not own, from which its membership, grants, watches and tabs were
	// removed.
	StrippedWorkspaces []string
	// DeletedWorkspaces names the workspaces the account owned, now
	// soft-deleted: they were made by whoever held the account before.
	DeletedWorkspaces []string
}

// ClaimAccountByVerification hands a never-verified account to whoever holds
// its mailbox, proven by the email-verification token (BUG-3382). The token
// was mailed to the address, so the clicker owns the mailbox; the account's
// current credentials may belong to someone who registered the address
// first (a squatter). One transaction:
//
//  1. spend the token (same first step, and lock order, as
//     ConsumeEmailVerification);
//  2. lock the account and require it unverified, live and with no billing
//     customer;
//  3. reset every credential: an unusable password, no 2FA, every session,
//     PAT, OAuth grant, CLI handoff, reset and verification token gone, and
//     credential_epoch bumped so no sign-in checked before this can mint;
//  4. mark the address verified;
//  5. remove the account from workspaces it does not own (membership,
//     grants, watches, tabs), and soft-delete the ones it owns (TASK-3351
//     ruling (3)): what the previous holder joined or built is not the
//     mailbox owner's.
//
// The caller then gives the claimant a way to set a password and kicks the
// account's live connections.
func (s *Store) ClaimAccountByVerification(token string) (*AccountClaim, error) {
	sum := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(sum[:])

	// An unusable password: a random secret nobody keeps.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("account claim: random password: %w", err)
	}
	unusable, err := bcrypt.GenerateFromPassword([]byte(hex.EncodeToString(raw)), bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("account claim: hash password: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("account claim: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ts := now()

	var userID string
	err = tx.QueryRow(s.q(`
		UPDATE email_verification_tokens SET used_at = ?
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?
		RETURNING user_id`), ts, tokenHash, ts).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrClaimNotEligible
	}
	if err != nil {
		return nil, fmt.Errorf("account claim: spend token: %w", err)
	}

	lockQ := `SELECT COALESCE(email_verified_at, ''), CASE WHEN disabled_at IS NULL THEN 0 ELSE 1 END, COALESCE(stripe_customer_id, '') FROM users WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
		lockQ += ` FOR NO KEY UPDATE`
	}
	var verifiedAt, customer string
	var disabled int
	err = tx.QueryRow(s.q(lockQ), userID).Scan(&verifiedAt, &disabled, &customer)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrClaimNotEligible
	}
	if err != nil {
		return nil, fmt.Errorf("account claim: lock user: %w", err)
	}
	if verifiedAt != "" || disabled == 1 {
		return nil, ErrClaimNotEligible
	}
	if customer != "" {
		return nil, ErrClaimNeedsSupport
	}

	claim := &AccountClaim{UserID: userID}
	names := func(query string) ([]string, error) {
		rows, err := tx.Query(s.q(query), userID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return nil, err
			}
			out = append(out, n)
		}
		return out, rows.Err()
	}
	if claim.StrippedWorkspaces, err = names(`
		SELECT w.name FROM workspace_members m JOIN workspaces w ON w.id = m.workspace_id
		WHERE m.user_id = ? AND w.owner_id <> m.user_id AND w.deleted_at IS NULL ORDER BY w.name`); err != nil {
		return nil, fmt.Errorf("account claim: list joined workspaces: %w", err)
	}
	if claim.DeletedWorkspaces, err = names(`
		SELECT name FROM workspaces WHERE owner_id = ? AND deleted_at IS NULL ORDER BY name`); err != nil {
		return nil, fmt.Errorf("account claim: list owned workspaces: %w", err)
	}

	notOwned := `(SELECT id FROM workspaces WHERE owner_id <> ?)`
	stmts := []struct {
		what, query string
		args        []any
	}{
		{"reset credentials", `UPDATE users SET password_hash = ?, password_set = ?, totp_secret = '', totp_enabled = ?, recovery_codes = '',
			email_verified_at = ?, updated_at = ?, credential_epoch = credential_epoch + 1 WHERE id = ?`,
			[]any{string(unusable), s.dialect.BoolToInt(false), s.dialect.BoolToInt(false), ts, ts, userID}},
		{"delete sessions", `DELETE FROM sessions WHERE user_id = ?`, []any{userID}},
		{"delete api tokens", `DELETE FROM api_tokens WHERE user_id = ?`, []any{userID}},
		{"revoke oauth access tokens", `UPDATE oauth_access_tokens SET active = ? WHERE subject = ?`, []any{s.dialect.BoolToInt(false), userID}},
		{"revoke oauth refresh tokens", `UPDATE oauth_refresh_tokens SET active = ? WHERE subject = ?`, []any{s.dialect.BoolToInt(false), userID}},
		{"revoke oauth authorization codes", `UPDATE oauth_authorization_codes SET active = ? WHERE request_id IN (SELECT request_id FROM oauth_connections WHERE user_id = ?)`, []any{s.dialect.BoolToInt(false), userID}},
		{"delete oauth pkce requests", `DELETE FROM oauth_pkce_requests WHERE request_id IN (SELECT request_id FROM oauth_connections WHERE user_id = ?)`, []any{userID}},
		{"delete oauth connections", `DELETE FROM oauth_connections WHERE user_id = ?`, []any{userID}},
		{"delete cli handoffs", `DELETE FROM cli_auth_sessions WHERE user_id = ?`, []any{userID}},
		{"delete reset tokens", `DELETE FROM password_reset_tokens WHERE user_id = ?`, []any{userID}},
		{"delete verification tokens", `DELETE FROM email_verification_tokens WHERE user_id = ?`, []any{userID}},
		{"delete grants elsewhere (collections)", `DELETE FROM collection_grants WHERE user_id = ? AND workspace_id IN ` + notOwned, []any{userID, userID}},
		{"delete grants elsewhere (items)", `DELETE FROM item_grants WHERE user_id = ? AND workspace_id IN ` + notOwned, []any{userID, userID}},
		{"delete watches elsewhere", `DELETE FROM watches WHERE user_id = ? AND workspace_id IN ` + notOwned, []any{userID, userID}},
		{"delete tabs elsewhere", `DELETE FROM user_workspace_tabs WHERE user_id = ? AND workspace_id IN ` + notOwned, []any{userID, userID}},
		{"delete memberships elsewhere", `DELETE FROM workspace_members WHERE user_id = ? AND workspace_id IN ` + notOwned, []any{userID, userID}},
		// Owned workspaces go as account deletion takes them: soft-deleted,
		// with every holder's tab on them (users.go deleteAccountAtomic).
		{"delete tabs of owned workspaces", `DELETE FROM user_workspace_tabs WHERE workspace_id IN (SELECT id FROM workspaces WHERE owner_id = ? AND deleted_at IS NULL)`, []any{userID}},
		{"soft-delete owned workspaces", `UPDATE workspaces SET deleted_at = ?, updated_at = ? WHERE owner_id = ? AND deleted_at IS NULL`, []any{ts, ts, userID}},
	}
	for _, st := range stmts {
		if _, err := tx.Exec(s.q(st.query), st.args...); err != nil {
			return nil, fmt.Errorf("account claim: %s: %w", st.what, err)
		}
	}
	if err := tx.QueryRow(s.q(`SELECT credential_epoch FROM users WHERE id = ?`), userID).Scan(&claim.Epoch); err != nil {
		return nil, fmt.Errorf("account claim: read epoch: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("account claim: commit: %w", err)
	}
	return claim, nil
}
