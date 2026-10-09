package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const cliAuthSessionTTL = 5 * time.Minute

// cliAuthSetupSessionTTL is the longer window granted to a CLI auth session
// minted during first-run setup (no users exist yet). The setup handoff
// creates the session BEFORE the operator fills out the admin-account form
// in the browser (so /setup can redirect straight to the approval page —
// BUG-1843), so its clock must cover account creation AND the approval
// click, not just the click. 5 minutes was tight for that combined window;
// 20 gives comfortable headroom. Normal `pad auth login` (users already
// exist) keeps the shorter default.
const cliAuthSetupSessionTTL = 20 * time.Minute

// CLIAuthSession represents a pending or approved CLI login session.
type CLIAuthSession struct {
	Code      string `json:"code"`
	Status    string `json:"status"` // "pending", "approved", "expired", "denied"
	Token     string `json:"token,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	ExpiresAt string `json:"expires_at"`
	CreatedAt string `json:"created_at"`
	// Who asked (TASK-2253), shown on the approval page. RequesterIP is the
	// server's view of the client address; RequesterUserAgent is whatever
	// the requester sent, so it is requester-controlled text. Both are ''
	// on rows created before migration 132.
	RequesterIP        string `json:"requester_ip,omitempty"`
	RequesterUserAgent string `json:"requester_user_agent,omitempty"`
}

// CLIAuthRequester is where a CLI sign-in request came from (TASK-2253).
type CLIAuthRequester struct {
	IP        string
	UserAgent string
}

// cliAuthUserAgentMaxRunes caps the stored User-Agent. Long enough for
// every real agent string (Go's default is 18 bytes, a browser's ~120);
// anything past it is requester padding.
const cliAuthUserAgentMaxRunes = 200

// ErrCLIAuthNotPending is returned by ApproveCLIAuthSession and
// DenyCLIAuthSession when the session left the pending state between the
// caller's read and its write (a concurrent approve or deny). The caller
// re-reads to learn what it became.
var ErrCLIAuthNotPending = errors.New("cli auth session is no longer pending")

// CreateCLIAuthSession generates a new pending CLI auth session with a
// cryptographically random code and the default TTL. Returns the session
// including the code the CLI should present to the user.
func (s *Store) CreateCLIAuthSession() (*CLIAuthSession, error) {
	return s.createCLIAuthSession(cliAuthSessionTTL, CLIAuthRequester{})
}

// CreateCLIAuthSessionForSetup is CreateCLIAuthSession with the longer
// first-run window. The setup handoff mints the session before the admin
// account is even created (BUG-1843), so its clock must cover account
// creation as well as the approval click.
func (s *Store) CreateCLIAuthSessionForSetup() (*CLIAuthSession, error) {
	return s.createCLIAuthSession(cliAuthSetupSessionTTL, CLIAuthRequester{})
}

// CreateCLIAuthSessionFrom is the handler's door: it records who asked
// (TASK-2253) and picks the setup TTL when setup is true.
func (s *Store) CreateCLIAuthSessionFrom(setup bool, req CLIAuthRequester) (*CLIAuthSession, error) {
	ttl := cliAuthSessionTTL
	if setup {
		ttl = cliAuthSetupSessionTTL
	}
	return s.createCLIAuthSession(ttl, req)
}

// capRunes truncates s to at most n runes without splitting one, and drops
// invalid UTF-8 so the stored text is always valid.
func capRunes(s string, n int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func (s *Store) createCLIAuthSession(ttl time.Duration, req CLIAuthRequester) (*CLIAuthSession, error) {
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
	expiresAt := time.Now().UTC().Add(ttl).Format(time.RFC3339)

	ip := capRunes(req.IP, 64)
	ua := capRunes(req.UserAgent, cliAuthUserAgentMaxRunes)
	_, err := s.db.Exec(s.q(`
		INSERT INTO cli_auth_sessions (code, status, created_at, expires_at, requester_ip, requester_user_agent)
		VALUES (?, 'pending', ?, ?, ?, ?)
	`), code, ts, expiresAt, ip, ua)
	if err != nil {
		return nil, fmt.Errorf("insert cli auth session: %w", err)
	}

	return &CLIAuthSession{
		Code:               code,
		Status:             "pending",
		ExpiresAt:          expiresAt,
		CreatedAt:          ts,
		RequesterIP:        ip,
		RequesterUserAgent: ua,
	}, nil
}

// GetCLIAuthSession retrieves a CLI auth session by code. Returns nil if
// not found. Expired sessions are returned with status updated to "expired".
func (s *Store) GetCLIAuthSession(code string) (*CLIAuthSession, error) {
	var sess CLIAuthSession
	var token, userID sql.NullString

	err := s.db.QueryRow(s.q(`
		SELECT code, status, token, user_id, created_at, expires_at, requester_ip, requester_user_agent
		FROM cli_auth_sessions WHERE code = ?
	`), code).Scan(&sess.Code, &sess.Status, &token, &userID, &sess.CreatedAt, &sess.ExpiresAt,
		&sess.RequesterIP, &sess.RequesterUserAgent)
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
	if sess.Status == "denied" {
		return ErrCLIAuthNotPending
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
		return ErrCLIAuthNotPending
	}

	return nil
}

// DenyCLIAuthSession refuses a pending session (TASK-2253): the person
// shown the approval page did not ask for it. Only pending -> denied; the
// row stays (so the CLI's next poll learns why) until the expiry sweep in
// createCLIAuthSession removes it. Denying a denied session is a no-op.
// Returns ErrCLIAuthNotPending when it is approved or expired, after which
// the caller re-reads it.
func (s *Store) DenyCLIAuthSession(code string) error {
	result, err := s.db.Exec(s.q(`
		UPDATE cli_auth_sessions SET status = 'denied'
		WHERE code = ? AND status = 'pending' AND expires_at >= ?
	`), code, now())
	if err != nil {
		return fmt.Errorf("deny cli auth session: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 1 {
		return nil
	}
	sess, err := s.GetCLIAuthSession(code)
	if err != nil {
		return err
	}
	if sess != nil && sess.Status == "denied" {
		return nil
	}
	return ErrCLIAuthNotPending
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
