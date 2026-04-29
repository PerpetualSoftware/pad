package models

// WorkspaceExport is the complete portable representation of a workspace.
type WorkspaceExport struct {
	Version      int                 `json:"version"` // Export format version (1)
	ExportedAt   string              `json:"exported_at"`
	Workspace    WorkspaceExportMeta `json:"workspace"`
	Collections  []CollectionExport  `json:"collections"`
	Items        []ItemExport        `json:"items"`
	Comments     []CommentExport     `json:"comments,omitempty"`
	ItemLinks    []ItemLinkExport    `json:"item_links,omitempty"`
	ItemVersions []ItemVersionExport `json:"item_versions,omitempty"`
}

// AttachmentManifestEntry describes one attachment blob in the
// tar-bundle export's attachments/manifest.json. The bundle layout is:
//
//	pad-export.json                     # the WorkspaceExport above
//	attachments/manifest.json           # uuid → AttachmentManifestEntry
//	attachments/<uuid>.<ext>            # the actual blob bytes
//
// Thumbnails are NOT bundled — they're re-derived on import via the
// existing thumbnail pipeline. ParentID and Variant therefore stay
// nil/empty for every entry shipped in a bundle, but the fields are
// kept here for forward compatibility (e.g. if a future format
// version starts shipping pre-derived variants).
type AttachmentManifestEntry struct {
	ID          string `json:"id"`           // attachment UUID (the original)
	Filename    string `json:"filename"`     // user-facing filename
	MIME        string `json:"mime"`         // canonical MIME from upload time
	SizeBytes   int64  `json:"size_bytes"`   // bytes on disk (matches the blob)
	ContentHash string `json:"content_hash"` // sha256 hex, the dedupe key
	Width       *int   `json:"width,omitempty"`
	Height      *int   `json:"height,omitempty"`
	ItemID      string `json:"item_id,omitempty"` // exporter's item UUID; remapped on import
	ParentID    string `json:"parent_id,omitempty"`
	Variant     string `json:"variant,omitempty"`
	UploadedBy  string `json:"uploaded_by"`
	CreatedAt   string `json:"created_at"`
}

// AttachmentManifest is the top-level shape of attachments/manifest.json
// inside an export bundle. Wraps a list of entries plus a small
// "schema" version so the import path can validate / migrate.
type AttachmentManifest struct {
	Version int                       `json:"version"` // manifest version, 1
	Entries []AttachmentManifestEntry `json:"entries"`
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
	IsSystem    bool   `json:"is_system"`
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
