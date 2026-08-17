package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// thumbnailSpecs declares the variants the upload pipeline derives.
// Keep in sync with the AttachmentVariant constants in
// internal/models/attachment.go and the editor's expectations
// (TASK-874 / TASK-876 request thumb-md by default).
var thumbnailSpecs = []struct {
	Variant string
	MaxLong int
}{
	{models.AttachmentVariantThumbSm, 256},
	{models.AttachmentVariantThumbMd, 1024},
}

// deriveThumbnails generates thumb-sm and thumb-md variants for the
// uploaded image identified by parentID. Idempotent and tolerant of
// every failure mode — a thumbnail-generation hiccup never affects
// the original blob. Designed to run inside goAsync so the response
// never waits for imaging work, but is also safe to call directly
// from tests that want deterministic post-conditions.
//
// Skip cases (logged but not failed):
//   - Original row missing or already deleted (race with delete).
//   - Parent ITEM soft-deleted, or item_id malformed so it resolves
//     to nothing in this workspace (PLAN-2391 DR-14 — see the note
//     at the check).
//   - Source format not supported by the configured processor (e.g.
//     pure-Go on a WebP upload — the original survives, the user
//     just sees the original at native resolution).
//   - Source dimensions known and already smaller than the variant's
//     target — pointless to encode an upscaled or same-size copy.
//   - Variant already exists (idempotent reruns from a future
//     "regenerate thumbnails" admin action).
//
// Each successfully-derived variant becomes its own attachments row
// with parent_id = parentID and variant = "thumb-sm" / "thumb-md".
// Variants count toward workspace storage usage (DOC-865 explicit).
func (s *Server) deriveThumbnails(parentID string) {
	if s.imageProcessor == nil || s.attachments == nil {
		return
	}
	ctx := context.Background()

	parent, err := s.store.GetAttachment(parentID)
	if err != nil {
		slog.Warn("thumbnails: get parent failed", "attachment_id", parentID, "error", err)
		return
	}
	if parent == nil || parent.DeletedAt != nil {
		// Original was deleted between upload completion and our
		// goroutine running. Nothing to do — the orphan blob (if any)
		// will be cleaned up by orphan GC.
		return
	}
	if !s.thumbnailParentItemLive(parent) {
		return
	}

	src, err := s.openOriginalForThumbnail(ctx, parent)
	if err != nil {
		slog.Warn("thumbnails: open source failed",
			"attachment_id", parent.ID, "storage_key", parent.StorageKey, "error", err)
		return
	}
	defer src.Close()

	img, format, err := s.imageProcessor.Decode(src)
	if err != nil {
		// Unsupported / oversized source. Log at debug and bail —
		// this is the documented graceful-degradation path; the
		// original is fine, only derivation skips.
		level := slog.LevelWarn
		if errors.Is(err, attachments.ErrUnsupportedFormat) || errors.Is(err, attachments.ErrImageTooLarge) {
			level = slog.LevelDebug
		}
		slog.Log(ctx, level, "thumbnails: decode skipped",
			"attachment_id", parent.ID, "format", format, "error", err)
		return
	}

	outFormat := attachments.ThumbnailFormat(format)

	for _, spec := range thumbnailSpecs {
		// Skip when the source is already within the variant's bounds
		// — the download handler's variant fallback path serves the
		// original, which is what we'd produce anyway.
		if parent.Width != nil && parent.Height != nil &&
			*parent.Width <= spec.MaxLong && *parent.Height <= spec.MaxLong {
			continue
		}
		if existing, err := s.store.GetAttachmentVariant(parent.WorkspaceID, parent.ID, spec.Variant); err == nil && existing != nil {
			continue
		}

		resized, err := s.imageProcessor.Resize(img, spec.MaxLong)
		if err != nil {
			slog.Warn("thumbnails: resize failed",
				"attachment_id", parent.ID, "variant", spec.Variant, "error", err)
			continue
		}

		if err := s.persistThumbnail(ctx, parent, resized, spec.Variant, outFormat); err != nil {
			slog.Warn("thumbnails: persist failed",
				"attachment_id", parent.ID, "variant", spec.Variant, "error", err)
			continue
		}
	}

	// Derived rows count toward workspace storage usage. Drop the
	// cached summary so the next storage/usage GET reflects the new
	// thumbnail bytes; the upload handler already invalidated when
	// inserting the original, but thumbnail rows land later.
	s.storageInfoCache.invalidate(parent.WorkspaceID)
}

