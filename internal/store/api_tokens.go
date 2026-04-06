package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"

	"github.com/xarmian/pad/internal/models"
)

// CreateAPIToken generates a new API token owned by a user, optionally
// scoped to a workspace. The plaintext token is returned in the response
// and is never stored — only its SHA-256 hash is persisted.
func (s *Store) CreateAPIToken(userID string, input models.APITokenCreate) (*models.APITokenWithSecret, error) {
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

	_, err := s.db.Exec(s.q(`
		INSERT INTO api_tokens (id, workspace_id, user_id, name, token_hash, prefix, scopes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`), id, wsID, userID, input.Name, tokenHash, prefix, scopes, ts)
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
