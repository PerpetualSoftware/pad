package models

import "time"

// Valid actions
var ValidActions = []string{
	"created", "updated", "archived", "restored", "moved", "read", "searched",
}

type Activity struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	DocumentID  string    `json:"document_id,omitempty"`
	Action      string    `json:"action"`
	Actor       string    `json:"actor"`
	Source      string    `json:"source"`
	Metadata    string    `json:"metadata,omitempty"` // JSON
	UserID      string    `json:"user_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`

	// Enrichment fields — populated by handlers, not stored in DB
	ItemTitle      string `json:"item_title,omitempty"`
	ItemSlug       string `json:"item_slug,omitempty"`
	CollectionSlug string `json:"collection_slug,omitempty"`
	ActorName      string `json:"actor_name,omitempty"`
}

type ActivityListParams struct {
	Action string
	Actor  string
	Source string
	Limit  int
	Offset int
}
