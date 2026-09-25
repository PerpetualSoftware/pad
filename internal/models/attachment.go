package models

import "time"

// Attachment models a single uploaded blob in the attachments table.
//
// See [[Attachments — architecture & migration design]] (DOC-865). Storage is
// content-addressed: StorageKey is "<backend>:<sha256>" so the driver registry
// resolves the prefix to a concrete AttachmentStore. Item content references
// attachments by UUID via "pad-attachment:<id>" — never by URL — so a backend
// migration (FS → S3) leaves item content untouched.
//
// Derived blobs (thumbnails) are rows of their own with ParentID pointing at
// the original and Variant set to one of "thumb-sm" / "thumb-md". They count
// against the workspace storage quota — they are real bytes on disk.
type Attachment struct {
	ID          string  `json:"id"`
	WorkspaceID string  `json:"workspace_id"`
	ItemID      *string `json:"item_id,omitempty"` // nil = orphan, eligible for GC
	UploadedBy  string  `json:"uploaded_by"`
	StorageKey  string  `json:"storage_key"` // "<backend>:<hash>"
	ContentHash string  `json:"content_hash"`
	MimeType    string  `json:"mime_type"`
	SizeBytes   int64   `json:"size_bytes"`
	Filename    string  `json:"filename"`
	// FilenameSource says where Filename came from: caller, normalised,
	// substituted, derived, or unknown for a row written before it was
	// recorded (BUG-2819; values in attachments.FilenameSource). A bundle
	// import stores the BUNDLE's claim, not an observation by this server,
	// which is acceptable only because nothing branches on it for trust:
	// the attachment's identity is its ID.
	FilenameSource string  `json:"filename_source"`
	Width          *int    `json:"width,omitempty"`  // images only
	Height         *int    `json:"height,omitempty"` // images only
	ParentID       *string `json:"parent_id,omitempty"`
	Variant        *string `json:"variant,omitempty"` // "original" | "thumb-sm" | "thumb-md"

	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// Attachment variants.
const (
	AttachmentVariantOriginal = "original"
	AttachmentVariantThumbSm  = "thumb-sm"
	AttachmentVariantThumbMd  = "thumb-md"
)
