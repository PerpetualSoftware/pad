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
	// Reminders round-trip with the workspace (IDEA-2641). They are
	// item-scoped workspace CONTENT, like links and versions, not per-user
	// state like stars and watches — which is the line this list has always
	// drawn, and it puts reminders on the exported side of it. Without them a
	// backup/restore or a SQLite→Postgres migration silently loses every
	// pending reminder, and "silently" is the part that matters: nothing in
	// the destination would show that anything was dropped.
	Reminders []ReminderExport `json:"reminders,omitempty"`
}

// ReminderExport is one item reminder in a workspace bundle.
//
// The LIFECYCLE MARKS ARE CARRIED, not reset. A fired-and-unacknowledged
// reminder is still owed to whoever armed it, so it arrives pending on the
// destination; an armed one whose instant has passed fires once on the first
// tick there, which is the same thing that would have happened had the
// workspace never moved. Re-arming everything on import would be inventing a
// new schedule the user did not set.
type ReminderExport struct {
	ItemID    string `json:"item_id"`
	RemindAt  string `json:"remind_at"`
	FiredAt   string `json:"fired_at,omitempty"`
	AckedAt   string `json:"acked_at,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
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
	// Traits carries the collection's kernel-trait declarations (SPEC-5).
	// omitempty so an archive from a deployment where nothing declares traits
	// omits the key entirely rather than carrying a noise "{}" on every
	// collection, and so an import of an older archive is unambiguous (absent,
	// not empty).
	//
	// This does NOT make a pre-TASK-2657 archive round-trip byte-identically:
	// import defaults a missing value to "{}" in the column, and any collection
	// the migration then backfills re-exports WITH declarations. Round-tripping
	// an old archive through a current deployment is expected to gain traits —
	// that is the backfill working, not export drift.
	Traits    string `json:"traits,omitempty"`
	Prefix    string `json:"prefix"`
	SortOrder int    `json:"sort_order"`
	IsDefault bool   `json:"is_default"`
	IsSystem  bool   `json:"is_system"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// DeletedAt carries a collection's soft-delete mark, empty for a live
	// collection (BUG-2884).
	//
	// The bundle used to skip soft-deleted collections while exporting every
	// live ITEM, and DeleteCollection soft-deletes only the collection row —
	// its items keep deleted_at IS NULL and stay reachable by ref, by id, and
	// through search, because no item-bearing read joins collection liveness.
	// So the two sections were filtered by different rules and the bundle
	// named a collection it did not carry; ImportWorkspace then dropped those
	// items on its orphan gate, silently. Since pad db migrate is
	// ExportWorkspace piped into ImportWorkspace, that was live data lost on a
	// SQLite→Postgres migration, not just a lossy backup.
	//
	// Filtering the items to live collections instead would have made the
	// migration DELETE reachable rows, so the bundle carries the archived
	// collection and the importer reproduces the archive.
	//
	// omitempty for the same reason as Traits: an archive from a workspace
	// with nothing deleted omits the key rather than carrying a noise "" on
	// every collection. Absent decodes to "" and MUST mean live — that is what
	// keeps every archive written before this field importable.
	DeletedAt string `json:"deleted_at,omitempty"`
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
