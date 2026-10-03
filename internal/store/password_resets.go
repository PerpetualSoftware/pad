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

const resetTokenTTL = 1 * time.Hour

// CreatePasswordReset generates a reset token for the given user.
// Returns the plaintext token (to embed in the reset URL). The token is
// stored as a SHA-256 hash — the plaintext cannot be recovered.
func (s *Store) CreatePasswordReset(userID string) (string, error) {
	// A bot never gets a reset link (TASK-3392).
	if err := s.refuseAppPrincipalQ(s.db, userID); err != nil {
		return "", err
	}
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

// LookupPasswordReset performs a read-only validation of a reset token
// and returns the associated user WITHOUT consuming the token. Used by
// the reset handler so we can run identity-aware password strength
// checks (which need email/name) before burning the token — rejecting
// a weak password pre-consume lets the user retry on the same reset
// link instead of having to request a fresh email.
//
// Returns nil user if the token is invalid, already used, or expired.
func (s *Store) LookupPasswordReset(token string) (*models.User, error) {
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	var userID string
	err := s.db.QueryRow(s.q(`
		SELECT user_id FROM password_reset_tokens
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?
		  -- A bot's token is no token (TASK-3392).
		  AND NOT EXISTS (SELECT 1 FROM users u WHERE u.id = password_reset_tokens.user_id AND u.kind = 'app')
	`), tokenHash, now()).Scan(&userID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup reset token: %w", err)
	}
	return s.GetUser(userID)
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
	//
	// The account's credential_epoch is read in the SAME statement
	// (BUG-3382): the password write that follows is fenced on it, and a
	// claim that committed after this token was spent must refuse that
	// write. A later read could return the claim's epoch instead. The token
	// row this statement locks is one the claim deletes, so a claim in
	// flight either commits before this statement (the token is gone) or
	// after it (its epoch bump is not seen).
	var userID string
	var epoch int64
	err := s.db.QueryRow(s.q(`
		UPDATE password_reset_tokens
		SET used_at = ?
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?
		  -- A bot's token is no token (TASK-3392).
		  AND NOT EXISTS (SELECT 1 FROM users u WHERE u.id = password_reset_tokens.user_id AND u.kind = 'app')
		RETURNING user_id, (SELECT credential_epoch FROM users WHERE users.id = password_reset_tokens.user_id)
	`), now(), tokenHash, now()).Scan(&userID, &epoch)

	if err == sql.ErrNoRows {
		return nil, nil // Invalid, expired, or already used
	}
	if err != nil {
		return nil, fmt.Errorf("consume reset token: %w", err)
	}

	if passwordResetSpentHook != nil {
		passwordResetSpentHook(userID)
	}

	// Fetch the user
	user, err := s.GetUser(userID)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	if user != nil {
		user.CredentialEpoch = epoch
	}

	return user, nil
}

// passwordResetSpentHook, when set by a test, runs after the reset token is
// spent and before the user is read back, which is where a claim can land
// (BUG-3382).
var passwordResetSpentHook func(userID string)

// CleanExpiredPasswordResets removes old reset tokens.
func (s *Store) CleanExpiredPasswordResets() error {
	_, err := s.db.Exec(s.q(`
		DELETE FROM password_reset_tokens WHERE expires_at < ? OR used_at IS NOT NULL
	`), now())
	return err
}
