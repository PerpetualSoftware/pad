package models

import "time"

type FieldDef struct {
	Key        string   `json:"key"`
	Label      string   `json:"label"`
	Type       string   `json:"type"`                 // text, number, select, multi_select, date, checkbox, url, relation
	Options    []string `json:"options,omitempty"`
	Default    any      `json:"default,omitempty"`
	Required   bool     `json:"required,omitempty"`
	Computed   bool     `json:"computed,omitempty"`
	Collection string   `json:"collection,omitempty"` // for relation type
	Suffix     string   `json:"suffix,omitempty"`     // for number type display
}

type CollectionSchema struct {
	Fields []FieldDef `json:"fields"`
}

type CollectionSettings struct {
	Layout       string `json:"layout,omitempty"`        // fields-primary, content-primary, balanced
	DefaultView  string `json:"default_view,omitempty"`  // list, board, table
	BoardGroupBy string `json:"board_group_by,omitempty"`
	ListSortBy   string `json:"list_sort_by,omitempty"`
	ListGroupBy  string `json:"list_group_by,omitempty"`
}

type Collection struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	Name        string     `json:"name"`
	Slug        string     `json:"slug"`
	Icon        string     `json:"icon"`
	Description string     `json:"description"`
	Schema      string     `json:"schema"`   // JSON string in DB, parsed via methods
	Settings    string     `json:"settings"`  // JSON string in DB
	Prefix      string     `json:"prefix"`
	SortOrder   int        `json:"sort_order"`
	IsDefault   bool       `json:"is_default"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`

	// Computed (not stored)
	ItemCount int `json:"item_count,omitempty"`
}

type CollectionCreate struct {
	Name        string `json:"name"`
	Slug        string `json:"slug,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Description string `json:"description,omitempty"`
	Schema      string `json:"schema,omitempty"`
	Settings    string `json:"settings,omitempty"`
	IsDefault   bool   `json:"is_default,omitempty"`
}

type CollectionUpdate struct {
	Name        *string `json:"name,omitempty"`
	Prefix      *string `json:"prefix,omitempty"`
	Icon        *string `json:"icon,omitempty"`
	Description *string `json:"description,omitempty"`
	Schema      *string `json:"schema,omitempty"`
	Settings    *string `json:"settings,omitempty"`
	SortOrder   *int    `json:"sort_order,omitempty"`
}
