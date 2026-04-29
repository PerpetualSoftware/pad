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
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

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

	// Quota tracking — log only, no enforcement in Phase 1. The download
	// URL points at the GET handler shipped in TASK-872 (same PR series).
	// Workspaces are addressed by slug everywhere else in the API; we
	// surface the {slug} form here so the UI can link directly.
	s.goAsync(func() { s.maybeWarnStorageQuota(workspaceID) })

	// Thumbnail derivation (TASK-878). For images on the supported
	// allowlist we generate two variants — thumb-sm (256px long edge)
	// and thumb-md (1024px long edge) — each as its own attachments
	// row with parent_id pointing at the original. Runs async via
	// goAsync so the upload response doesn't wait on imaging work,
	// and Server.Stop() waits for in-flight thumbnailing before the
	// process exits / SQLite is closed. The download handler's
	// variant fallback already serves the original when no derived
	// row exists, so the upload response is correct the moment it
	// returns regardless of when the goroutine completes.
	if entry.Category == attachments.CategoryImage && s.imageProcessor != nil {
		original := att.ID
		s.goAsync(func() { s.deriveThumbnails(original) })
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          att.ID,
		"url":         attachmentURL(chi.URLParam(r, "slug"), att.ID),
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
// Uses the workspace slug from the request — the same shape every other
// API endpoint uses, so clients can build it without an extra lookup.
func attachmentURL(workspaceSlug, attachmentID string) string {
	return "/api/v1/workspaces/" + workspaceSlug + "/attachments/" + attachmentID
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

// handleGetAttachment streams an attachment back to the caller. Auth is
// enforced by the route's RequireWorkspaceAccess middleware; we
// additionally require viewer+ here.
//
// The handler:
//   - Looks up the row by ID (TASK-871's UUID), refusing with 404 if it
//     belongs to a different workspace — leaking 403 vs 404 on a
//     cross-workspace probe would let a member of workspace A enumerate
//     attachment IDs in workspace B.
//   - Optionally resolves a derived variant (?variant=thumb-sm|thumb-md).
//     If the variant row is missing (TASK-878 hasn't generated thumbnails
//     for this attachment yet), falls back to the original — clients can
//     always ask for a thumb and get something renderable.
//   - Resolves the storage backend via the Registry and opens the blob.
//   - Sets Content-Type from the DB row (which is already the canonical
//     post-allowlist MIME, not the client-supplied one).
//   - Sets Content-Disposition from the MIME's RenderMode entry:
//     RenderForceDownload → "attachment", everything else → "inline".
//   - Hands off to http.ServeContent when the backend supports Seek
//     (FSStore returns *os.File, so this is the common path) — that
//     gives us If-Modified-Since, If-None-Match, Range/206, Accept-Ranges
//     all for free. Backends that only support Read fall back to a
//     plain stream copy with no Range support.
//   - Sets a short Cache-Control: private, max-age=3600. Phase 3 will
//     revisit for CDN caching.
func (s *Server) handleGetAttachment(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "viewer") {
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

	id := chi.URLParam(r, "attachmentID")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Missing attachment id")
		return
	}

	att, err := s.store.GetAttachment(id)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// Cross-workspace defense: 404 (not 403) so an attacker can't
	// distinguish "exists in another workspace" from "doesn't exist".
	if att == nil || att.WorkspaceID != workspaceID || att.DeletedAt != nil {
		writeError(w, http.StatusNotFound, "not_found", "Attachment not found")
		return
	}

	// Optional variant lookup. If the requested variant doesn't exist
	// yet (TASK-878 generates thumbnails — until that ships, every
	// variant fetch falls back to the original) we silently serve the
	// original. The editor uses thumbnails as an optimization, not a
	// correctness requirement, so falling back keeps every render path
	// working as soon as TASK-872 ships.
	if variant := r.URL.Query().Get("variant"); variant != "" {
		if !isKnownVariant(variant) {
			writeError(w, http.StatusBadRequest, "bad_variant",
				"Unknown variant — supported: thumb-sm, thumb-md")
			return
		}
		if derived, dErr := s.store.GetAttachmentVariant(att.ID, variant); dErr != nil {
			writeInternalError(w, dErr)
			return
		} else if derived != nil {
			att = derived
		}
	}

	store, err := s.attachments.Resolve(att.StorageKey)
	if err != nil {
		writeInternalError(w, fmt.Errorf("resolve attachment store for %s: %w", att.StorageKey, err))
		return
	}
	body, err := store.Get(r.Context(), att.StorageKey)
	if err != nil {
		if errors.Is(err, attachments.ErrNotFound) {
			// DB row exists but the on-disk blob is missing. This is a
			// "shouldn't happen" state — log it and return 404 so the
			// client can surface a useful error to the user.
			slog.Warn("attachments: blob missing for live row",
				"attachment_id", att.ID, "storage_key", att.StorageKey)
			writeError(w, http.StatusNotFound, "blob_missing",
				"Attachment metadata exists but the file is missing on disk")
			return
		}
		writeInternalError(w, fmt.Errorf("get attachment blob: %w", err))
		return
	}
	defer body.Close()

	// Headers come BEFORE ServeContent / io.Copy so they make it onto
	// the wire even when the response is a 304 / 206.
	w.Header().Set("Content-Type", att.MimeType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	disposition := "inline"
	if entry, ok := attachments.LookupMIME(att.MimeType); ok && entry.RenderMode == attachments.RenderForceDownload {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`%s; filename=%q`, disposition, sanitizeHeaderFilename(att.Filename)))

	// http.ServeContent gives Range, conditional GETs, and 206
	// responses for free — but it requires an io.ReadSeeker. FSStore
	// returns an *os.File, which satisfies that. Backends that don't
	// support Seek (a future S3 streaming reader, say) fall back to a
	// plain Copy with no Range / 304 support.
	if seeker, ok := body.(io.ReadSeeker); ok {
		modtime := att.CreatedAt
		if modtime.IsZero() {
			modtime = time.Now()
		}
		// http.ServeContent already handles HEAD correctly on this
		// path — it sets headers and stops without writing the body.
		http.ServeContent(w, r, att.Filename, modtime, seeker)
		return
	}

	// Streaming fallback. Set Content-Length when the backend exposes
	// it (Stat is cheap on the FS but may be a network call on S3 —
	// we only call it here, never on the ServeContent fast path).
	if size, sErr := store.Stat(r.Context(), att.StorageKey); sErr == nil && size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	// HEAD on the streaming fallback: skip the body. Go's response
	// writer would silently discard body writes for HEAD anyway, but
	// reading the entire blob from a non-seekable backend just to
	// throw it away would burn S3 GetObject bandwidth. Bail out before
	// io.Copy ever touches the reader.
	if r.Method == http.MethodHead {
		return
	}
	if _, copyErr := io.Copy(w, body); copyErr != nil {
		// Body has likely already been written to; can't change status.
		// Just log so we have a trail.
		slog.Warn("attachments: streaming copy failed",
			"attachment_id", att.ID, "error", copyErr)
	}
}

// isKnownVariant gates the ?variant= query against a closed set so an
// attacker can't probe arbitrary variant strings. Mirrors the constants
// in models.Attachment.
func isKnownVariant(v string) bool {
	switch v {
	case models.AttachmentVariantThumbSm, models.AttachmentVariantThumbMd, models.AttachmentVariantOriginal:
		return true
	}
	return false
}

// sanitizeHeaderFilename strips characters that can't safely appear in
// a Content-Disposition filename header — quotes, CR, LF, and any
// control byte. The Filename column was already basenamed at upload
// time so path separators are not a concern; we only need to keep the
// header value parseable.
func sanitizeHeaderFilename(name string) string {
	if name == "" {
		return "attachment"
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r == '"' || r == '\\':
			// Drop quotes and backslashes — they break the quoted-string syntax.
		case r < 0x20 || r == 0x7f:
			// Drop control bytes — protect against header injection.
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		return "attachment"
	}
	return out
}

// maybeWarnStorageQuota emits a slog warning if the workspace's usage
// has crossed the owner-plan storage limit. Phase 1 is observational
// only — see DOC-865 "Phase 1 tracks usage and exposes it ... but does
// not block uploads". Runs via goAsync so it never blocks the upload
// response and is drained by Server.Stop() at shutdown.
//
// CheckLimit is intentionally NOT used here because its featureCount
// path doesn't know about byte-counted features (storage_bytes is
// computed via WorkspaceStorageUsage, not COUNT(*)). WorkspaceStorageLimit
// does the same three-tier resolution but returns the limit only.
func (s *Server) maybeWarnStorageQuota(workspaceID string) {
	usage, err := s.store.WorkspaceStorageUsage(workspaceID)
	if err != nil {
		slog.Warn("attachments: storage usage probe failed", "workspace_id", workspaceID, "error", err)
		return
	}
	limit, err := s.store.WorkspaceStorageLimit(workspaceID)
	if err != nil {
		slog.Warn("attachments: storage limit probe failed", "workspace_id", workspaceID, "error", err)
		return
	}
	if limit < 0 {
		return // unlimited plan
	}
	if usage > limit {
		slog.Warn("attachments: workspace exceeds storage quota (Phase 1 = no enforcement)",
			"workspace_id", workspaceID,
			"used_bytes", usage,
			"limit_bytes", limit)
	}
}
