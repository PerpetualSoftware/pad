package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// OAuthPendingLinkTTL is how long a pending provider link waits for the
// account's full sign-in (TASK-4745).
const OAuthPendingLinkTTL = 10 * time.Minute

// OAuthPendingLink is a provider identity an OAuth sign-in asserted for an
// existing account that had not linked that provider (TASK-4745). The
// account's next full sign-in in the browser holding the token links it.
type OAuthPendingLink struct {
	UserID   string
	Provider string
	Subject  string
	Email    string
}

func pendingLinkHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// CreateOAuthPendingLink records a pending link and returns its plaintext
// token (padpl_ + 64 hex). Only the SHA-256 is stored. Every older unused
// token of the account is spent in the same transaction, so only the newest
// is live.
func (s *Store) CreateOAuthPendingLink(userID, provider, subject, email string) (string, error) {
	// A bot never links a provider (TASK-3392).
	if err := s.refuseAppPrincipalQ(s.db, userID); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("pending link: generate token: %w", err)
	}
	plaintext := "padpl_" + hex.EncodeToString(raw)
	ts := now()
	expiresAt := time.Now().UTC().Add(OAuthPendingLinkTTL).Format(time.RFC3339)

	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("pending link: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(s.q(`
		UPDATE oauth_pending_links SET used_at = ? WHERE user_id = ? AND used_at IS NULL
	`), ts, userID); err != nil {
		return "", fmt.Errorf("pending link: invalidate older: %w", err)
	}
	if _, err := tx.Exec(s.q(`
		INSERT INTO oauth_pending_links (id, token_hash, user_id, provider, subject, email, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`), newID(), pendingLinkHash(plaintext), userID, provider, subject, email, expiresAt, ts); err != nil {
		return "", fmt.Errorf("pending link: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("pending link: commit: %w", err)
	}
	return plaintext, nil
}

// PeekOAuthPendingLink reads a live pending link without spending it. Nil
// when the token is unknown, spent or expired.
func (s *Store) PeekOAuthPendingLink(token string) (*OAuthPendingLink, error) {
	var pl OAuthPendingLink
	err := s.db.QueryRow(s.q(`
		SELECT user_id, provider, subject, email FROM oauth_pending_links
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?
	`), pendingLinkHash(token), now()).Scan(&pl.UserID, &pl.Provider, &pl.Subject, &pl.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("peek pending link: %w", err)
	}
	return &pl, nil
}

// ConsumeOAuthPendingLink spends a live pending link in one statement and
// returns it, so of two concurrent callers exactly one gets it. Nil when the
// token is unknown, spent or expired.
func (s *Store) ConsumeOAuthPendingLink(token string) (*OAuthPendingLink, error) {
	var pl OAuthPendingLink
	ts := now()
	err := s.db.QueryRow(s.q(`
		UPDATE oauth_pending_links SET used_at = ?
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?
		RETURNING user_id, provider, subject, email
	`), ts, pendingLinkHash(token), ts).Scan(&pl.UserID, &pl.Provider, &pl.Subject, &pl.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("consume pending link: %w", err)
	}
	return &pl, nil
}

// CleanExpiredOAuthPendingLinks removes spent and expired pending links. Run
// by the token reaper.
func (s *Store) CleanExpiredOAuthPendingLinks() error {
	_, err := s.db.Exec(s.q(`
		DELETE FROM oauth_pending_links WHERE expires_at < ? OR used_at IS NOT NULL
	`), now())
	return err
}
