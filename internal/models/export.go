package models

// WorkspaceExport is the complete portable representation of a workspace.
type WorkspaceExport struct {
	Version      int                 `json:"version"`    // Export format version (1)
	ExportedAt   string              `json:"exported_at"`
	Workspace    WorkspaceExportMeta `json:"workspace"`
	Collections  []CollectionExport  `json:"collections"`
	Items        []ItemExport        `json:"items"`
	Comments     []CommentExport     `json:"comments,omitempty"`
	ItemLinks    []ItemLinkExport    `json:"item_links,omitempty"`
	ItemVersions []ItemVersionExport `json:"item_versions,omitempty"`
}

// WorkspaceExportMeta holds workspace metadata for export.
type WorkspaceExportMeta struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	Settings    string `json:"settings"`
}

// CollectionExport holds a collection's data for export.
type CollectionExport struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	Schema      string `json:"schema"`
	Settings    string `json:"settings"`
	Prefix      string `json:"prefix"`
	SortOrder   int    `json:"sort_order"`
	IsDefault   bool   `json:"is_default"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// ItemExport holds an item's data for export.
type ItemExport struct {
	ID             string `json:"id"`
	CollectionID   string `json:"collection_id"`
	Title          string `json:"title"`
	Slug           string `json:"slug"`
	Content        string `json:"content"`
	Fields         string `json:"fields"`
	Tags           string `json:"tags"`
	Pinned         bool   `json:"pinned"`
	SortOrder      int    `json:"sort_order"`
	ParentID       string `json:"parent_id,omitempty"`
	CreatedBy      string `json:"created_by"`
	LastModifiedBy string `json:"last_modified_by"`
	Source         string `json:"source"`
	ItemNumber     int    `json:"item_number"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// CommentExport holds a comment's data for export.
type CommentExport struct {
	ID        string `json:"id"`
	ItemID    string `json:"item_id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedBy string `json:"created_by"`
	Source    string `json:"source"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ItemLinkExport holds an item link's data for export.
type ItemLinkExport struct {
	ID        string `json:"id"`
	SourceID  string `json:"source_id"`
	TargetID  string `json:"target_id"`
	LinkType  string `json:"link_type"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
}

// ItemVersionExport holds an item version's data for export.
type ItemVersionExport struct {
	ID            string `json:"id"`
	ItemID        string `json:"item_id"`
	Content       string `json:"content"`
	ChangeSummary string `json:"change_summary"`
	CreatedBy     string `json:"created_by"`
	Source        string `json:"source"`
	IsDiff        bool   `json:"is_diff"`
	CreatedAt     string `json:"created_at"`
}
