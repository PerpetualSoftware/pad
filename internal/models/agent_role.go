package models

import "time"

// Role park modes stored in agent_roles.park_mode.
const (
	ParkModeNone = "none"
	ParkModeTop  = "top"
	ParkModeLine = "line"
)

// RolePark represents an active per-role park line on an agent role.
// Absent (nil) on AgentRole when park_mode is "none".
type RolePark struct {
	Mode        string     `json:"mode"`
	SortOrder   *int       `json:"sort_order"`
	SetAt       *time.Time `json:"set_at"`
	SetByUserID *string    `json:"set_by_user_id,omitempty"`
	SetBy       *string    `json:"set_by"`
	ParkSeq     int        `json:"park_seq"`
}

// AgentRole represents a workspace-scoped capability role for human-agent assignment.
// Roles describe what kind of work gets done (e.g. "Planner", "Implementer", "Reviewer"),
// not who or what tool does it. Items are assigned to a (user, role) pair.
type AgentRole struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Icon        string    `json:"icon"`
	Tools       string    `json:"tools"`
	SortOrder   int       `json:"sort_order"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// Park is non-nil when the role lane is parked (mode "top" or "line").
	Park *RolePark `json:"park,omitempty"`

	// Computed (not stored)
	ItemCount int `json:"item_count,omitempty"`
}

// AgentRoleCreate is the input for creating a new agent role.
type AgentRoleCreate struct {
	Name        string `json:"name"`
	Slug        string `json:"slug,omitempty"` // auto-generated from name if empty
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Tools       string `json:"tools,omitempty"`
}

// AgentRoleUpdate is the input for updating an existing agent role.
type AgentRoleUpdate struct {
	Name        *string `json:"name,omitempty"`
	Slug        *string `json:"slug,omitempty"`
	Description *string `json:"description,omitempty"`
	Icon        *string `json:"icon,omitempty"`
	Tools       *string `json:"tools,omitempty"`
	SortOrder   *int    `json:"sort_order,omitempty"`
}
