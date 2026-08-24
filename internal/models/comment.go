package models

import "time"

// Comment represents a comment on an item.
type Comment struct {
	ID          string `json:"id"`
	ItemID      string `json:"item_id"`
	WorkspaceID string `json:"workspace_id"`
	Author      string `json:"author"`
	// UserID is the authenticated user who authored the comment. Empty for
	// pre-identity comments (created before TASK-1663) and agent/system
	// comments; the comment-edit permission check treats empty as
	// "no provable author" → admin-only.
	UserID     string    `json:"user_id,omitempty"`
	Body       string    `json:"body"`
	CreatedBy  string    `json:"created_by"`
	Source     string    `json:"source"`
	ActivityID string    `json:"activity_id,omitempty"`
	ParentID   string    `json:"parent_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`

	// Populated by joins (not stored)
	ItemTitle string `json:"item_title,omitempty"`
	ItemSlug  string `json:"item_slug,omitempty"`
	// AgentName is the display name the writing agent declared (the
	// X-Pad-Agent header), read off the `commented` activity this comment's
	// ActivityID points at — comments themselves never store it; workspace
	// activity rows are the only carrier (TASK-2759 / TASK-2760). Its ONLY
	// writer is the LEFT JOIN in the store's list queries (scanComments in
	// comments.go), so it is empty on a comment read any other way
	// (GetComment, the create/update read-back) and on a comment whose
	// activity carries no stamp — a human's write, a pre-BUG-2542 row, an
	// agent that sent no header. It is populated whatever CreatedBy says;
	// the client decides whether the actor kind makes it meaningful.
	// Self-declared, so it records what the client said, not who acted.
	AgentName string `json:"agent_name,omitempty"`

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
