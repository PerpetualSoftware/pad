package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrOAuthSubjectMismatch: the provider account asserting the address is not
// the one this pad account is bound to, or that provider account is bound to
// a different pad account (TASK-3351).
var ErrOAuthSubjectMismatch = errors.New("oauth identity: subject mismatch")

// ErrOAuthProviderNotLinked: a binding was asked for a provider the account
// no longer has linked, e.g. a sign-in that read the link before an unlink
// committed (TASK-3351).
var ErrOAuthProviderNotLinked = errors.New("oauth identity: provider not linked")

// lockUserProvidersTx locks the account row and returns its linked
// providers. Every write to an account's providers or bindings goes through
// it, so a link, a bind, an unlink and a claim on one account are serialized
// (TASK-3351): a bind cannot land after the unlink that should have removed
// it. found is false when the account does not exist.
func (s *Store) lockUserProvidersTx(tx *sql.Tx, userID string) (providers []string, found bool, err error) {
	q := `SELECT COALESCE(oauth_providers, '') FROM users WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
		q += ` FOR NO KEY UPDATE`
	}
	var raw string
	err = tx.QueryRow(s.q(q), userID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("lock user providers: %w", err)
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &providers); err != nil {
			return nil, true, fmt.Errorf("lock user providers: decode: %w", err)
		}
	}
	return providers, true, nil
}

func providerListHas(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// writeUserProvidersTx stores the account's linked providers.
func (s *Store) writeUserProvidersTx(tx *sql.Tx, userID string, providers []string) error {
	var val string
	if len(providers) > 0 {
		data, err := json.Marshal(providers)
		if err != nil {
			return fmt.Errorf("write user providers: marshal: %w", err)
		}
		val = string(data)
	}
	if _, err := tx.Exec(s.q(`UPDATE users SET oauth_providers = ?, updated_at = ? WHERE id = ?`), val, now(), userID); err != nil {
		return fmt.Errorf("write user providers: %w", err)
	}
	return nil
}

// bindOAuthIdentityTx binds `subject` to userID for provider, or confirms it
// is already. The caller holds the account lock. A concurrent bind of the
// same subject to another account settles on one row: the losing insert is
// ignored and the read-back decides.
func (s *Store) bindOAuthIdentityTx(tx *sql.Tx, userID, provider, subject string) error {
	var stmt string
	if s.dialect.Driver() == DriverPostgres {
		stmt = `INSERT INTO user_oauth_identities (provider, subject, user_id, created_at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`
	} else {
		stmt = `INSERT OR IGNORE INTO user_oauth_identities (provider, subject, user_id, created_at) VALUES (?, ?, ?, ?)`
	}
	if _, err := tx.Exec(s.q(stmt), provider, subject, userID, now()); err != nil {
		return fmt.Errorf("bind oauth identity: %w", err)
	}
	var owner, bound string
	err := tx.QueryRow(s.q(`
		SELECT
			COALESCE((SELECT user_id FROM user_oauth_identities WHERE provider = ? AND subject = ?), ''),
			COALESCE((SELECT subject FROM user_oauth_identities WHERE user_id = ? AND provider = ?), '')`),
		provider, subject, userID, provider).Scan(&owner, &bound)
	if err != nil {
		return fmt.Errorf("bind oauth identity: read back: %w", err)
	}
	if owner != userID || bound != subject {
		return ErrOAuthSubjectMismatch
	}
	return nil
}

// BindOAuthIdentity records that the provider account `subject` signs in to
// userID, or confirms the record already says so, for a provider the account
// has linked. The first sign-in carrying a subject binds it; every later one
// must match (ErrOAuthSubjectMismatch). A provider unlinked since the caller
// read the account binds nothing (ErrOAuthProviderNotLinked).
func (s *Store) BindOAuthIdentity(userID, provider, subject string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("bind oauth identity: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	providers, found, err := s.lockUserProvidersTx(tx, userID)
	if err != nil {
		return err
	}
	if !found || !providerListHas(providers, provider) {
		return ErrOAuthProviderNotLinked
	}
	if err := s.bindOAuthIdentityTx(tx, userID, provider, subject); err != nil {
		return err
	}
	return tx.Commit()
}

// LinkOAuthProvider links provider to userID and, when subject is given,
// binds that provider account, in one transaction: a subject bound
// elsewhere links nothing.
func (s *Store) LinkOAuthProvider(userID, provider, subject string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("link oauth provider: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	providers, found, err := s.lockUserProvidersTx(tx, userID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("link oauth provider: user not found")
	}
	if subject != "" {
		if err := s.bindOAuthIdentityTx(tx, userID, provider, subject); err != nil {
			return err
		}
	}
	if !providerListHas(providers, provider) {
		if err := s.writeUserProvidersTx(tx, userID, append(providers, provider)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// OAuthIdentityOwner returns the pad account the provider account `subject`
// is bound to, or "" when it is bound to none.
func (s *Store) OAuthIdentityOwner(provider, subject string) (string, error) {
	var owner string
	err := s.db.QueryRow(s.q(`SELECT COALESCE((SELECT user_id FROM user_oauth_identities WHERE provider = ? AND subject = ?), '')`),
		provider, subject).Scan(&owner)
	if err != nil {
		return "", fmt.Errorf("oauth identity owner: %w", err)
	}
	return owner, nil
}
