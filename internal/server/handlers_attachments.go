package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"

	// Stdlib decoders for width/height probing on the formats Phase 1
	// has to inspect server-side. WebP/AVIF/HEIC are not in the stdlib;
	// we accept those uploads but skip the dimension probe (matches the
	// "pure-Go gracefully degrades on WebP/AVIF/HEIC" decision in DOC-865).
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// defaultAttachmentMaxBytes caps a single upload to 25 MiB by default.
// Operators can raise this with SetAttachments(reg, customLimit). The
// cap is enforced via http.MaxBytesReader before any streaming writes
// touch the disk so a multi-GB POST never makes it into the temp file.
const defaultAttachmentMaxBytes = 25 << 20 // 25 MiB

// multipartParseMemory is the in-memory threshold used when calling
// (*http.Request).ParseMultipartForm. We only POST a single "file"
// field — anything past this is spilled to disk by net/http, which is
// the desired behavior for large uploads.
const multipartParseMemory = 1 << 20 // 1 MiB

// handleUploadAttachment accepts a multipart upload and writes it into
// the attachments table + the configured AttachmentStore.
//
// Flow:
//  1. Auth: editor+ role on the workspace (already gated by the route's
//     RequireWorkspaceAccess middleware; we additionally require editor).
//  2. Cap the body via MaxBytesReader to attachmentMaxBytes(s).
//  3. Stream the multipart "file" part into a temp file, sha256ing as
//     we go so we never hold the whole payload in RAM.
//  4. Sniff MIME on the first 512 bytes of the temp file and
//     cross-check against the filename extension.
//  5. Optionally probe width/height for stdlib-decodable image formats.
//  6. Call AttachmentStore.Put — Put hash-verifies and writes via
//     atomic temp+rename, content-addressed by the sha256 we computed.
//  7. Insert the attachments row.
//  8. Track (do not enforce) quota: WorkspaceStorageUsage vs the
//     workspace owner's storage_bytes limit. Phase 2 will enforce.
//  9. Return JSON {id, url, mime, size, width?, height?, filename, category, render_mode}.
func (s *Server) handleUploadAttachment(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
		return
	}
	if s.attachments == nil {
		writeError(w, http.StatusServiceUnavailable, "attachments_disabled",
			"Attachment storage is not configured on this server")
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	// Attribution: prefer the logged-in user. On a fresh install (no
	// users yet) RequireWorkspaceAccess grants implicit owner access
	// without setting a user — record those uploads as "system" so the
	// uploaded_by NOT NULL column has a stable value.
	uploadedBy := currentUserID(r)
	if uploadedBy == "" {
		uploadedBy = "system"
	}

	maxBytes := s.attachmentMaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultAttachmentMaxBytes
	}

	// Cap the request body BEFORE ParseMultipartForm spools any of it.
	// MaxBytesReader trips a typed "*http.MaxBytesError" once the limit
	// is exceeded, which we surface as 413 below.
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+(1<<16)) // +64KiB headroom for multipart envelope

	if err := r.ParseMultipartForm(multipartParseMemory); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "file_too_large",
				fmt.Sprintf("File exceeds %d MiB upload limit", maxBytes>>20))
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid multipart body: "+err.Error())
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing_file", `Missing "file" form field`)
		return
	}
	defer file.Close()

	// Optional ?item_id=<slug-or-id> associates the attachment with an
	// item at upload time. The editor will normally upload first,
	// receive the UUID, insert the markdown reference, and then PATCH
	// the item — at which point the attachment can be associated via a
	// follow-up call. For now the parameter is optional and stored
	// verbatim if supplied.
	itemID := strings.TrimSpace(r.URL.Query().Get("item_id"))
	if itemID == "" {
		itemID = strings.TrimSpace(r.FormValue("item_id"))
	}
	var itemIDPtr *string
	if itemID != "" {
		itemIDPtr = &itemID
	}

	// Sanitize the filename: strip path components so a client can't
	// sneak directory traversal through the display name. We don't
	// store this in the storage backend — only in the DB row for UI.
	filename := filepath.Base(header.Filename)
	if filename == "" || filename == "." || filename == "/" {
		filename = "upload.bin"
	}

	// Stream into a temp file under the OS temp dir. We copy in 32KiB
	// chunks via io.Copy and tee through a sha256 hasher.
	tmp, err := os.CreateTemp("", "pad-upload-*.bin")
	if err != nil {
		writeInternalError(w, fmt.Errorf("create upload temp: %w", err))
		return
	}
	tmpPath := tmp.Name()
	defer func() {
		// CreateTemp gives 0600. Always remove on the way out — we
		// never need the temp file after this handler returns.
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hasher), file)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "file_too_large",
				fmt.Sprintf("File exceeds %d MiB upload limit", maxBytes>>20))
			return
		}
		writeInternalError(w, fmt.Errorf("stream upload to temp: %w", err))
		return
	}
	if written > maxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "file_too_large",
			fmt.Sprintf("File exceeds %d MiB upload limit", maxBytes>>20))
		return
	}
	if written == 0 {
		writeError(w, http.StatusBadRequest, "empty_file", "Uploaded file is empty")
		return
	}
	if err := tmp.Sync(); err != nil {
		writeInternalError(w, fmt.Errorf("sync upload temp: %w", err))
		return
	}

	hash := hex.EncodeToString(hasher.Sum(nil))

	// Sniff MIME against the allowlist on the first 512 bytes.
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		writeInternalError(w, fmt.Errorf("rewind upload temp: %w", err))
		return
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(tmp, head)
	head = head[:n]

	entry, code, vErr := attachments.ValidateUpload(head, filename)
	if vErr != nil {
		writeError(w, http.StatusUnsupportedMediaType, code, vErr.Error())
		return
	}

	// Probe image dimensions for stdlib-decodable formats. WebP/AVIF/HEIC
	// fall through here with width/height nil (the editor still renders
	// them at their natural size — the browser handles those).
	var width, height *int
	if entry.Category == attachments.CategoryImage {
		if _, err := tmp.Seek(0, io.SeekStart); err == nil {
			if cfg, _, decodeErr := image.DecodeConfig(tmp); decodeErr == nil {
				w := cfg.Width
				h := cfg.Height
				width = &w
				height = &h
			}
		}
	}

	// Hand the temp file to the storage backend. Put will hash-verify
	// and write atomically; we already know the hash matches the
	// streamed bytes, but the FSStore re-hashes defensively.
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		writeInternalError(w, fmt.Errorf("rewind upload temp for store.Put: %w", err))
		return
	}
	store, err := s.attachments.Resolve(attachments.FSPrefix + ":" + hash)
	if err != nil {
		writeInternalError(w, fmt.Errorf("resolve attachment store: %w", err))
		return
	}
	storageKey, err := store.Put(r.Context(), hash, entry.MIME, tmp)
	if err != nil {
		writeInternalError(w, fmt.Errorf("attachment store.Put: %w", err))
		return
	}

	// Insert the DB row referencing the freshly-stored blob.
	att := &models.Attachment{
		WorkspaceID: workspaceID,
		ItemID:      itemIDPtr,
		UploadedBy:  uploadedBy,
		StorageKey:  storageKey,
		ContentHash: hash,
		MimeType:    entry.MIME,
		SizeBytes:   written,
		Filename:    filename,
		Width:       width,
		Height:      height,
	}
	if err := s.store.CreateAttachment(att); err != nil {
		// Note: the blob is now an orphan on disk. Orphan GC (TASK-886)
		// will reclaim it past the grace period; we do NOT delete it
		// here because the same hash may still be used by a concurrent
		// upload that succeeded its DB insert.
		writeInternalError(w, fmt.Errorf("create attachment row: %w", err))
		return
	}

	// Quota tracking — log only, no enforcement in Phase 1. Hard limits
	// + Stripe metered overage land in Phase 2 (TASK-881 builds the
	// usage API + effective-limit math; we lazily reuse CheckLimit here
	// for the warning since it already does the three-tier resolution).
	go s.maybeWarnStorageQuota(workspaceID)

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          att.ID,
		"url":         attachmentURL(workspaceID, att.ID),
		"mime":        att.MimeType,
		"size":        att.SizeBytes,
		"width":       att.Width,
		"height":      att.Height,
		"filename":    att.Filename,
		"category":    string(entry.Category),
		"render_mode": renderModeString(entry.RenderMode),
	})
}

