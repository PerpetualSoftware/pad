package models

import "time"

type Workspace struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Slug        string            `json:"slug"`
	Description string            `json:"description"`
	Settings    string            `json:"settings"` // JSON
	SortOrder   int               `json:"sort_order"`
	Context     *WorkspaceContext `json:"context,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	DeletedAt   *time.Time        `json:"deleted_at,omitempty"`
}

type WorkspaceCreate struct {
	Name        string            `json:"name"`
	Slug        string            `json:"slug,omitempty"` // auto-generated if empty
	Description string            `json:"description,omitempty"`
	Settings    string            `json:"settings,omitempty"`
	Context     *WorkspaceContext `json:"context,omitempty"`
	Template    string            `json:"template,omitempty"` // workspace template name (e.g. "startup", "scrum", "product")
}

type WorkspaceUpdate struct {
	Name        *string           `json:"name,omitempty"`
	Description *string           `json:"description,omitempty"`
	Settings    *string           `json:"settings,omitempty"`
	Context     *WorkspaceContext `json:"context,omitempty"`
}
