package models

import "time"

type FieldDef struct {
	Key             string   `json:"key"`
	Label           string   `json:"label"`
	Type            string   `json:"type"`                      // text, number, select, multi_select, date, checkbox, url, relation
	Options         []string `json:"options,omitempty"`
	TerminalOptions []string `json:"terminal_options,omitempty"` // for select fields: which options represent a terminal/finalized state
	Default         any      `json:"default,omitempty"`
	Required        bool     `json:"required,omitempty"`
	Computed        bool     `json:"computed,omitempty"`
	Collection      string   `json:"collection,omitempty"`      // for relation type
	Suffix          string   `json:"suffix,omitempty"`          // for number type display
}

type CollectionSchema struct {
	Fields []FieldDef `json:"fields"`
}

// QuickAction defines a prompt template that can be triggered from the UI.
type QuickAction struct {
	Label    string `json:"label"`              // display label for the button
	Prompt   string `json:"prompt"`             // prompt template with {ref}, {title}, {status}, etc.
	Scope    string `json:"scope"`              // "item" or "collection"
	Icon     string `json:"icon,omitempty"`     // optional emoji/icon
}

type CollectionSettings struct {
	Layout          string        `json:"layout,omitempty"`           // fields-primary, content-primary, balanced
	DefaultView     string        `json:"default_view,omitempty"`     // list, board, table
	BoardGroupBy    string        `json:"board_group_by,omitempty"`
	ListSortBy      string        `json:"list_sort_by,omitempty"`
	ListGroupBy     string        `json:"list_group_by,omitempty"`
	QuickActions    []QuickAction `json:"quick_actions,omitempty"`
	ContentTemplate string        `json:"content_template,omitempty"` // markdown template for new items
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
	IsSystem    bool       `json:"is_system"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`

	// Computed (not stored)
	ItemCount       int `json:"item_count"`
	ActiveItemCount int `json:"active_item_count"`
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
	IsSystem    bool   `json:"is_system,omitempty"`
}

// FieldMigration describes a bulk update to apply to existing items when
// a collection schema changes (e.g. renaming select options).
type FieldMigration struct {
	Field         string            `json:"field"`                    // field key to migrate
	RenameOptions map[string]string `json:"rename_options,omitempty"` // old_value → new_value
}

type CollectionUpdate struct {
	Name        *string          `json:"name,omitempty"`
	Prefix      *string          `json:"prefix,omitempty"`
	Icon        *string          `json:"icon,omitempty"`
	Description *string          `json:"description,omitempty"`
	Schema      *string          `json:"schema,omitempty"`
	Settings    *string          `json:"settings,omitempty"`
	SortOrder   *int             `json:"sort_order,omitempty"`
	Migrations  []FieldMigration `json:"migrations,omitempty"`
}
