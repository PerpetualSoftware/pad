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

// CreateAPIToken generates a new API token owned by a user, optionally
// scoped to a workspace. The plaintext token is returned in the response
// and is never stored — only its SHA-256 hash is persisted.
//
// If expiresInDays is 0 on the input, the platform default is used.
// The maxLifetimeDays parameter enforces a ceiling on expiry (0 = no limit).
func (s *Store) CreateAPIToken(userID string, input models.APITokenCreate, defaultExpiryDays, maxLifetimeDays int) (*models.APITokenWithSecret, error) {
	// Generate 32 random bytes → 64 hex chars
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}
	plaintext := "pad_" + hex.EncodeToString(raw)
	prefix := plaintext[:8]

	// SHA-256 hash for storage
	hash := sha256.Sum256([]byte(plaintext))
	tokenHash := hex.EncodeToString(hash[:])

	id := newID()
	ts := now()

	scopes := input.Scopes
	if scopes == "" {
		scopes = `["*"]`
	}

	// workspace_id is optional (empty string → NULL for unscoped tokens)
	var wsID interface{}
	if input.WorkspaceID != "" {
		wsID = input.WorkspaceID
	}

	// Determine expiry
	expiryDays := input.ExpiresIn
	if expiryDays <= 0 {
		expiryDays = defaultExpiryDays
	}
	// Enforce max lifetime if configured
	if maxLifetimeDays > 0 && expiryDays > maxLifetimeDays {
		expiryDays = maxLifetimeDays
	}

	var expiresAt interface{}
	if expiryDays > 0 {
		expiresAt = time.Now().UTC().Add(time.Duration(expiryDays) * 24 * time.Hour).Format(time.RFC3339)
	}

	_, err := s.db.Exec(s.q(`
		INSERT INTO api_tokens (id, workspace_id, user_id, name, token_hash, prefix, scopes, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), id, wsID, userID, input.Name, tokenHash, prefix, scopes, expiresAt, ts)
	if err != nil {
		return nil, fmt.Errorf("insert api token: %w", err)
	}

	token, err := s.getAPIToken(id)
	if err != nil {
		return nil, err
	}

	return &models.APITokenWithSecret{
		APIToken: *token,
		Token:    plaintext,
	}, nil
}

// RotateAPIToken generates a new secret for an existing token, preserving
// all metadata (name, scopes, workspace, user, expiry). The old token is
// invalidated and the new plaintext is returned once.
//
// expiryDays controls the new expiry: 0 keeps the original, >0 sets a new one.
// maxLifetimeDays enforces a ceiling on the new expiry (0 = no limit).
func (s *Store) RotateAPIToken(tokenID, userID string, expiryDays, maxLifetimeDays int) (*models.APITokenWithSecret, error) {
	// Verify the token exists and belongs to the user
	existing, err := s.getAPIToken(tokenID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, sql.ErrNoRows
	}
	if existing.UserID != userID {
		return nil, sql.ErrNoRows
	}

	// Generate new secret
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate rotated token: %w", err)
	}
	plaintext := "pad_" + hex.EncodeToString(raw)
	prefix := plaintext[:8]

	hash := sha256.Sum256([]byte(plaintext))
	tokenHash := hex.EncodeToString(hash[:])

	// Determine new expiry
	var expiresAt interface{}
	if expiryDays > 0 {
		if maxLifetimeDays > 0 && expiryDays > maxLifetimeDays {
			expiryDays = maxLifetimeDays
		}
		expiresAt = time.Now().UTC().Add(time.Duration(expiryDays) * 24 * time.Hour).Format(time.RFC3339)
	} else if existing.ExpiresAt != nil {
		// Preserve original expiry
		expiresAt = existing.ExpiresAt.Format(time.RFC3339)
	}

	_, err = s.db.Exec(s.q(`
		UPDATE api_tokens SET token_hash = ?, prefix = ?, expires_at = ?, last_used_at = NULL
		WHERE id = ?
	`), tokenHash, prefix, expiresAt, tokenID)
	if err != nil {
		return nil, fmt.Errorf("rotate api token: %w", err)
	}

	updated, err := s.getAPIToken(tokenID)
	if err != nil {
		return nil, err
	}

	return &models.APITokenWithSecret{
		APIToken: *updated,
		Token:    plaintext,
	}, nil
}

// ListAPITokens returns all API tokens for a workspace (without secrets).
func (s *Store) ListAPITokens(workspaceID string) ([]models.APIToken, error) {
	rows, err := s.db.Query(s.q(`
		SELECT id, COALESCE(workspace_id, ''), COALESCE(user_id, ''), name, prefix, scopes, expires_at, last_used_at, created_at
		FROM api_tokens
		WHERE workspace_id = ?
		ORDER BY created_at ASC
	`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	defer rows.Close()

	var result []models.APIToken
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *t)
	}
	return result, rows.Err()
}

// ListUserAPITokens returns all API tokens owned by a user (without secrets).
func (s *Store) ListUserAPITokens(userID string) ([]models.APIToken, error) {
	rows, err := s.db.Query(s.q(`
		SELECT id, COALESCE(workspace_id, ''), COALESCE(user_id, ''), name, prefix, scopes, expires_at, last_used_at, created_at
		FROM api_tokens
		WHERE user_id = ?
		ORDER BY created_at ASC
	`), userID)
	if err != nil {
		return nil, fmt.Errorf("list user api tokens: %w", err)
	}
	defer rows.Close()

	var result []models.APIToken
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *t)
	}
	return result, rows.Err()
}

// DeleteAPIToken removes an API token by ID.
func (s *Store) DeleteAPIToken(id string) error {
	result, err := s.db.Exec(s.q("DELETE FROM api_tokens WHERE id = ?"), id)
	if err != nil {
		return fmt.Errorf("delete api token: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteUserAPIToken removes an API token by ID, verifying it belongs to the user.
func (s *Store) DeleteUserAPIToken(id, userID string) error {
	result, err := s.db.Exec(s.q("DELETE FROM api_tokens WHERE id = ? AND user_id = ?"), id, userID)
	if err != nil {
		return fmt.Errorf("delete user api token: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ValidateToken hashes the provided plaintext token, looks it up in the
// database, checks expiry, and updates last_used_at. Returns nil if the
// token is invalid or expired.
func (s *Store) ValidateToken(token string) (*models.APIToken, error) {
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	var t models.APIToken
	var expiresAt, lastUsedAt, userID *string
	var workspaceID *string
	var createdAt string

	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, user_id, name, prefix, scopes, expires_at, last_used_at, created_at
		FROM api_tokens
		WHERE token_hash = ?
	`), tokenHash).Scan(
		&t.ID, &workspaceID, &userID, &t.Name, &t.Prefix, &t.Scopes,
		&expiresAt, &lastUsedAt, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("validate token: %w", err)
	}

	if workspaceID != nil {
		t.WorkspaceID = *workspaceID
	}
	if userID != nil {
		t.UserID = *userID
	}
	t.CreatedAt = parseTime(createdAt)
	t.ExpiresAt = parseTimePtr(expiresAt)
	t.LastUsedAt = parseTimePtr(lastUsedAt)

	// Check expiry
	if t.ExpiresAt != nil && t.ExpiresAt.Before(parseTime(now())) {
		return nil, nil
	}

	// Update last_used_at
	ts := now()
	_, _ = s.db.Exec(s.q("UPDATE api_tokens SET last_used_at = ? WHERE id = ?"), ts, t.ID)

	return &t, nil
}

