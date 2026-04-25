package models

import "time"

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
