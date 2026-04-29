package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// exportBundleVersion pins the on-disk bundle layout. Bumped if the
// internal structure changes in a way the import path can't handle
// transparently. Independent of WorkspaceExport.Version so we can
// evolve the JSON schema and the bundle format on different cadences.
const exportBundleVersion = 1

// handleExportWorkspaceBundle streams a tar.gz containing the
// workspace JSON export plus every original (non-thumbnail)
// attachment blob and a manifest. Bundle layout:
//
//	pad-export.json
//	attachments/manifest.json
//	attachments/<uuid>.<ext>
//
// Tar entries are written in a deterministic order so byte-identical
// workspaces produce byte-identical bundles (modulo `exported_at`):
//
//  1. pad-export.json
//  2. attachments/manifest.json
//  3. attachments/<uuid>.<ext>  — sorted by (created_at, id) via the
//     store's ORDER BY in WorkspaceAttachmentsForExport.
//
// We stream chunks straight to the response writer rather than
// buffering — a workspace with multi-GB of attachments would otherwise
// pin that much memory for the duration of the download.
//
// On error mid-stream the connection is dropped (the client sees a
// truncated tar that gunzip will fail to decompress); that's the
// least-bad option since headers + early bytes are already on the
// wire. The error is logged with attachment_id context so operators
// can diagnose without re-running the export.
//
// Auth: owner. The plain JSON path is owner-only too; the bundle
// has the same access scope plus the user-uploaded attachment
// blobs, so don't loosen.
func (s *Server) handleExportWorkspaceBundle(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	ws, ok := s.getWorkspace(w, r)
	if !ok {
		return
	}

	export, err := s.store.ExportWorkspace(ws.Slug)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}

	// Build the manifest before writing anything to the response so a
	// store error doesn't strand half a tar header. Streaming the
	// blobs themselves still happens after we commit to the response.
	attachments, err := s.store.WorkspaceAttachmentsForExport(ws.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// If attachments exist, the registry must be wired — otherwise
	// the blobs aren't reachable and the bundle would be a lie.
	if len(attachments) > 0 && s.attachments == nil {
		writeError(w, http.StatusServiceUnavailable, "attachments_disabled",
			"Attachment storage is not configured on this server")
		return
	}

	manifest := models.AttachmentManifest{
		Version: exportBundleVersion,
		Entries: make([]models.AttachmentManifestEntry, 0, len(attachments)),
	}
	for _, a := range attachments {
		entry := models.AttachmentManifestEntry{
			ID:          a.ID,
			Filename:    a.Filename,
			MIME:        a.MimeType,
			SizeBytes:   a.SizeBytes,
			ContentHash: a.ContentHash,
			Width:       a.Width,
			Height:      a.Height,
			UploadedBy:  a.UploadedBy,
			CreatedAt:   a.CreatedAt.UTC().Format(time.RFC3339),
		}
		if a.ItemID != nil {
			entry.ItemID = *a.ItemID
		}
		// ParentID + Variant stay empty for shipped entries — derived
		// rows are filtered out in WorkspaceAttachmentsForExport.
		manifest.Entries = append(manifest.Entries, entry)
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s-export.tar.gz"`, ws.Slug))
	// Bundles are streamed; we don't know the final size up front. No
	// Content-Length header — http.Server falls through to chunked
	// transfer-encoding, which the gzip+tar pair handles fine.

	gzw := gzip.NewWriter(w)
	tw := tar.NewWriter(gzw)
	// Explicit Close ordering matters: tw.Close() flushes the tar
	// trailer (and reports if a previous entry got fewer bytes than
	// its declared Size — a corruption signal we don't want to drop
	// silently); gzw.Close() then flushes the gzip footer. We use a
	// single defer that handles both errors so a bug-bail-out path
	// (e.g. context cancel mid-stream) still surfaces a truncation.
	defer func() {
		if err := tw.Close(); err != nil {
			s.logBundleStreamError(r.Context(), "tar close", err)
		}
		if err := gzw.Close(); err != nil {
			s.logBundleStreamError(r.Context(), "gzip close", err)
		}
	}()

	// 1. pad-export.json
	exportJSON, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		s.logBundleStreamError(r.Context(), "marshal export", err)
		return
	}
	if err := writeTarFile(tw, "pad-export.json", exportJSON); err != nil {
		s.logBundleStreamError(r.Context(), "write pad-export.json", err)
		return
	}

	// 2. attachments/manifest.json
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		s.logBundleStreamError(r.Context(), "marshal manifest", err)
		return
	}
	if err := writeTarFile(tw, "attachments/manifest.json", manifestJSON); err != nil {
		s.logBundleStreamError(r.Context(), "write manifest", err)
		return
	}

	// 3. attachment blobs
	for _, a := range attachments {
		if err := s.streamAttachmentToTar(r.Context(), tw, &a); err != nil {
			s.logBundleStreamError(r.Context(), "stream attachment", err,
				"attachment_id", a.ID, "storage_key", a.StorageKey)
			return
		}
	}
}

