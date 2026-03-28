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
	CreatedAt   time.Time `json:"created_at"`
}

type ActivityListParams struct {
	Action string
	Actor  string
	Limit  int
	Offset int
}
