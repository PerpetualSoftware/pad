package models

import (
	"fmt"
	"time"
)

type Item struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	CollectionID   string     `json:"collection_id"`
	Title          string     `json:"title"`
	Slug           string     `json:"slug"`
	Ref            string     `json:"ref,omitempty"` // computed: e.g. "TASK-5", "BUG-8"
	Content        string     `json:"content"`
	Fields         string     `json:"fields"` // JSON string
	Tags           string     `json:"tags"`   // JSON array string
	Pinned         bool       `json:"pinned"`
	SortOrder      int        `json:"sort_order"`
	ParentID       *string    `json:"parent_id,omitempty"`
	CreatedBy      string     `json:"created_by"`
	LastModifiedBy string     `json:"last_modified_by"`
	Source         string     `json:"source"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`

	// Auto-assigned sequential number within collection
	ItemNumber *int `json:"item_number,omitempty"`

	// Populated by joins (not stored)
	CollectionSlug   string `json:"collection_slug,omitempty"`
	CollectionName   string `json:"collection_name,omitempty"`
	CollectionIcon   string `json:"collection_icon,omitempty"`
	CollectionPrefix string `json:"collection_prefix,omitempty"`
}

// ComputeRef sets the Ref field from CollectionPrefix and ItemNumber.
// Call this after populating the item from a database query.
func (item *Item) ComputeRef() {
	if item.CollectionPrefix != "" && item.ItemNumber != nil {
		item.Ref = fmt.Sprintf("%s-%d", item.CollectionPrefix, *item.ItemNumber)
	}
}

type ItemCreate struct {
	Title     string  `json:"title"`
	Content   string  `json:"content,omitempty"`
	Fields    string  `json:"fields,omitempty"`
	Tags      string  `json:"tags,omitempty"`
	Pinned    bool    `json:"pinned,omitempty"`
	ParentID  *string `json:"parent_id,omitempty"`
	CreatedBy string  `json:"created_by,omitempty"`
	Source    string  `json:"source,omitempty"`
}

type ItemUpdate struct {
	Title          *string `json:"title,omitempty"`
	Content        *string `json:"content,omitempty"`
	Fields         *string `json:"fields,omitempty"`
	Tags           *string `json:"tags,omitempty"`
	Pinned         *bool   `json:"pinned,omitempty"`
	SortOrder      *int    `json:"sort_order,omitempty"`
	ParentID       *string `json:"parent_id,omitempty"`
	LastModifiedBy string  `json:"last_modified_by,omitempty"`
	Source         string  `json:"source,omitempty"`
	ChangeSummary  string  `json:"change_summary,omitempty"`
}

type ItemListParams struct {
	CollectionSlug  string
	Fields          map[string]string // field filters: key=value
	Sort            string            // e.g. "priority:desc,created_at:asc"
	GroupBy         string
	Search          string // FTS query
	ParentID        string
	Tag             string
	IncludeArchived bool
	Limit           int
	Offset          int
}

type ItemLink struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	SourceID    string    `json:"source_id"`
	TargetID    string    `json:"target_id"`
	LinkType    string    `json:"link_type"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`

	// Populated by joins
	SourceTitle string `json:"source_title,omitempty"`
	TargetTitle string `json:"target_title,omitempty"`
}

type ItemLinkCreate struct {
	TargetID  string `json:"target_id"`
	LinkType  string `json:"link_type,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
}
