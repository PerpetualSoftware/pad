package models

import (
	"encoding/json"
	"time"
)

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
	// X-Pad-Agent header), read off the activity this comment's ActivityID
	// points at — the `commented` row a comment or reply logs, or the
	// `updated` row of an item update that carried the comment. Comments
	// themselves never store it; workspace activity rows are the only
	// carrier (TASK-2759 / TASK-2760). Its ONLY
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

// IsEdited reports whether the body changed after creation: create stamps
// created_at and updated_at with one value, and only an edit moves updated_at
// (reactions live in their own table). Timestamps are second-precision, so an
// edit inside the creation second is not seen. Same rule as the web timeline's
// isEdited (TimelineCommentCard.svelte); TASK-2695 brought it to the CLI.
func (c Comment) IsEdited() bool {
	return !c.CreatedAt.IsZero() && c.UpdatedAt.After(c.CreatedAt)
}

// MarshalJSON adds a derived, read-only `edited` (IsEdited) to every
// serialised comment, so an agent reading JSON on either MCP transport sees
// the same marker the CLI table and the web timeline show without comparing
// timestamps itself (TASK-2695). It is not stored and not read back: Comment
// has no field for it, so a decode ignores it.
func (c Comment) MarshalJSON() ([]byte, error) {
	type plain Comment
	return json.Marshal(struct {
		plain
		Edited bool `json:"edited"`
	}{plain(c), c.IsEdited()})
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