// streamAttachmentToTar resolves the storage backend for one
// attachment row, writes a tar header sized to size_bytes, and copies
// the blob from the backend into the tar writer in 32 KiB chunks.
//
// Filename inside the tar is `attachments/<uuid><ext>` where ext
// comes from the original filename. Falls back to .bin when the
// upload had no extension. Using <uuid> avoids name collisions when
// two distinct attachments share the same display filename, which
// happens routinely with screenshots ("Screenshot 2025-...png").
func (s *Server) streamAttachmentToTar(ctx context.Context, tw *tar.Writer, a *models.Attachment) error {
	store, err := s.attachments.Resolve(a.StorageKey)
	if err != nil {
		return fmt.Errorf("resolve storage backend: %w", err)
	}
	body, err := store.Get(ctx, a.StorageKey)
	if err != nil {
		return fmt.Errorf("get blob: %w", err)
	}
	defer body.Close()

	hdr := &tar.Header{
		Name:    bundleAttachmentPath(a.ID, a.Filename),
		Mode:    0o644,
		Size:    a.SizeBytes,
		ModTime: a.CreatedAt.UTC(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header: %w", err)
	}
	n, err := io.Copy(tw, body)
	if err != nil {
		return fmt.Errorf("copy blob: %w", err)
	}
	// io.Copy on a backend that returns fewer bytes than expected
	// would otherwise return nil and the tar writer would surface a
	// "missed N bytes" error only at Close. Catch the truncation
	// here so the per-blob log carries the attachment id +
	// storage_key; the deferred tw.Close() then trips its own
	// missed-bytes error which we already log.
	if n != a.SizeBytes {
		return fmt.Errorf("blob truncated: copied %d bytes, expected %d (size_bytes column out of sync with backend?)",
			n, a.SizeBytes)
	}
	return nil
}

// bundleAttachmentPath builds the tar entry name for an attachment.
// Exported as a function (not a const helper) so the import path can
// import it and resolve manifest entries without duplicating the
// filename logic.
func bundleAttachmentPath(id, filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		ext = ".bin"
	}
	return "attachments/" + id + ext
}

// writeTarFile writes a single buffered file into the tar archive.
// Used for the small JSON entries (pad-export.json + manifest.json);
// blob entries stream through streamAttachmentToTar instead.
func writeTarFile(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{
		Name: name,
		Mode: 0o644,
		Size: int64(len(data)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	return nil
}

// logBundleStreamError logs at warn level with structured context.
// Mid-stream failures can't be turned into a clean HTTP error response
// (we've already started the response body), so the operator-facing
// log is the best we can do for diagnostics.
func (s *Server) logBundleStreamError(_ context.Context, op string, err error, kv ...any) {
	args := append([]any{"op", op, "error", err}, kv...)
	slog.Warn("export bundle stream failed", args...)
}
