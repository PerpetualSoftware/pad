package models

import "time"

// Comment represents a comment on an item.
type Comment struct {
	ID          string    `json:"id"`
	ItemID      string    `json:"item_id"`
	WorkspaceID string    `json:"workspace_id"`
	Author      string    `json:"author"`
	Body        string    `json:"body"`
	CreatedBy   string    `json:"created_by"`
	Source      string    `json:"source"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// Populated by joins (not stored)
	ItemTitle string `json:"item_title,omitempty"`
	ItemSlug  string `json:"item_slug,omitempty"`
}

// CommentCreate is the input for creating a new comment.
type CommentCreate struct {
	Author    string `json:"author,omitempty"`
	Body      string `json:"body"`
	CreatedBy string `json:"created_by,omitempty"`
	Source    string `json:"source,omitempty"`
}