// thumbnailParentItemLive reports whether it is worth deriving variants
// for parent — i.e. whether parent is an orphan (no item at all) or is
// bound to an item that is live and in parent's own workspace.
//
// Why derivation cares about the ITEM at all (PLAN-2391 DR-14). Each
// derived row copies parent.ItemID, so a variant of an archived item's
// attachment is quota-counted storage that nobody can ever read: DR-13
// makes the blob path 404 for a soft-deleted parent item, so the bytes
// are written, charged, and refused. The same holds for a malformed
// item_id — the column carries no FK and no same-workspace constraint,
// so a row can name an item in another workspace or no item at all
// (that invariant is fixed at the source by TASK-2400 and repaired for
// existing rows by PLAN-2397), and such a row is likewise unreadable.
// Deriving for either is pure waste, so skip both.
//
// THE POST-CHECK WINDOW IS DELIBERATELY ACCEPTED — this is not an
// oversight, please don't file it as a bug. The check is point-in-time:
// item deletion commits in its own transaction (store.DeleteItem), and
// everything between here and persistThumbnail's insert — the blob read,
// decode, resize, encode, Put — is unbounded work. So an item archived
// mid-flight can still get a variant row written against it.
//
// Transform (TASK-2402) closes its equivalent window with an item row
// lock inside the insert (store.CreateAttachmentForLiveItem). Derivation
// deliberately makes the OPPOSITE trade and does NOT use it: transform is
// user-initiated and low-volume, whereas derivation is a background worker
// that fans out from EVERY image upload, so an item lock on this path is
// disproportionate to the harm. And the harm is small: what leaks through
// the window is a thumbnail — a small derived file, unreadable under DR-13
// for exactly as long as its item stays archived, and tombstoned by the
// delete cascade along with its parent attachment. If profiling later shows
// the lock is cheap here, tightening this is a follow-up, not a defect.
// The invariant itself lives in resolveAttachmentParentItem, shared with the
// blob read, transform and delete paths (includeArchived=false, so "live"
// means exactly what the read path means by it). Only the REACTION is local:
// this is background work with no HTTP response, so there is no 404 shape to
// match — each malformed outcome gets its own WARN so it stays greppable ahead
// of PLAN-2397's repair, which is the opposite of what the HTTP paths need.
func (s *Server) thumbnailParentItemLive(parent *models.Attachment) bool {
	item, outcome, err := s.resolveAttachmentParentItem(parent, false)
	if err != nil {
		slog.Warn("thumbnails: get parent item failed",
			"attachment_id", parent.ID, "item_id", derefOrEmpty(parent.ItemID), "error", err)
		return false
	}
	switch outcome {
	case attachmentParentOrphan, attachmentParentOK:
		return true
	case attachmentParentForeign:
		slog.Warn("thumbnails: skipped, parent item belongs to another workspace",
			"attachment_id", parent.ID, "item_id", derefOrEmpty(parent.ItemID),
			"attachment_workspace_id", parent.WorkspaceID, "item_workspace_id", item.WorkspaceID)
		return false
	default: // attachmentParentGone
		slog.Warn("thumbnails: skipped, parent item is archived or unresolvable",
			"attachment_id", parent.ID, "item_id", derefOrEmpty(parent.ItemID))
		return false
	}
}

// derefOrEmpty renders a nullable id for a log field.
func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// openOriginalForThumbnail resolves the parent row's storage key
// against the registry and returns a stream positioned at the start
// of the blob. The caller MUST close it.
func (s *Server) openOriginalForThumbnail(ctx context.Context, parent *models.Attachment) (io.ReadCloser, error) {
	store, err := s.attachments.Resolve(parent.StorageKey)
	if err != nil {
		return nil, fmt.Errorf("resolve storage backend: %w", err)
	}
	body, err := store.Get(ctx, parent.StorageKey)
	if err != nil {
		return nil, fmt.Errorf("read original blob: %w", err)
	}
	return body, nil
}

