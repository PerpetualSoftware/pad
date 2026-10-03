package store

import (
	"errors"
	"fmt"
)

// ErrOAuthSubjectMismatch: the provider account asserting the address is not
// the one this pad account is bound to, or that provider account is bound to
// a different pad account (TASK-3351).
var ErrOAuthSubjectMismatch = errors.New("oauth identity: subject mismatch")

// BindOAuthIdentity records that the provider account `subject` signs in to
// userID, or confirms the record already says so. The first sign-in carrying
// a subject binds it; every later one must match. It returns
// ErrOAuthSubjectMismatch when userID is bound to a different subject for
// this provider, or the subject is bound to another account. Concurrent
// first binds settle on one row: the losing insert is ignored and the
// read-back decides.
func (s *Store) BindOAuthIdentity(userID, provider, subject string) error {
	var stmt string
	if s.dialect.Driver() == DriverPostgres {
		stmt = `INSERT INTO user_oauth_identities (provider, subject, user_id, created_at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`
	} else {
		stmt = `INSERT OR IGNORE INTO user_oauth_identities (provider, subject, user_id, created_at) VALUES (?, ?, ?, ?)`
	}
	if _, err := s.db.Exec(s.q(stmt), provider, subject, userID, now()); err != nil {
		return fmt.Errorf("bind oauth identity: %w", err)
	}
	var owner, bound string
	err := s.db.QueryRow(s.q(`
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

// DeleteOAuthIdentity forgets the provider account bound to userID for
// provider, on unlink.
func (s *Store) DeleteOAuthIdentity(userID, provider string) error {
	if _, err := s.db.Exec(s.q(`DELETE FROM user_oauth_identities WHERE user_id = ? AND provider = ?`), userID, provider); err != nil {
		return fmt.Errorf("delete oauth identity: %w", err)
	}
	return nil
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
