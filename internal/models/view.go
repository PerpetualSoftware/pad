package models

import "time"

type View struct {
	ID           string    `json:"id"`
	WorkspaceID  string    `json:"workspace_id"`
	CollectionID *string   `json:"collection_id,omitempty"`
	Name         string    `json:"name"`
	Slug         string    `json:"slug"`
	ViewType     string    `json:"view_type"` // list, board, table
	Config       string    `json:"config"`    // JSON
	SortOrder    int       `json:"sort_order"`
	IsDefault    bool      `json:"is_default"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type ViewCreate struct {
	CollectionID *string `json:"collection_id,omitempty"`
	Name         string  `json:"name"`
	Slug         string  `json:"slug,omitempty"`
	ViewType     string  `json:"view_type"`
	Config       string  `json:"config,omitempty"`
}

type ViewUpdate struct {
	Name      *string `json:"name,omitempty"`
	ViewType  *string `json:"view_type,omitempty"`
	Config    *string `json:"config,omitempty"`
	SortOrder *int    `json:"sort_order,omitempty"`
}
