package models

import (
	"encoding/json"
	"errors"
	"time"
)

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

// ErrInvalidConfigType is returned by ViewUpdate.UnmarshalJSON and
// ViewCreate.UnmarshalJSON when the inbound `config` value is neither a
// JSON object nor a JSON-encoded string. IDEA-1488: handler-layer shape
// validation ceiling, paired with the IDEA-1486 NOT NULL floor. Wire
// handlers surface the sentinel's message verbatim so callers see a
// clean domain-level error instead of leaked Go internals (mirrors
// item.go's ErrInvalidFieldsType / ErrInvalidTagsType).
var ErrInvalidConfigType = errors.New(`"config" must be a JSON object or a JSON-encoded string`)

// UnmarshalJSON for ViewCreate accepts `config` either as the canonical
// JSON-encoded string shape (matches models.View.Config storage) OR as
// the natural nested object shape any reasonable HTTP client would send.
// Mirrors ItemCreate.UnmarshalJSON; see flexJSONToString in item.go for
// the shape contract.
func (c *ViewCreate) UnmarshalJSON(data []byte) error {
	type alias ViewCreate
	aux := struct {
		Config json.RawMessage `json:"config,omitempty"`
		*alias
	}{alias: (*alias)(c)}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	if configStr, err := flexJSONToString(aux.Config, '{', ErrInvalidConfigType); err != nil {
		return err
	} else if configStr != nil {
		c.Config = *configStr
	}

	return nil
}

// UnmarshalJSON for ViewUpdate accepts `config` either as the canonical
// JSON-encoded string shape OR as the natural nested object shape any
// reasonable HTTP client would send. Mirrors ItemUpdate.UnmarshalJSON;
// the struct field stays `*string` and downstream consumers (store
// writes, web/CLI) are unchanged — we just normalize the wire input.
//
// IDEA-1488: closes the shape-validation gap that IDEA-1486's NOT NULL
// floor doesn't cover. After this lands, the writer-side store coercion
// at views.go:152 only sees empty-string-or-valid-object input; the
// "is it a JSON object" assertion sits at the handler boundary.
func (u *ViewUpdate) UnmarshalJSON(data []byte) error {
	type alias ViewUpdate
	aux := struct {
		Config json.RawMessage `json:"config,omitempty"`
		*alias
	}{alias: (*alias)(u)}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	configStr, err := flexJSONToString(aux.Config, '{', ErrInvalidConfigType)
	if err != nil {
		return err
	}
	u.Config = configStr

	return nil
}
