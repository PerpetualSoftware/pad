package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
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
	return claimWithRetry(func() (*AccountClaim, error) {
		return claimRetryingDeadlocks(s, func() (*AccountClaim, error) { return s.claimAccountByVerificationOnce(token) })
	})
}

func (s *Store) claimAccountByVerificationOnce(token string) (*AccountClaim, error) {
	sum := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(sum[:])
	unusable, err := unusablePasswordHash()
	if err != nil {
		return nil, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("account claim: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ts := now()

	// Lock order: the account, then its tokens (see ConsumeEmailVerification).
	// Find the token's account without a lock; the core locks the account
	// and then spends the token.
	var userID string
	err = tx.QueryRow(s.q(`
		SELECT user_id FROM email_verification_tokens
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`), tokenHash, ts).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrClaimNotEligible
	}
	if err != nil {
		return nil, fmt.Errorf("account claim: find token: %w", err)
	}
	spend := func(tx *sql.Tx) error {
		// The clock is read again: the account lock may have been waited
		// for, and the token may have expired meanwhile.
		at := now()
		var spentBy string
		err := tx.QueryRow(s.q(`
			UPDATE email_verification_tokens SET used_at = ?
			WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?
			RETURNING user_id`), at, tokenHash, at).Scan(&spentBy)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && spentBy != userID) {
			return ErrClaimNotEligible
		}
		if err != nil {
			return fmt.Errorf("account claim: spend token: %w", err)
		}
		return nil
	}
	claim, err := s.claimAccountTx(tx, userID, unusable, ts, "", spend)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("account claim: commit: %w", err)
	}
	return claim, nil
}

// ClaimAccountByProvider is ClaimAccountByVerification for a sign-in
// provider that vouched for the address (TASK-3351): the provider's verified
// assertion stands in for the mailed token. The same core, the same rules:
// only a live, never-verified account with no billing customer. The
// claimant's provider is linked, and its account `subject` bound when given,
// in the SAME transaction: a subject bound to another account
// (ErrOAuthSubjectMismatch) rolls the whole claim back, so a refused sign-in
// changes nothing.
//
// name is the provider's display name for the claimant, used to reset the
// account's name and username; empty falls back to the address.
func (s *Store) ClaimAccountByProvider(userID, provider, subject, name string) (*AccountClaim, error) {
	return claimWithRetry(func() (*AccountClaim, error) {
		return claimRetryingDeadlocks(s, func() (*AccountClaim, error) {
			return s.claimAccountByProviderOnce(userID, provider, subject, name)
		})
	})
}

func (s *Store) claimAccountByProviderOnce(userID, provider, subject, name string) (*AccountClaim, error) {
	unusable, err := unusablePasswordHash()
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("account claim: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Lock order: the account, then its tokens, as everywhere (see
	// ConsumeEmailVerification); the core locks the account first.
	claim, err := s.claimAccountTx(tx, userID, unusable, now(), name, nil)
	if err != nil {
		return nil, err
	}
	// The claim cleared every provider; this one is the claimant's.
	if subject != "" {
		if err := s.bindOAuthIdentityTx(tx, userID, provider, subject); err != nil {
			return nil, err
		}
	}
	if err := s.writeUserProvidersTx(tx, userID, []string{provider}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("account claim: commit: %w", err)
	}
	return claim, nil
}

// claimAfterUserLockHook, when set by a test, runs once the claim holds the
// account lock, which is where a concurrent verification interleaves.
var claimAfterUserLockHook func()

// SetUsernameValidator sets the rule a generated username must pass. The
// server installs ValidateUsername, which this package cannot import.
func (s *Store) SetUsernameValidator(v func(string) error) { s.usernameValidator = v }

// maxUsernameLen matches the server's ValidateUsername limit.
const maxUsernameLen = 39

// usernameCandidate is base for n == 1, else base-n, with base cut short so
// the result stays within maxUsernameLen.
func usernameCandidate(base string, n int) string {
	if n == 1 {
		return base
	}
	sfx := "-" + strconv.Itoa(n)
	if len(base)+len(sfx) > maxUsernameLen {
		base = strings.TrimRight(base[:maxUsernameLen-len(sfx)], "-")
	}
	return base + sfx
}

// uniqueUsernameTx returns the first of base, base-2, base-3, ... that
// passes the username rules (reserved names included) and that no OTHER
// account holds, read in the claim's transaction. Another account can still
// take it before the claim commits; the unique index then refuses the write
// and the claim is retried (claimWithRetry).
func (s *Store) uniqueUsernameTx(tx *sql.Tx, base, userID string) (string, error) {
	for n := 1; n <= 1000; n++ {
		username := usernameCandidate(base, n)
		if s.usernameValidator != nil && s.usernameValidator(username) != nil {
			continue
		}
		var taken int
		if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM users WHERE username = ? AND id <> ?`), username, userID).Scan(&taken); err != nil {
			return "", fmt.Errorf("account claim: username: %w", err)
		}
		if taken == 0 {
			if claimAfterUsernameHook != nil {
				claimAfterUsernameHook(username)
			}
			return username, nil
		}
	}
	// Nothing derived from the name will do: every candidate fails the rules
	// (a fallback the generator leaves invalid, like "foo--bar") or is held.
	// A random one still lets the claim through.
	for i := 0; i < 20; i++ {
		raw := make([]byte, 4)
		if _, err := rand.Read(raw); err != nil {
			return "", fmt.Errorf("account claim: random username: %w", err)
		}
		username := "user-" + hex.EncodeToString(raw)
		if s.usernameValidator != nil && s.usernameValidator(username) != nil {
			continue
		}
		var taken int
		if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM users WHERE username = ? AND id <> ?`), username, userID).Scan(&taken); err != nil {
			return "", fmt.Errorf("account claim: username: %w", err)
		}
		if taken == 0 {
			return username, nil
		}
	}
	return "", fmt.Errorf("account claim: no free username for %q", base)
}

