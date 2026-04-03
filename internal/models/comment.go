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
	ActivityID  string    `json:"activity_id,omitempty"`
	ParentID    string    `json:"parent_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// Populated by joins (not stored)
	ItemTitle string `json:"item_title,omitempty"`
	ItemSlug  string `json:"item_slug,omitempty"`

	// Populated by handlers for threaded views
	Replies   []Comment  `json:"replies,omitempty"`
	Reactions []Reaction `json:"reactions,omitempty"`
}

// CommentCreate is the input for creating a new comment.
type CommentCreate struct {
	Author     string `json:"author,omitempty"`
	Body       string `json:"body"`
	CreatedBy  string `json:"created_by,omitempty"`
	Source     string `json:"source,omitempty"`
	ParentID   string `json:"parent_id,omitempty"`
	ActivityID string `json:"activity_id,omitempty"`
}

// Reaction represents an emoji reaction on a comment.
type Reaction struct {
	ID        string    `json:"id"`
	CommentID string    `json:"comment_id"`
	UserID    string    `json:"user_id,omitempty"`
	Actor     string    `json:"actor"`
	Emoji     string    `json:"emoji"`
	CreatedAt time.Time `json:"created_at"`
	ActorName string    `json:"actor_name,omitempty"`
}