// getAPIToken retrieves a single API token by ID.
func (s *Store) getAPIToken(id string) (*models.APIToken, error) {
	var t models.APIToken
	var expiresAt, lastUsedAt, userID, workspaceID *string
	var createdAt string

	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, user_id, name, prefix, scopes, expires_at, last_used_at, created_at
		FROM api_tokens
		WHERE id = ?
	`), id).Scan(
		&t.ID, &workspaceID, &userID, &t.Name, &t.Prefix, &t.Scopes,
		&expiresAt, &lastUsedAt, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get api token: %w", err)
	}

	if workspaceID != nil {
		t.WorkspaceID = *workspaceID
	}
	if userID != nil {
		t.UserID = *userID
	}
	t.CreatedAt = parseTime(createdAt)
	t.ExpiresAt = parseTimePtr(expiresAt)
	t.LastUsedAt = parseTimePtr(lastUsedAt)
	return &t, nil
}

// scanner is an interface satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...interface{}) error
}

// scanAPIToken scans an API token from a row scanner.
func scanAPIToken(s scanner) (*models.APIToken, error) {
	var t models.APIToken
	var expiresAt, lastUsedAt *string
	var createdAt string

	if err := s.Scan(
		&t.ID, &t.WorkspaceID, &t.UserID, &t.Name, &t.Prefix, &t.Scopes,
		&expiresAt, &lastUsedAt, &createdAt,
	); err != nil {
		return nil, fmt.Errorf("scan api token: %w", err)
	}

	t.CreatedAt = parseTime(createdAt)
	t.ExpiresAt = parseTimePtr(expiresAt)
	t.LastUsedAt = parseTimePtr(lastUsedAt)
	return &t, nil
}