// attachmentURL returns the canonical download URL for an attachment.
// The serve handler is wired in TASK-872; the path shape is committed
// here so the upload response can return a usable URL today.
func attachmentURL(workspaceID, attachmentID string) string {
	return "/api/v1/workspaces/" + workspaceID + "/attachments/" + attachmentID
}

func renderModeString(m attachments.RenderMode) string {
	switch m {
	case attachments.RenderInline:
		return "inline"
	case attachments.RenderChip:
		return "chip"
	case attachments.RenderForceDownload:
		return "download"
	default:
		return "chip"
	}
}

// maybeWarnStorageQuota emits a slog warning if the workspace's usage
// has crossed the owner-plan storage limit. Phase 1 is observational
// only — see DOC-865 "Phase 1 tracks usage and exposes it ... but does
// not block uploads". The check is fire-and-forget so it never blocks
// the upload response.
func (s *Server) maybeWarnStorageQuota(workspaceID string) {
	usage, err := s.store.WorkspaceStorageUsage(workspaceID)
	if err != nil {
		slog.Warn("attachments: storage usage probe failed", "workspace_id", workspaceID, "error", err)
		return
	}
	limit, err := s.store.CheckLimit(workspaceID, "storage_bytes")
	if err != nil || limit == nil || limit.Limit < 0 {
		return
	}
	if usage > int64(limit.Limit) {
		slog.Warn("attachments: workspace exceeds storage quota (Phase 1 = no enforcement)",
			"workspace_id", workspaceID,
			"plan", limit.Plan,
			"used_bytes", usage,
			"limit_bytes", limit.Limit)
	}
}
