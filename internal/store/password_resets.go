package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/xarmian/pad/internal/models"
)

const resetTokenTTL = 1 * time.Hour

// CreatePasswordReset generates a reset token for the given user.
// Returns the plaintext token (to embed in the reset URL). The token is
// stored as a SHA-256 hash — the plaintext cannot be recovered.
func (s *Store) CreatePasswordReset(userID string) (string, error) {
	// Invalidate any existing unused tokens for this user
	_, _ = s.db.Exec(s.q(`
		UPDATE password_reset_tokens SET used_at = ? WHERE user_id = ? AND used_at IS NULL
	`), now(), userID)

	// Generate token
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	plaintext := "padres_" + hex.EncodeToString(raw)
	hash := sha256.Sum256([]byte(plaintext))
	tokenHash := hex.EncodeToString(hash[:])

	expiresAt := time.Now().UTC().Add(resetTokenTTL).Format(time.RFC3339)

	_, err := s.db.Exec(s.q(`
		INSERT INTO password_reset_tokens (id, user_id, token_hash, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)
	`), newID(), userID, tokenHash, expiresAt, now())
	if err != nil {
		return "", fmt.Errorf("insert reset token: %w", err)
	}

	return plaintext, nil
}

// ConsumePasswordReset atomically validates and marks a reset token as used.
// Returns the user if the token is valid, unused, and not expired.
// Returns nil user if the token is invalid, already used, or expired.
// The token is marked as used in the same UPDATE, preventing race conditions
// where two concurrent requests could both validate the same token.
func (s *Store) ConsumePasswordReset(token string) (*models.User, error) {
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	// Atomically mark the token as used and return it, only if it's
	// currently unused and not expired. The WHERE clause ensures only
	// one concurrent caller can succeed.
	var userID string
	err := s.db.QueryRow(s.q(`
		UPDATE password_reset_tokens
		SET used_at = ?
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?
		RETURNING user_id
	`), now(), tokenHash, now()).Scan(&userID)

	if err == sql.ErrNoRows {
		return nil, nil // Invalid, expired, or already used
	}
	if err != nil {
		return nil, fmt.Errorf("consume reset token: %w", err)
	}

	// Fetch the user
	user, err := s.GetUser(userID)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}

	return user, nil
}

// CleanExpiredPasswordResets removes old reset tokens.
func (s *Store) CleanExpiredPasswordResets() error {
	_, err := s.db.Exec(s.q(`
		DELETE FROM password_reset_tokens WHERE expires_at < ? OR used_at IS NOT NULL
	`), now())
	return err
}
