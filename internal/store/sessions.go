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

// CreateSession generates a random session token, stores its SHA-256 hash,
// and returns the plaintext token (prefixed with "padsess_"). The plaintext
// is returned exactly once and never stored.
func (s *Store) CreateSession(userID, deviceInfo string, ttl time.Duration) (string, error) {
	// Generate 32 random bytes → hex → prefix
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	plaintext := "padsess_" + hex.EncodeToString(raw)

	hash := sha256.Sum256([]byte(plaintext))
	tokenHash := hex.EncodeToString(hash[:])

	id := newID()
	ts := now()
	expiresAt := time.Now().UTC().Add(ttl).Format(time.RFC3339)

	_, err := s.db.Exec(`
		INSERT INTO sessions (id, user_id, token_hash, device_info, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, id, userID, tokenHash, deviceInfo, expiresAt, ts)
	if err != nil {
		return "", fmt.Errorf("insert session: %w", err)
	}

	return plaintext, nil
}

// ValidateSession hashes the provided token, looks it up, checks expiry,
// and returns the associated user. Returns nil if invalid or expired.
func (s *Store) ValidateSession(token string) (*models.User, error) {
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	var userID, expiresAt string
	err := s.db.QueryRow(`
		SELECT user_id, expires_at FROM sessions WHERE token_hash = ?
	`, tokenHash).Scan(&userID, &expiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("validate session: %w", err)
	}

	// Check expiry
	if parseTime(expiresAt).Before(time.Now().UTC()) {
		return nil, nil
	}

	return s.GetUser(userID)
}

// DeleteSession destroys a session by its plaintext token.
func (s *Store) DeleteSession(token string) error {
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	_, err := s.db.Exec("DELETE FROM sessions WHERE token_hash = ?", tokenHash)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteUserSessions destroys all sessions for a user (logout everywhere).
func (s *Store) DeleteUserSessions(userID string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE user_id = ?", userID)
	if err != nil {
		return fmt.Errorf("delete user sessions: %w", err)
	}
	return nil
}

// CleanExpiredSessions removes all sessions past their expiry time.
func (s *Store) CleanExpiredSessions() error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE expires_at < ?", now())
	if err != nil {
		return fmt.Errorf("clean expired sessions: %w", err)
	}
	return nil
}
