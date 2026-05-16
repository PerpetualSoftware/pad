package models

import (
	"encoding/json"
	"errors"
	"time"
)

type FieldDef struct {
	Key             string   `json:"key"`
	Label           string   `json:"label"`
	Type            string   `json:"type"` // text, number, select, multi_select, date, checkbox, url, relation, json
	Options         []string `json:"options,omitempty"`
	TerminalOptions []string `json:"terminal_options,omitempty"` // for select fields: which options represent a terminal/finalized state
	Default         any      `json:"default,omitempty"`
	Required        bool     `json:"required,omitempty"`
	Computed        bool     `json:"computed,omitempty"`
	Collection      string   `json:"collection,omitempty"`   // for relation type
	Suffix          string   `json:"suffix,omitempty"`       // for number type display
	Pattern         string   `json:"pattern,omitempty"`      // optional ECMAScript-style regex applied to text values; empty = no pattern check
	UniqueScope     string   `json:"unique_scope,omitempty"` // "workspace_collection" enforces uniqueness within a collection (non-empty values only); empty = no uniqueness
}

type CollectionSchema struct {
	Fields []FieldDef `json:"fields"`
}

// QuickAction defines a prompt template that can be triggered from the UI.
type QuickAction struct {
	Label  string `json:"label"`          // display label for the button
	Prompt string `json:"prompt"`         // prompt template with {ref}, {title}, {status}, etc.
	Scope  string `json:"scope"`          // "item" or "collection"
	Icon   string `json:"icon,omitempty"` // optional emoji/icon
}

type CollectionSettings struct {
	Layout          string        `json:"layout,omitempty"`       // fields-primary, content-primary, balanced
	DefaultView     string        `json:"default_view,omitempty"` // list, board, table
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
	Settings    string     `json:"settings"` // JSON string in DB
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

// ErrInvalidSettingsType is returned by CollectionUpdate.UnmarshalJSON
// and CollectionCreate.UnmarshalJSON when the inbound `settings` value is
// neither a JSON object nor a JSON-encoded string. IDEA-1488: handler-
// layer shape validation ceiling, paired with the IDEA-1484 NOT NULL
// floor on collections.settings. Mirrors item.go's
// ErrInvalidFieldsType / ErrInvalidTagsType and view.go's
// ErrInvalidConfigType.
var ErrInvalidSettingsType = errors.New(`"settings" must be a JSON object or a JSON-encoded string`)

// UnmarshalJSON for CollectionCreate accepts `settings` either as the
// canonical JSON-encoded string shape (matches models.Collection.Settings
// storage) OR as the natural nested object shape any reasonable HTTP
// client would send. Mirrors ItemCreate.UnmarshalJSON; see
// flexJSONToString in item.go for the shape contract.
func (c *CollectionCreate) UnmarshalJSON(data []byte) error {
	type alias CollectionCreate
	aux := struct {
		Settings json.RawMessage `json:"settings,omitempty"`
		*alias
	}{alias: (*alias)(c)}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	if settingsStr, err := flexJSONToString(aux.Settings, '{', ErrInvalidSettingsType); err != nil {
		return err
	} else if settingsStr != nil {
		c.Settings = *settingsStr
	}

	return nil
}

// UnmarshalJSON for CollectionUpdate accepts `settings` either as the
// canonical JSON-encoded string shape OR as the natural nested object
// shape. Mirrors ItemUpdate.UnmarshalJSON; the struct field stays
// `*string` and downstream consumers are unchanged.
//
// IDEA-1488: closes the shape-validation gap that IDEA-1484's NOT NULL
// floor doesn't cover. After this lands, the writer-side store coercion
// at collections.go:248 only sees empty-string-or-valid-object input.
func (u *CollectionUpdate) UnmarshalJSON(data []byte) error {
	type alias CollectionUpdate
	aux := struct {
		Settings json.RawMessage `json:"settings,omitempty"`
		*alias
	}{alias: (*alias)(u)}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	settingsStr, err := flexJSONToString(aux.Settings, '{', ErrInvalidSettingsType)
	if err != nil {
		return err
	}
	u.Settings = settingsStr

	return nil
}
