package models

import "time"

// Webhook represents a registered webhook endpoint that receives
// POST notifications when events occur in a workspace.
type Webhook struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	URL         string `json:"url"`
	// Secret is the HMAC signing secret in PLAINTEXT. It is encrypted at rest
	// (see internal/store/webhooks.go) and decrypted on read for internal use
	// (the dispatcher signs payloads with it). API responses return the raw
	// secret ONLY on creation; list/get responses mask it — see
	// internal/server/handlers_webhooks.go::maskWebhookSecret.
	Secret string `json:"secret,omitempty"`
	// HasSecret reports whether an HMAC secret is configured, without leaking
	// the value. Populated on reads so masked responses still signal presence.
	HasSecret       bool       `json:"has_secret"`
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
