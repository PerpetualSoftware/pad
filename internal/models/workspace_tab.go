package models

import "time"

// WorkspaceTab is one workspace in a user's open set: a tab in the workspace
// tab bar (PLAN-3002 U1 / TASK-3256). Stored as a user_workspace_tabs row, one
// per (user, workspace). The whole row is server-side so the bar is the same
// on every device (PLAN-3002 Q1).
type WorkspaceTab struct {
	WorkspaceID string `json:"-"`
	// Workspace fields the bar renders, filled from the caller's visible set
	// at read time.
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	OwnerUsername string `json:"owner_username"`
	// IsGuest marks a workspace the caller reaches through grants only
	// (PLAN-3002 Q10: guests enter the open set like members, with the guest
	// marker).
	IsGuest bool `json:"is_guest"`

	Position int `json:"position"`
	// Ephemeral marks a tab a landing opened that has not been kept yet. At
	// most one per user.
	Ephemeral bool `json:"ephemeral"`
	// LastRoute is the in-workspace path the tab returns to, or empty.
	LastRoute string    `json:"last_route,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
