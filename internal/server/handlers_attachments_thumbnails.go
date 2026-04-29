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
		if existing, err := s.store.GetAttachmentVariant(parent.ID, spec.Variant); err == nil && existing != nil {
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
	if err := s.store.CreateAttachment(row); err != nil {
		return fmt.Errorf("create thumbnail row: %w", err)
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
