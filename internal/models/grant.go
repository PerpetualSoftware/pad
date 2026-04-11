package models

import "time"

// CollectionGrant represents a user's direct access to a collection
// (for guests or member overrides).
type CollectionGrant struct {
	ID           string    `json:"id"`
	CollectionID string    `json:"collection_id"`
	WorkspaceID  string    `json:"workspace_id"`
	UserID       string    `json:"user_id"`
	Permission   string    `json:"permission"` // "view" or "edit"
	GrantedBy    string    `json:"granted_by"`
	CreatedAt    time.Time `json:"created_at"`

	// Populated by JOINs (not stored)
	UserName     string `json:"user_name,omitempty"`
	UserEmail    string `json:"user_email,omitempty"`
	UserUsername string `json:"user_username,omitempty"`
}

// ItemGrant represents a user's direct access to a specific item
// (for guests or member overrides).
type ItemGrant struct {
	ID          string    `json:"id"`
	ItemID      string    `json:"item_id"`
	WorkspaceID string    `json:"workspace_id"`
	UserID      string    `json:"user_id"`
	Permission  string    `json:"permission"` // "view" or "edit"
	GrantedBy   string    `json:"granted_by"`
	CreatedAt   time.Time `json:"created_at"`

	// Populated by JOINs (not stored)
	UserName     string `json:"user_name,omitempty"`
	UserEmail    string `json:"user_email,omitempty"`
	UserUsername string `json:"user_username,omitempty"`
}
