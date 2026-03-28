package models

import "time"

// APIToken represents a stored API token (without the secret).
type APIToken struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	Name        string     `json:"name"`
	Prefix      string     `json:"prefix"`
	Scopes      string     `json:"scopes"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// APITokenCreate is the input for creating a new API token.
type APITokenCreate struct {
	Name   string `json:"name"`
	Scopes string `json:"scopes,omitempty"`
}

// APITokenWithSecret is returned only on creation and includes the
// plaintext token. The token is never stored and cannot be retrieved again.
type APITokenWithSecret struct {
	APIToken
	Token string `json:"token"` // Only returned once
}
