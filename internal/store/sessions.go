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

// SessionInfo holds the result of session validation, including the
// authenticated user and session binding metadata for IP/UA verification.
type SessionInfo struct {
	User      *models.User
	IPAddress string
	UAHash    string
}

// CreateSession generates a random session token, stores its SHA-256 hash,
// and returns the plaintext token (prefixed with "padsess_"). The plaintext
// is returned exactly once and never stored. IP address and User-Agent hash
// are stored for session binding validation.
func (s *Store) CreateSession(userID, deviceInfo, ipAddress, userAgent string, ttl time.Duration) (string, error) {
	// Generate 32 random bytes → hex → prefix
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	plaintext := "padsess_" + hex.EncodeToString(raw)

	hash := sha256.Sum256([]byte(plaintext))
	tokenHash := hex.EncodeToString(hash[:])

	// Hash the User-Agent for storage (we don't need the original)
	uaHash := ""
	if userAgent != "" {
		h := sha256.Sum256([]byte(userAgent))
		uaHash = hex.EncodeToString(h[:])
	}

	id := newID()
	ts := now()
	expiresAt := time.Now().UTC().Add(ttl).Format(time.RFC3339)

	_, err := s.db.Exec(s.q(`
		INSERT INTO sessions (id, user_id, token_hash, device_info, ip_address, ua_hash, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`), id, userID, tokenHash, deviceInfo, ipAddress, uaHash, expiresAt, ts)
	if err != nil {
		return "", fmt.Errorf("insert session: %w", err)
	}

	return plaintext, nil
}

// ValidateSession hashes the provided token, looks it up, checks expiry,
// and returns the session info including the associated user and binding
// metadata (IP/UA). Returns nil if invalid or expired.
func (s *Store) ValidateSession(token string) (*SessionInfo, error) {
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	var userID, expiresAt, ipAddress, uaHash string
	err := s.db.QueryRow(s.q(`
		SELECT user_id, expires_at, ip_address, ua_hash FROM sessions WHERE token_hash = ?
	`), tokenHash).Scan(&userID, &expiresAt, &ipAddress, &uaHash)
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

	user, err := s.GetUser(userID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, nil
	}

	return &SessionInfo{
		User:      user,
		IPAddress: ipAddress,
		UAHash:    uaHash,
	}, nil
}

// UpdateSessionIP records a new client IP on a session, identified by its
// plaintext token. Used by the IP-change audit path to track the current
// observed IP without closing the session. Safe to call when the new IP
// matches the stored one — still a no-op UPDATE, no rows touched.
func (s *Store) UpdateSessionIP(token, newIP string) error {
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	_, err := s.db.Exec(s.q(`
		UPDATE sessions SET ip_address = ? WHERE token_hash = ?
	`), newIP, tokenHash)
	if err != nil {
		return fmt.Errorf("update session ip: %w", err)
	}
	return nil
}

// DeleteSession destroys a session by its plaintext token.
func (s *Store) DeleteSession(token string) error {
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	_, err := s.db.Exec(s.q("DELETE FROM sessions WHERE token_hash = ?"), tokenHash)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteUserSessions destroys all sessions for a user (logout everywhere).
func (s *Store) DeleteUserSessions(userID string) error {
	_, err := s.db.Exec(s.q("DELETE FROM sessions WHERE user_id = ?"), userID)
	if err != nil {
		return fmt.Errorf("delete user sessions: %w", err)
	}
	return nil
}

// CleanExpiredSessions removes all sessions past their expiry time.
func (s *Store) CleanExpiredSessions() error {
	_, err := s.db.Exec(s.q("DELETE FROM sessions WHERE expires_at < ?"), now())
	if err != nil {
		return fmt.Errorf("clean expired sessions: %w", err)
	}
	return nil
}
