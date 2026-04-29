package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// transformRequest is the body shape for POST /transform. Operation
// is the discriminator; the per-operation params live in their own
// fields so callers don't have to wrap them in a generic args map.
//
// Adding a new operation = adding a new case in the dispatch below
// + a new param field here. Keeps the wire format tight and the
// handler boring.
type transformRequest struct {
	Operation string `json:"operation"`

	// rotate
	Degrees int `json:"degrees,omitempty"`

	// crop (TASK-880 will populate this; defined here so the wire
	// format is stable across the two PRs).
	Rect *transformRect `json:"rect,omitempty"`
}

// transformRect is the crop rectangle in original-image pixel space.
// Origin top-left, x/y/w/h are integers — preview-pixel-to-original
// conversion is the editor's responsibility, so server-side math
// stays in canonical coordinates.
type transformRect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// transformResponse is the success payload — same shape as the
// upload response so the editor can swap the node's UUID and any
// width/height attrs in one go without a follow-up GET.
type transformResponse struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Mime     string `json:"mime"`
	Size     int64  `json:"size"`
	Width    *int   `json:"width,omitempty"`
	Height   *int   `json:"height,omitempty"`
	Filename string `json:"filename"`
}

// handleTransformAttachment implements POST
// /api/v1/workspaces/{slug}/attachments/{attachmentID}/transform.
//
// Phase 1 supports two operations: "rotate" (TASK-879) and "crop"
// (TASK-880). Both follow the same flow:
//
//  1. Auth: editor+ on the workspace.
//  2. Load the parent attachment row (cross-workspace = 404).
//  3. Reject if no image processor is wired (libvips build that
//     hasn't shipped Phase 2 yet, or a self-host that opted out).
//  4. Decode the original — defends against unsupported MIMEs and
//     oversized inputs via the processor's Decode rules.
//  5. Apply the operation. Each op validates its own params.
//  6. Encode in the policy format (PNG → PNG to preserve alpha,
//     everything else → JPEG q=85). Same policy as thumbnails so
//     transformed and thumbnail blobs deduplicate cleanly.
//  7. Hash + Put through the storage backend (content-addressed,
//     so identical transforms collapse onto the same blob).
//  8. Insert a NEW attachments row owned by the same workspace /
//     uploaded_by / item as the parent. The original is left in
//     place — orphan GC reclaims it past the grace period
//     (TASK-886) once the editor swaps its reference.
//  9. Return the new row's ID/URL/dimensions so the editor can
//     update its node attrs without a follow-up GET.
func (s *Server) handleTransformAttachment(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
		return
	}
	if s.attachments == nil {
		writeError(w, http.StatusServiceUnavailable, "attachments_disabled",
			"Attachment storage is not configured on this server")
		return
	}
	if s.imageProcessor == nil {
		writeError(w, http.StatusServiceUnavailable, "image_processor_disabled",
			"Image transformation is not available on this build (the libvips backend has not shipped yet)")
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "attachmentID")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Missing attachment id")
		return
	}

	parent, err := s.store.GetAttachment(id)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// 404 — same cross-workspace defense as the GET handler. Leaking
	// 403 vs 404 here would let a member of workspace B enumerate
	// attachment IDs in workspace A.
	if parent == nil || parent.WorkspaceID != workspaceID || parent.DeletedAt != nil {
		writeError(w, http.StatusNotFound, "not_found", "Attachment not found")
		return
	}

	var req transformRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body")
		return
	}

	// Open the original blob through the registry. Defer Close so
	// every error path below releases the file handle / network
	// connection.
	srcStore, err := s.attachments.Resolve(parent.StorageKey)
	if err != nil {
		writeInternalError(w, fmt.Errorf("resolve source backend: %w", err))
		return
	}
	body, err := srcStore.Get(r.Context(), parent.StorageKey)
	if err != nil {
		writeInternalError(w, fmt.Errorf("read source blob: %w", err))
		return
	}
	defer body.Close()

	srcImg, srcFormat, err := s.imageProcessor.Decode(body)
	if err != nil {
		switch {
		case errors.Is(err, attachments.ErrUnsupportedFormat):
			// Per spec: editor surfaces this as an inline tooltip.
			// 415 is the canonical "media type not supported" code.
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_format",
				"Image transformation isn't available for "+parent.MimeType+" on this build")
		case errors.Is(err, attachments.ErrImageTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "image_too_large",
				"Image dimensions exceed the processor's safe-decode limit")
		default:
			writeInternalError(w, fmt.Errorf("decode source: %w", err))
		}
		return
	}

	transformed, opErr := applyImageTransform(s.imageProcessor, srcImg, &req)
	if opErr != nil {
		writeError(w, http.StatusBadRequest, "bad_transform", opErr.Error())
		return
	}

	outFormat := attachments.ThumbnailFormat(srcFormat)
	var buf bytes.Buffer
	if err := s.imageProcessor.Encode(transformed, outFormat, &buf); err != nil {
		writeInternalError(w, fmt.Errorf("encode transformed image: %w", err))
		return
	}
	hash := sha256Hex(buf.Bytes())

	dstStore, err := s.attachments.Resolve(attachments.FSPrefix + ":" + hash)
	if err != nil {
		writeInternalError(w, fmt.Errorf("resolve destination backend: %w", err))
		return
	}
	// Same orphan-GC fence as the upload handler — see the comment
	// at handlers_attachments.go's store.Put callsite. Released
	// after CreateAttachment below regardless of outcome.
	releaseInFlight := s.markUploadInFlight(hash)
	defer releaseInFlight()
	storageKey, err := dstStore.Put(r.Context(), hash, attachments.ThumbnailMime(outFormat), bytes.NewReader(buf.Bytes()))
	if err != nil {
		writeInternalError(w, fmt.Errorf("put transformed blob: %w", err))
		return
	}

	bounds := transformed.Bounds()
	tw := bounds.Dx()
	th := bounds.Dy()

	// Inherit attribution + item linkage from the parent. The new
	// row is a peer (NOT a derived/variant row — that's only for
	// thumbnails) so ParentID and Variant stay nil; the editor
	// swaps its reference and the original ages into orphan GC.
	//
	// UploadedBy inherits from the parent — same policy as the
	// thumbnail pipeline. A user who rotates someone else's upload
	// shouldn't inadvertently take ownership of the resulting blob;
	// audit attribution stays anchored to whoever first put the
	// bytes into the workspace. (The transform itself is auditable
	// at the request layer if/when we add a transform-events log.)
	row := &models.Attachment{
		WorkspaceID: parent.WorkspaceID,
		ItemID:      parent.ItemID,
		UploadedBy:  parent.UploadedBy,
		StorageKey:  storageKey,
		ContentHash: hash,
		MimeType:    attachments.ThumbnailMime(outFormat),
		SizeBytes:   int64(buf.Len()),
		Filename:    transformedFilename(parent.Filename, req.Operation, outFormat),
		Width:       &tw,
		Height:      &th,
	}
	if err := s.store.CreateAttachment(row); err != nil {
		writeInternalError(w, fmt.Errorf("create transformed row: %w", err))
		return
	}

	// Drop the storage-usage cache so the Settings → Storage UI sees
	// the new attachment bytes on the next read. Same rationale as
	// the upload path.
	s.storageInfoCache.invalidate(workspaceID)

	// Quota tracking — observational only in Phase 1, same as upload.
	s.goAsync(func() { s.maybeWarnStorageQuota(workspaceID) })

	writeJSON(w, http.StatusCreated, transformResponse{
		ID:       row.ID,
		URL:      attachmentURL(chi.URLParam(r, "slug"), row.ID),
		Mime:     row.MimeType,
		Size:     row.SizeBytes,
		Width:    row.Width,
		Height:   row.Height,
		Filename: row.Filename,
	})
}

