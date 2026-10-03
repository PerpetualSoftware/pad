package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// App attachment writes and reads inside a FencedTx (SPEC-6 U7, DOC-3371 §4;
// TASK-3396).
//
// An app upload is created BOUND to one companion item that this install
// created, in one transaction, with uploaded_by = the acting identity and
// via_app = the install. It writes the attachments row and nothing else: no
// item row (no seq, updated_at or etag change) and no last_referenced_at
// stamp (DOC-3371 day-85 revisions).
//
// LOCKS. The workspace seq lock comes first, as in every fenced write: it
// pins the companion check against a concurrent move, and it is what
// serializes the cap recount. Every app writer of via_app rows holds it, so
// two uploads for one install cannot both pass a recount that only one of
// them fits. The spec names a FOR UPDATE on the install row for this, but the
// fence already holds that row FOR SHARE (BeginFenced), and two uploads each
// upgrading a share lock to an exclusive one deadlock; the seq lock gives the
// same serialization without the upgrade.

// App attachment caps (DOC-3371 §4 Limits). Variants are exempt.
const (
	AppAttachmentMaxFiles int64 = 2000
	AppAttachmentMaxBytes int64 = 1 << 30
)

var (
	// ErrAppAttachmentLimit: the install is at its file or byte cap. It
	// carries no totals.
	ErrAppAttachmentLimit = errors.New("app write: attachment limit reached")
	// ErrAppAttachmentNotFound: the attachment is not visible to the app
	// (missing, deleted, foreign, or bound to a non-companion item). One
	// error for all of them.
	ErrAppAttachmentNotFound = errors.New("app read: attachment not found")
)

// FencedAttachmentCreate is an app upload whose blob is already canonical.
type FencedAttachmentCreate struct {
	ItemID         string
	Actor          FencedActor
	StorageKey     string
	ContentHash    string
	MimeType       string
	Size           int64
	Filename       string
	FilenameSource string
	Width, Height  *int
}

// requireUploadTarget is the upload write gate, the same as an app field
// edit's: the seq lock, then a live companion item created by this install.
func (f *FencedTx) requireUploadTarget(itemID string) error {
	if err := f.LockWorkspaceSeq(); err != nil {
		return err
	}
	if _, err := f.requireCompanionItem(itemID); err != nil {
		return err
	}
	return f.requireCreatedHere(itemID)
}

// attachmentUsage recounts the install's live ORIGINAL attachment rows. The
// caller holds the seq lock (see LOCKS above).
func (f *FencedTx) attachmentUsage() (files, bytes int64, err error) {
	err = f.tx.QueryRow(f.s.q(`SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM attachments
		WHERE via_app = ? AND parent_id IS NULL AND deleted_at IS NULL`), f.installID).Scan(&files, &bytes)
	if err != nil {
		return 0, 0, fmt.Errorf("fenced attachment usage: %w", err)
	}
	return files, bytes, nil
}

// AttachmentAdmissionUsage runs an upload's write gate before any byte is
// read and returns the install's current usage (live original rows), for the
// caller's admission check. It writes nothing; the caller compares, adding
// its in-process reservations, WHILE this transaction still holds the seq
// lock, then rolls back. Reading the reservations under the lock is what
// keeps an upload from being counted twice: an upload releases its
// reservation inside its own row transaction, also under the lock, so a
// concurrent admission sees it as either a reservation or a row, never both
// (codex round 1).
func (f *FencedTx) AttachmentAdmissionUsage(itemID string, actor FencedActor) (files, bytes int64, err error) {
	if !actor.valid() {
		return 0, 0, ErrAppBadActor
	}
	if err := f.requireUploadTarget(itemID); err != nil {
		return 0, 0, err
	}
	return f.attachmentUsage()
}

// CreateAttachment inserts the app upload's row: the gate again, the EXACT
// cap check on rows at the actual size, then the INSERT. The blob is already
// canonical; if this refuses, the caller leaves the blob for orphan GC.
func (f *FencedTx) CreateAttachment(in FencedAttachmentCreate) (*models.Attachment, error) {
	if !in.Actor.valid() {
		return nil, ErrAppBadActor
	}
	if in.StorageKey == "" || in.ContentHash == "" || in.Size <= 0 {
		return nil, fmt.Errorf("fenced attachment: incomplete blob reference")
	}
	if err := f.requireUploadTarget(in.ItemID); err != nil {
		return nil, err
	}
	files, bytes, err := f.attachmentUsage()
	if err != nil {
		return nil, err
	}
	if files+1 > AppAttachmentMaxFiles || bytes+in.Size > AppAttachmentMaxBytes {
		return nil, ErrAppAttachmentLimit
	}
	source := in.FilenameSource
	if source == "" {
		source = "unknown"
	}
	id := newID()
	ts := now()
	if _, err := f.tx.Exec(f.s.q(`
		INSERT INTO attachments (id, workspace_id, item_id, uploaded_by, storage_key, content_hash,
		                         mime_type, size_bytes, filename, width, height, parent_id, variant,
		                         created_at, deleted_at, filename_source, imported, via_app)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, NULL, ?, 0, ?)`),
		id, f.workspaceID, in.ItemID, in.Actor.UserID, in.StorageKey, in.ContentHash,
		in.MimeType, in.Size, in.Filename, in.Width, in.Height, ts, source, f.installID,
	); err != nil {
		return nil, fmt.Errorf("fenced attachment insert: %w", err)
	}
	return f.visibleAttachment(id)
}

// VisibleAttachment is the app read rule: a live attachment in the install's
// workspace, bound to a live item whose CURRENT collection is a companion.
// A variant qualifies through its own item_id, which it inherits from its
// parent. Every other case is ErrAppAttachmentNotFound.
func (f *FencedTx) VisibleAttachment(id string) (*models.Attachment, error) {
	if f.unusable() {
		return nil, ErrFenceClosed
	}
	return f.visibleAttachment(id)
}

// VisibleAttachmentVariant is the variant row of a visible original, by
// variant key, under the same rule; (nil, nil) when that variant does not
// exist yet, so the caller can fall back to the original as the human
// download does.
func (f *FencedTx) VisibleAttachmentVariant(parentID, variant string) (*models.Attachment, error) {
	if f.unusable() {
		return nil, ErrFenceClosed
	}
	var id string
	err := f.tx.QueryRow(f.s.q(`SELECT id FROM attachments
		WHERE parent_id = ? AND variant = ? AND workspace_id = ? AND deleted_at IS NULL`),
		parentID, variant, f.workspaceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fenced attachment variant: %w", err)
	}
	return f.visibleAttachment(id)
}

func (f *FencedTx) visibleAttachment(id string) (*models.Attachment, error) {
	a, err := scanAttachment(f.tx.QueryRow(f.s.q(`SELECT `+attachmentColumns+` FROM attachments
		WHERE id = ? AND workspace_id = ? AND deleted_at IS NULL`), id, f.workspaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAppAttachmentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("fenced attachment read: %w", err)
	}
	if a.ItemID == nil || *a.ItemID == "" {
		return nil, ErrAppAttachmentNotFound
	}
	if _, err := f.requireCompanionItem(*a.ItemID); err != nil {
		if errors.Is(err, ErrNotCompanion) {
			return nil, ErrAppAttachmentNotFound
		}
		return nil, err
	}
	return a, nil
}
