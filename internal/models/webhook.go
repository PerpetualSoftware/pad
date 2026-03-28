package models

import "time"

// Webhook represents a registered webhook endpoint that receives
// POST notifications when events occur in a workspace.
type Webhook struct {
	ID              string     `json:"id"`
	WorkspaceID     string     `json:"workspace_id"`
	URL             string     `json:"url"`
	Secret          string     `json:"secret,omitempty"`
	Events          string     `json:"events"`
	Active          bool       `json:"active"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	LastTriggeredAt *time.Time `json:"last_triggered_at,omitempty"`
	FailureCount    int        `json:"failure_count"`
}

// WebhookCreate is the input for registering a new webhook.
type WebhookCreate struct {
	URL    string `json:"url"`
	Secret string `json:"secret,omitempty"`
	Events string `json:"events,omitempty"` // JSON array of event types, defaults to ["*"]
}