// applyImageTransform dispatches on the operation discriminator.
// Each branch validates its own params and returns a clear error
// — the handler turns those into 400 Bad Request without leaking
// internal detail. New ops slot in here.
func applyImageTransform(p attachments.Processor, src image.Image, req *transformRequest) (image.Image, error) {
	switch req.Operation {
	case "rotate":
		// Validate degrees up-front so we can return a clean 400
		// instead of leaking the processor's "only multiples of 90"
		// internal-error message. The set is intentionally narrow:
		// 90 / 180 / 270 are pixel-exact reorders (no resampling),
		// and 0 is a no-op the editor shouldn't be sending.
		switch req.Degrees {
		case 90, 180, 270:
			return p.Rotate(src, req.Degrees)
		default:
			return nil, fmt.Errorf("rotate: degrees must be 90, 180, or 270 (got %d)", req.Degrees)
		}
	case "crop":
		if req.Rect == nil {
			return nil, fmt.Errorf("crop: rect is required")
		}
		if req.Rect.W <= 0 || req.Rect.H <= 0 {
			return nil, fmt.Errorf("crop: rect width/height must be positive")
		}
		if req.Rect.X < 0 || req.Rect.Y < 0 {
			return nil, fmt.Errorf("crop: rect x/y must be non-negative")
		}
		// Processor.Crop intersects with the image bounds itself, so
		// we don't need to clip here. The processor returns an error
		// only for empty intersections (rect entirely outside the
		// image) — bubble that up as a 400.
		return p.Crop(src, image.Rect(req.Rect.X, req.Rect.Y, req.Rect.X+req.Rect.W, req.Rect.Y+req.Rect.H))
	default:
		return nil, fmt.Errorf("operation must be one of: rotate, crop (got %q)", req.Operation)
	}
}

// transformedFilename builds the synthetic filename for the derived
// row. We append the operation tag (e.g. ".rotated", ".cropped")
// before the extension so a user downloading the file directly sees
// what happened to it. Matches the shape thumbnailFilename emits.
func transformedFilename(parent, op, format string) string {
	ext := attachments.ThumbnailExt(format)
	base := parent
	// Strip any existing extension so we don't end up with
	// "shot.png.rotated.png" round-trip cruft.
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '.' {
			base = base[:i]
			break
		}
	}
	if base == "" {
		base = "attachment"
	}
	return base + "." + op + ext
}
