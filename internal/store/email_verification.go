package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// verificationTokenTTL is the lifetime of an email verification link.
// 24h (vs password reset's 1h) — verification is a lower-urgency,
// higher-friction flow (a new signup may not check email immediately),
// so the link stays valid longer. PLAN-1933 DR-2 / DR-5.
const verificationTokenTTL = 24 * time.Hour

// CreateEmailVerification generates an email-verification token for the given
// user. Returns the plaintext token (to embed in the verification URL). The
// token is stored as a SHA-256 hash — the plaintext cannot be recovered.
//
// Mirrors CreatePasswordReset (PLAN-1933 DR-2) with three deltas: a 24h TTL,
// the `padver_` prefix, and no password/session side-effects. The
// invalidate-prior-unused-tokens-on-mint behavior is KEPT so a
// resend-verification silently burns the previous link.
func (s *Store) CreateEmailVerification(userID string) (string, error) {
	// Invalidate any existing unused tokens for this user so a resend
	// invalidates the previous verification link.
	_, _ = s.db.Exec(s.q(`
		UPDATE email_verification_tokens SET used_at = ? WHERE user_id = ? AND used_at IS NULL
	`), now(), userID)

	// Generate token (256-bit crypto/rand).
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	plaintext := "padver_" + hex.EncodeToString(raw)
	hash := sha256.Sum256([]byte(plaintext))
	tokenHash := hex.EncodeToString(hash[:])

	expiresAt := time.Now().UTC().Add(verificationTokenTTL).Format(time.RFC3339)

	_, err := s.db.Exec(s.q(`
		INSERT INTO email_verification_tokens (id, user_id, token_hash, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)
	`), newID(), userID, tokenHash, expiresAt, now())
	if err != nil {
		return "", fmt.Errorf("insert verification token: %w", err)
	}

	return plaintext, nil
}

// LookupEmailVerification performs a read-only validation of a verification
// token and returns the associated user WITHOUT consuming the token. Mirrors
// LookupPasswordReset — useful for a pre-consume validity check.
//
// Returns nil user if the token is invalid, already used, or expired.
func (s *Store) LookupEmailVerification(token string) (*models.User, error) {
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	var userID string
	err := s.db.QueryRow(s.q(`
		SELECT user_id FROM email_verification_tokens
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?
	`), tokenHash, now()).Scan(&userID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup verification token: %w", err)
	}
	return s.GetUser(userID)
}

// ConsumeEmailVerification atomically validates and marks a verification token
// as used, then sets users.email_verified_at to now. Returns the (now-verified)
// user if the token is valid, unused, and not expired; nil user otherwise.
//
// The token is marked used in the same UPDATE that gates on used_at IS NULL,
// so only one concurrent caller can succeed (race-safe single-use). Both the
// token consume and the user update run in one transaction so a crash between
// them can't leave a consumed-but-unverified state.
//
// The email_verified_at value uses the same RFC3339-with-`Z` format Wave 1's
// migration used (now() → time.RFC3339 UTC), so time.Parse(time.RFC3339, …) /
// store.parseTime reads it back. Unlike ConsumePasswordReset this does NOT
// reset a password or mint a session — verification is the sole side-effect.
func (s *Store) ConsumeEmailVerification(token string) (*models.User, error) {
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin consume verification: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Atomically mark the token used and return its user, only if it's
	// currently unused and not expired. The WHERE clause ensures only one
	// concurrent caller can succeed.
	var userID string
	err = tx.QueryRow(s.q(`
		UPDATE email_verification_tokens
		SET used_at = ?
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?
		RETURNING user_id
	`), now(), tokenHash, now()).Scan(&userID)
	if err == sql.ErrNoRows {
		return nil, nil // Invalid, expired, or already used
	}
	if err != nil {
		return nil, fmt.Errorf("consume verification token: %w", err)
	}

	// Side-effect: mark the user's email verified.
	if _, err := tx.Exec(s.q(`
		UPDATE users SET email_verified_at = ?, updated_at = ? WHERE id = ?
	`), now(), now(), userID); err != nil {
		return nil, fmt.Errorf("set email verified: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit consume verification: %w", err)
	}

	user, err := s.GetUser(userID)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return user, nil
}

// CleanExpiredEmailVerifications removes expired or already-used verification
// tokens. Mirrors CleanExpiredPasswordResets; run by the periodic token reaper.
func (s *Store) CleanExpiredEmailVerifications() error {
	_, err := s.db.Exec(s.q(`
		DELETE FROM email_verification_tokens WHERE expires_at < ? OR used_at IS NOT NULL
	`), now())
	return err
}
