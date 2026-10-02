package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// SessionInfo holds the result of session validation, including the
// authenticated user and session binding metadata for IP/UA verification.
type SessionInfo struct {
	User      *models.User
	IPAddress string
	UAHash    string
	// Kind is how the session was issued: SessionKindCLI or SessionKindWeb
	// (BUG-3350). Only a web session is accepted as a browser cookie.
	Kind string
	// CreatedAt is when the sign-in this session descends from happened: a
	// rotation (CreateSessionIssuedAt) carries it over rather than restarting
	// it (BUG-3336).
	CreatedAt time.Time
}

// Session kinds (BUG-3350). A CLI session is a bearer credential only; a web
// session is the browser's cookie (and may also be sent as a bearer).
const (
	SessionKindWeb = "web"
	SessionKindCLI = "cli"
)

// SessionKindFor maps a session's device label to its kind. The CLI's two
// sign-in paths label their sessions "cli-browser-auth" (browser approval)
// and "cli" (`pad auth login -i`).
func SessionKindFor(deviceInfo string) string {
	if deviceInfo == "cli" || deviceInfo == "cli-browser-auth" {
		return SessionKindCLI
	}
	return SessionKindWeb
}

// ErrUserDisabled is returned when a session would be created for an account
// that is disabled, or does not exist.
var ErrUserDisabled = errors.New("user is disabled")

// CreateSession generates a random session token, stores its SHA-256 hash,
// and returns the plaintext token (prefixed with "padsess_"). The plaintext
// is returned exactly once and never stored. IP address and User-Agent hash
// are stored for session binding validation.
func (s *Store) CreateSession(userID, deviceInfo, ipAddress, userAgent string, ttl time.Duration) (string, error) {
	return s.CreateSessionIssuedAt(userID, deviceInfo, ipAddress, userAgent, ttl, time.Time{})
}

