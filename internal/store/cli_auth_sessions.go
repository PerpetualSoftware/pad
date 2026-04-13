package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

const cliAuthSessionTTL = 5 * time.Minute

// CLIAuthSession represents a pending or approved CLI login session.
type CLIAuthSession struct {
	Code      string `json:"code"`
	Status    string `json:"status"` // "pending", "approved", "expired"
	Token     string `json:"token,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	ExpiresAt string `json:"expires_at"`
	CreatedAt string `json:"created_at"`
}

// CreateCLIAuthSession generates a new pending CLI auth session with a
// cryptographically random code. Returns the session including the code
// the CLI should present to the user.
func (s *Store) CreateCLIAuthSession() (*CLIAuthSession, error) {
	// Clean up expired sessions opportunistically
	_, _ = s.db.Exec(s.q(`
		DELETE FROM cli_auth_sessions WHERE expires_at < ?
	`), now())

	// Generate a 16-byte random code (32 hex chars)
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate cli auth code: %w", err)
	}
	code := hex.EncodeToString(raw)

	ts := now()
	expiresAt := time.Now().UTC().Add(cliAuthSessionTTL).Format(time.RFC3339)

	_, err := s.db.Exec(s.q(`
		INSERT INTO cli_auth_sessions (code, status, created_at, expires_at)
		VALUES (?, 'pending', ?, ?)
	`), code, ts, expiresAt)
	if err != nil {
		return nil, fmt.Errorf("insert cli auth session: %w", err)
	}

	return &CLIAuthSession{
		Code:      code,
		Status:    "pending",
		ExpiresAt: expiresAt,
		CreatedAt: ts,
	}, nil
}

// GetCLIAuthSession retrieves a CLI auth session by code. Returns nil if
// not found. Expired sessions are returned with status updated to "expired".
func (s *Store) GetCLIAuthSession(code string) (*CLIAuthSession, error) {
	var sess CLIAuthSession
	var token, userID sql.NullString

	err := s.db.QueryRow(s.q(`
		SELECT code, status, token, user_id, created_at, expires_at
		FROM cli_auth_sessions WHERE code = ?
	`), code).Scan(&sess.Code, &sess.Status, &token, &userID, &sess.CreatedAt, &sess.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get cli auth session: %w", err)
	}

	if token.Valid {
		sess.Token = token.String
	}
	if userID.Valid {
		sess.UserID = userID.String
	}

	// Check expiry
	if sess.Status == "pending" && parseTime(sess.ExpiresAt).Before(time.Now().UTC()) {
		sess.Status = "expired"
		// Update in DB lazily
		_, _ = s.db.Exec(s.q(`
			UPDATE cli_auth_sessions SET status = 'expired' WHERE code = ?
		`), code)
	}

	return &sess, nil
}

// ApproveCLIAuthSession marks a pending session as approved, storing the
// session token and user ID. Returns an error if the session doesn't exist,
// is expired, or was already approved.
func (s *Store) ApproveCLIAuthSession(code, sessionToken, userID string) error {
	sess, err := s.GetCLIAuthSession(code)
	if err != nil {
		return err
	}
	if sess == nil {
		return fmt.Errorf("cli auth session not found")
	}
	if sess.Status == "expired" {
		return fmt.Errorf("cli auth session has expired")
	}
	if sess.Status == "approved" {
		return fmt.Errorf("cli auth session already approved")
	}

	result, err := s.db.Exec(s.q(`
		UPDATE cli_auth_sessions
		SET status = 'approved', token = ?, user_id = ?
		WHERE code = ? AND status = 'pending'
	`), sessionToken, userID, code)
	if err != nil {
		return fmt.Errorf("approve cli auth session: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("cli auth session could not be approved (race condition or already consumed)")
	}

	return nil
}

// DeleteCLIAuthSession removes a CLI auth session by code.
func (s *Store) DeleteCLIAuthSession(code string) error {
	_, err := s.db.Exec(s.q(`DELETE FROM cli_auth_sessions WHERE code = ?`), code)
	if err != nil {
		return fmt.Errorf("delete cli auth session: %w", err)
	}
	return nil
}

// CleanExpiredCLIAuthSessions removes all expired CLI auth sessions.
func (s *Store) CleanExpiredCLIAuthSessions() error {
	_, err := s.db.Exec(s.q(`DELETE FROM cli_auth_sessions WHERE expires_at < ?`), now())
	if err != nil {
		return fmt.Errorf("clean expired cli auth sessions: %w", err)
	}
	return nil
}