// claimAfterUsernameHook, when set by a test, runs once the claim has picked
// a username, which is where another account can take it.
var claimAfterUsernameHook func(username string)

// claimWithRetry runs one claim attempt again when it lost its username to
// another account between choosing and writing it. The attempt's
// transaction rolled back whole (a link claim's token included), so a
// retry starts clean.
// claimRetryingDeadlocks runs one claim attempt under retryOnDeadlock: each
// attempt is one transaction with nothing outside it before its commit
// (TASK-3399).
func claimRetryingDeadlocks(s *Store, once func() (*AccountClaim, error)) (*AccountClaim, error) {
	var claim *AccountClaim
	err := s.retryOnDeadlock("account_claim", func() error {
		var err error
		claim, err = once()
		return err
	})
	return claim, err
}

func claimWithRetry(attempt func() (*AccountClaim, error)) (*AccountClaim, error) {
	var err error
	for i := 0; i < 3; i++ {
		var claim *AccountClaim
		claim, err = attempt()
		if err == nil || !isUniqueViolation(err) {
			return claim, err
		}
	}
	return nil, err
}

// unusablePasswordHash is a random secret nobody keeps.
func unusablePasswordHash() ([]byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("account claim: random password: %w", err)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(hex.EncodeToString(raw)), bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("account claim: hash password: %w", err)
	}
	return h, nil
}

// claimAccountTx is the claim itself, inside the caller's transaction: lock
// the account, check it is claimable, reset every credential, strip access
// elsewhere and soft-delete what it owns.
//
// spend, when given, runs once the account is locked and before anything
// is checked or changed: the link claim spends its token there, so the
// account is always locked before its tokens.
//
// The registrant's display name and username are replaced too (TASK-3351):
// either can impersonate ("support", a staff name), and nothing live depends
// on the username once the account's own workspaces are deleted. The name
// becomes claimantName, or the address's local part when that is empty; the
// username is generated from it and kept unique.
func (s *Store) claimAccountTx(tx *sql.Tx, userID string, unusable []byte, ts, claimantName string, spend func(*sql.Tx) error) (*AccountClaim, error) {
	lockQ := `SELECT COALESCE(email_verified_at, ''), CASE WHEN disabled_at IS NULL THEN 0 ELSE 1 END, COALESCE(stripe_customer_id, ''), email, kind FROM users WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
		lockQ += ` FOR NO KEY UPDATE`
	}
	var verifiedAt, customer, email, kind string
	var disabled int
	err := tx.QueryRow(s.q(lockQ), userID).Scan(&verifiedAt, &disabled, &customer, &email, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrClaimNotEligible
	}
	if err != nil {
		return nil, fmt.Errorf("account claim: lock user: %w", err)
	}
	if claimAfterUserLockHook != nil {
		claimAfterUserLockHook()
	}
	if spend != nil {
		if err := spend(tx); err != nil {
			return nil, err
		}
	}
	// A bot's address is never verified, so without the kind it would be
	// exactly the account a claim takes (TASK-3392). Same answer as any
	// other ineligible account.
	if verifiedAt != "" || disabled == 1 || kind == models.UserKindApp {
		return nil, ErrClaimNotEligible
	}
	if customer != "" {
		return nil, ErrClaimNeedsSupport
	}

	name := strings.TrimSpace(claimantName)
	if name == "" {
		name = strings.Split(email, "@")[0]
	}
	username, err := s.uniqueUsernameTx(tx, GenerateUsername(name, email), userID)
	if err != nil {
		return nil, err
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
			email_verified_at = ?, updated_at = ?, credential_epoch = credential_epoch + 1, name = ?, username = ? WHERE id = ?`,
			[]any{string(unusable), s.dialect.BoolToInt(false), s.dialect.BoolToInt(false), ts, ts, name, username, userID}},
		// Sign-in providers linked before the claim were linked by someone
		// else (TASK-3351); the claimant links their own.
		{"unlink providers", `UPDATE users SET oauth_providers = '' WHERE id = ?`, []any{userID}},
		{"delete provider bindings", `DELETE FROM user_oauth_identities WHERE user_id = ?`, []any{userID}},
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
	for i, st := range stmts {
		if _, err := tx.Exec(s.q(st.query), st.args...); err != nil {
			return nil, fmt.Errorf("account claim: %s: %w", st.what, err)
		}
		// After the users row (the first statement), before any token row:
		// the claimed account's delegated app grants (TASK-3399).
		if i == 0 {
			if err := s.revokeDelegatedGrantsTx(tx, userID, ""); err != nil {
				return nil, fmt.Errorf("account claim: %w", err)
			}
		}
	}
	if err := tx.QueryRow(s.q(`SELECT credential_epoch FROM users WHERE id = ?`), userID).Scan(&claim.Epoch); err != nil {
		return nil, fmt.Errorf("account claim: read epoch: %w", err)
	}
	return claim, nil
}