// persistThumbnail encodes the resized image, hashes the bytes,
// writes through the content-addressed storage backend, and inserts
// the derived attachments row. store.Put hash-verifies the streamed
// bytes against the supplied hash, so a corrupt-bytes case fails
// loudly before the DB row is created.
func (s *Server) persistThumbnail(
	ctx context.Context,
	parent *models.Attachment,
	resized image.Image,
	variant string,
	format string,
) error {
	var buf bytes.Buffer
	if err := s.imageProcessor.Encode(resized, format, &buf); err != nil {
		return fmt.Errorf("encode %s: %w", format, err)
	}
	hash := sha256Hex(buf.Bytes())

	store, err := s.attachments.Resolve(attachments.FSPrefix + ":" + hash)
	if err != nil {
		return fmt.Errorf("resolve storage backend: %w", err)
	}
	// Fence the Put + CreateAttachment pair against orphan-GC blob
	// deletion. See handlers_attachments.go for the upload-handler
	// rationale; thumbnails hit the same race when an old soft-
	// deleted thumbnail shares the same hash as a freshly-derived one.
	releaseInFlight := s.markUploadInFlight(hash)
	defer releaseInFlight()
	storageKey, err := store.Put(ctx, hash, attachments.ThumbnailMime(format), bytes.NewReader(buf.Bytes()))
	if err != nil {
		return fmt.Errorf("put thumbnail blob: %w", err)
	}

	bounds := resized.Bounds()
	w := bounds.Dx()
	h := bounds.Dy()

	parentRef := parent.ID
	variantRef := variant
	row := &models.Attachment{
		WorkspaceID: parent.WorkspaceID,
		ItemID:      parent.ItemID,
		UploadedBy:  parent.UploadedBy,
		StorageKey:  storageKey,
		ContentHash: hash,
		MimeType:    attachments.ThumbnailMime(format),
		SizeBytes:   int64(buf.Len()),
		Filename:    thumbnailFilename(parent.Filename, variant, format),
		Width:       &w,
		Height:      &h,
		ParentID:    &parentRef,
		Variant:     &variantRef,
	}
	inserted, err := s.store.CreateAttachmentVariantIfParentLive(row)
	if err != nil {
		return fmt.Errorf("create thumbnail row: %w", err)
	}
	if !inserted {
		// The parent was deleted between the derivation's early liveness
		// check and this insert (BUG-2388) — the conditional insert is
		// the fix for the race that used to mint a live variant row
		// under a tombstoned parent. Clean up the just-Put blob, unless
		// another live/in-grace row shares the hash (same dedupe
		// protection as the GC sweep, using the CONFIGURED grace — a
		// longer operator grace must keep a still-restorable peer's
		// bytes). The count runs first; the in-flight re-check + Delete
		// then hold inFlightHashesMu, mirroring the sweep's fence, so a
		// concurrent upload registering this hash between our check and
		// the Delete cannot lose its bytes (codex round 1 P1). We hold
		// one registration ourselves — > 1 means someone else does too.
		// A cleanup failure or skipped count only strands bytes — never
		// a row — and is logged so it isn't silent (codex round 1 P2).
		// Count INSIDE the fence (codex round 2): a full upload
		// lifecycle — register, Put, insert row, release — can complete
		// between an outside-the-mutex count and the delete, so the
		// count must observe the world the delete will act on. The
		// sweep holds this mutex across a backend resolve + FS delete
		// already; one bounded DB count under it is the same class.
		graceCutoff := time.Now().Add(-s.orphanGCGraceConfigured())
		s.inFlightHashesMu.Lock()
		if s.inFlightHashes[hash] <= 1 {
			others, cErr := s.store.CountProtectingAttachmentsForHash(hash, "", graceCutoff)
			if cErr != nil {
				slog.Warn("thumbnails: refusal cleanup count failed — blob stranded for manual cleanup",
					"storage_key", storageKey, "error", cErr)
			} else if others == 0 {
				if dErr := store.Delete(ctx, storageKey); dErr != nil {
					slog.Warn("thumbnails: orphan blob cleanup failed",
						"storage_key", storageKey, "error", dErr)
				}
			}
		}
		s.inFlightHashesMu.Unlock()
		slog.Info("thumbnails: skipped, parent deleted during derivation",
			"parent_id", parent.ID, "variant", variant)
		return nil
	}
	return nil
}

// sha256Hex returns the hex-encoded sha256 of b. The FS storage
// backend keys blobs by this exact form, so the value also serves as
// the storage_key suffix.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// thumbnailFilename builds the synthetic filename for a derived row
// — parent's basename + variant suffix + extension. Surfaces a
// sensible Content-Disposition when a user downloads a thumbnail
// directly (e.g. via the storage usage admin UI in TASK-882).
func thumbnailFilename(parent, variant, format string) string {
	ext := attachments.ThumbnailExt(format)
	base := strings.TrimSuffix(parent, filepath.Ext(parent))
	if base == "" {
		base = "attachment"
	}
	return fmt.Sprintf("%s.%s%s", base, variant, ext)
}
