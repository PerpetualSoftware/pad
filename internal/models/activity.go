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

// TimelineEntry represents a single entry in the unified item timeline.
// It wraps one of: a comment, an activity, or a version.
type TimelineEntry struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // "comment", "activity", "version"
	CreatedAt time.Time `json:"created_at"`
	Actor     string    `json:"actor"`
	ActorName string    `json:"actor_name,omitempty"`
	Source    string    `json:"source"`
	Comment   *Comment  `json:"comment,omitempty"`
	Activity  *Activity `json:"activity,omitempty"`
	Version   *Version  `json:"version,omitempty"`
}

// TimelineResponse is the paginated response from the timeline endpoint.
type TimelineResponse struct {
	Entries []TimelineEntry `json:"entries"`
	Total   int             `json:"total"`
}
