package models

import "time"

// ShareLink represents a shareable link to an item or collection.
// Tokens are hashed at rest; the raw token is returned only once on creation.
type ShareLink struct {
	ID              string     `json:"id"`
	TokenHash       string     `json:"-"`                              // Never serialized
	Token           string     `json:"token,omitempty"`                // Only set on creation response
	TargetType      string     `json:"target_type"`                    // "item" or "collection"
	TargetID        string     `json:"target_id"`
	WorkspaceID     string     `json:"workspace_id"`
	Permission      string     `json:"permission"`                     // "view" or "edit"
	CreatedBy       string     `json:"created_by"`
	PasswordHash    *string    `json:"-"`                              // Never serialized
	HasPassword     bool       `json:"has_password"`                   // Derived: password_hash IS NOT NULL
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	MaxViews        *int       `json:"max_views,omitempty"`
	RequireAuth     bool       `json:"require_auth"`
	RestrictToEmail string     `json:"restrict_to_email,omitempty"`
	ViewCount       int        `json:"view_count"`
	UniqueViewers   int        `json:"unique_viewers"`
	LastViewedAt    *time.Time `json:"last_viewed_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`

	// Derived (populated by handlers)
	URL         string `json:"url,omitempty"`          // Full share URL (e.g. /s/{token})
	TargetTitle string `json:"target_title,omitempty"` // Title of the shared item/collection
}

// ShareLinkView records a single view of a share link.
type ShareLinkView struct {
	ID                string    `json:"id"`
	ShareLinkID       string    `json:"share_link_id"`
	ViewerFingerprint string    `json:"viewer_fingerprint,omitempty"`
	ViewerUserID      string    `json:"viewer_user_id,omitempty"`
	ViewedAt          time.Time `json:"viewed_at"`
}
