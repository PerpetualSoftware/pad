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
	_, _ = s.db.Exec(`
		UPDATE password_reset_tokens SET used_at = ? WHERE user_id = ? AND used_at IS NULL
	`, now(), userID)

	// Generate token
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	plaintext := "padres_" + hex.EncodeToString(raw)
	hash := sha256.Sum256([]byte(plaintext))
	tokenHash := hex.EncodeToString(hash[:])

	expiresAt := time.Now().UTC().Add(resetTokenTTL).Format(time.RFC3339)

	_, err := s.db.Exec(`
		INSERT INTO password_reset_tokens (id, user_id, token_hash, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, newID(), userID, tokenHash, expiresAt, now())
	if err != nil {
		return "", fmt.Errorf("insert reset token: %w", err)
	}

	return plaintext, nil
}

// ValidatePasswordReset checks a plaintext reset token. Returns the user
// if the token is valid, unused, and not expired. Returns nil if invalid.
func (s *Store) ValidatePasswordReset(token string) (*models.User, string, error) {
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	var resetID, userID, expiresAtStr string
	var usedAt sql.NullString

	err := s.db.QueryRow(`
		SELECT id, user_id, expires_at, used_at FROM password_reset_tokens
		WHERE token_hash = ?
	`, tokenHash).Scan(&resetID, &userID, &expiresAtStr, &usedAt)

	if err == sql.ErrNoRows {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("query reset token: %w", err)
	}

	// Already used
	if usedAt.Valid {
		return nil, "", nil
	}

	// Expired
	expiresAt, err := time.Parse(time.RFC3339, expiresAtStr)
	if err != nil {
		return nil, "", fmt.Errorf("parse expires_at: %w", err)
	}
	if time.Now().UTC().After(expiresAt) {
		return nil, "", nil
	}

	// Valid — fetch the user
	user, err := s.GetUser(userID)
	if err != nil {
		return nil, "", fmt.Errorf("get user: %w", err)
	}

	return user, resetID, nil
}

// MarkPasswordResetUsed marks a reset token as consumed.
func (s *Store) MarkPasswordResetUsed(resetID string) error {
	_, err := s.db.Exec(`
		UPDATE password_reset_tokens SET used_at = ? WHERE id = ?
	`, now(), resetID)
	return err
}

// CleanExpiredPasswordResets removes old reset tokens.
func (s *Store) CleanExpiredPasswordResets() error {
	_, err := s.db.Exec(`
		DELETE FROM password_reset_tokens WHERE expires_at < ? OR used_at IS NOT NULL
	`, now())
	return err
}