// CreateSessionIssuedAt is CreateSession for a session that replaces an
// existing one (credential-change rotation): created_at is the REPLACED
// session's, so the sign-in it descends from stays dated where it happened.
// That keeps rotation from making a session look freshly signed in
// (BUG-3336's re-auth window) or from extending SessionMaxLifetime, which is
// measured from created_at. The expiry is capped at that lifetime too. A
// zero issuedAt means a new sign-in, now.
func (s *Store) CreateSessionIssuedAt(userID, deviceInfo, ipAddress, userAgent string, ttl time.Duration, issuedAt time.Time) (string, error) {
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
	expires := time.Now().UTC().Add(ttl)
	if !issuedAt.IsZero() {
		ts = issuedAt.UTC().Format(time.RFC3339)
		if limit := issuedAt.UTC().Add(SessionMaxLifetime); expires.After(limit) {
			expires = limit
		}
	}
	expiresAt := expires.Format(time.RFC3339)

	// Only for an account that is not disabled, decided under the users row
	// (BUG-3349, requireActiveUserTx): a request admitted before a disable,
	// minting after it, would otherwise leave a session its revoke never saw.
	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("insert session: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.requireActiveUserTx(tx, userID); err != nil {
		if errors.Is(err, ErrUserDisabled) {
			return "", err
		}
		return "", fmt.Errorf("insert session: %w", err)
	}
	if _, err := tx.Exec(s.q(`
		INSERT INTO sessions (id, user_id, token_hash, device_info, ip_address, ua_hash, expires_at, created_at, renew_ttl_seconds, kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), id, userID, tokenHash, deviceInfo, ipAddress, uaHash, expiresAt, ts, int64(ttl/time.Second), SessionKindFor(deviceInfo)); err != nil {
		return "", fmt.Errorf("insert session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("insert session: commit: %w", err)
	}

	return plaintext, nil
}

// ValidateSession hashes the provided token, looks it up, checks expiry,
// and returns the session info including the associated user and binding
// metadata (IP/UA). Returns nil if invalid or expired.
func (s *Store) ValidateSession(token string) (*SessionInfo, error) {
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	var userID, expiresAt, ipAddress, uaHash, createdAt, kind string
	err := s.db.QueryRow(s.q(`
		SELECT user_id, expires_at, ip_address, ua_hash, created_at, kind FROM sessions WHERE token_hash = ?
	`), tokenHash).Scan(&userID, &expiresAt, &ipAddress, &uaHash, &createdAt, &kind)
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
		CreatedAt: parseTime(createdAt),
		Kind:      kind,
	}, nil
}

// SessionMaxLifetime caps the total sliding lifetime of a session measured
// from its creation. Sliding renewal can push expires_at forward repeatedly
// while a session is active, but never past created_at + SessionMaxLifetime —
// so an indefinitely-active client still has to re-authenticate eventually.
const SessionMaxLifetime = 90 * 24 * time.Hour

// RenewSessionIfStale implements sliding-window expiration. When a session is
// past its renewal threshold (less than half its renewal window remaining) it
// extends expires_at to now + the session's renewal window, capped at
// created_at + SessionMaxLifetime. It returns the resulting expiry and whether
// a renewal was written.
//
// Sessions with renew_ttl_seconds <= 0 (legacy rows from before sliding
// renewal, and rows created with a non-positive TTL) are never renewed — their
// current expiry is returned with renewed=false.
//
// The threshold gate keeps writes rare: a 7-day window renews at most once
// every ~3.5 days of activity, not on every request. The UPDATE is a
// compare-and-set on the old expiry so concurrent requests don't double-write.
//
// Callers should treat this as best-effort: a renewal error must not fail an
// otherwise-valid request (the session is still valid until its current
// expiry).
func (s *Store) RenewSessionIfStale(token string) (time.Time, bool, error) {
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	var expiresAtStr, createdAtStr string
	var renewSeconds int64
	err := s.db.QueryRow(s.q(`
		SELECT expires_at, created_at, renew_ttl_seconds FROM sessions WHERE token_hash = ?
	`), tokenHash).Scan(&expiresAtStr, &createdAtStr, &renewSeconds)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read session for renewal: %w", err)
	}

	expiresAt := parseTime(expiresAtStr)
	if renewSeconds <= 0 {
		// Sliding renewal disabled for this session (legacy row).
		return expiresAt, false, nil
	}
	renewWindow := time.Duration(renewSeconds) * time.Second

	nowT := time.Now().UTC()
	// Only renew once we're past the threshold (< half the window remaining).
	if expiresAt.Sub(nowT) >= renewWindow/2 {
		return expiresAt, false, nil
	}

	newExpiry := nowT.Add(renewWindow)
	// Never let sliding renewal exceed the absolute lifetime cap.
	if createdAt := parseTime(createdAtStr); !createdAt.IsZero() {
		if cap := createdAt.Add(SessionMaxLifetime); newExpiry.After(cap) {
			newExpiry = cap
		}
	}
	// Never shorten an existing expiry (e.g. once the cap is reached).
	if !newExpiry.After(expiresAt) {
		return expiresAt, false, nil
	}

	newExpiryStr := newExpiry.Format(time.RFC3339)
	res, err := s.db.Exec(s.q(`
		UPDATE sessions SET expires_at = ? WHERE token_hash = ? AND expires_at = ?
	`), newExpiryStr, tokenHash, expiresAtStr)
	if err != nil {
		return expiresAt, false, fmt.Errorf("renew session: %w", err)
	}
	// Only report a renewal when this request's CAS actually wrote the row.
	// A zero-row update means we lost the race to a concurrent renewal (the
	// session is still renewed, just not by us) or the session was deleted
	// between the read and the write — in either case the caller must NOT
	// reissue a cookie advertising an expiry it didn't set. Treat an
	// unreliable RowsAffected as "not renewed" so we fail safe.
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return expiresAt, false, nil
	}
	return newExpiry, true, nil
}

// UpdateSessionIPIfEquals atomically rotates the stored IP on a session
// from expectedOldIP to newIP. Returns true if a row was updated (i.e. the
// caller is the race winner and should write the audit row); false when
// some other concurrent request already rotated the IP ahead of us.
//
// This compare-and-set behavior is what lets the auth middleware emit
// exactly one ActionSessionIPChanged audit row per transition even when
// multiple requests race after the client's IP flips.
func (s *Store) UpdateSessionIPIfEquals(token, expectedOldIP, newIP string) (bool, error) {
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	res, err := s.db.Exec(s.q(`
		UPDATE sessions
		SET ip_address = ?
		WHERE token_hash = ? AND ip_address = ?
	`), newIP, tokenHash, expectedOldIP)
	if err != nil {
		return false, fmt.Errorf("update session ip: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Some drivers don't report RowsAffected reliably. Err on the safe
		// side: treat as "not the winner" so we don't log when unsure.
		return false, nil
	}
	return n > 0, nil
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

// DeleteSessionIfExists destroys a session by plaintext token and reports
// whether a row was actually deleted. Used by the IP-change strict-mode
// path as a compare-and-set primitive: only the caller that actually
// deletes the row emits the ActionSessionIPChanged audit entry so
// concurrent requests don't write duplicate audit rows, and only the
// caller whose DELETE succeeded on the DB gets a clean "session is now
// gone" guarantee.
func (s *Store) DeleteSessionIfExists(token string) (bool, error) {
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	res, err := s.db.Exec(s.q("DELETE FROM sessions WHERE token_hash = ?"), tokenHash)
	if err != nil {
		return false, fmt.Errorf("delete session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Drivers that don't report affected rows → treat as "existed" so
		// callers don't skip their post-delete work.
		return true, nil
	}
	return n > 0, nil
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
