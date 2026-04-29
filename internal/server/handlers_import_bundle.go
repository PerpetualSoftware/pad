package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// importBundleMaxBytes caps an uploaded bundle. Mirrors the upload
// handler's defaultAttachmentMaxBytes scaling: a single workspace
// export can contain thousands of attachments, so we set this much
// higher than any single-file upload limit. The cap exists primarily
// to bound the temp-file footprint on the import host — operators
// can raise it via SetAttachments once we surface it.
const importBundleMaxBytes = 2 << 30 // 2 GiB

// importBlobMaxBytes caps any single blob inside a bundle. Defense
// against a malicious bundle declaring a multi-TiB blob in the tar
// header and forcing us to read it all before checking quota. Same
// limit as the per-upload cap (25 MiB by default) — bundles can't
// smuggle larger blobs than the upload endpoint accepts.
const importBlobMaxBytes = 25 << 20

// handleImportWorkspaceBundle accepts a tar.gz bundle produced by
// the export endpoint and rebuilds the workspace including all
// attachment blobs.
//
// The handler does the JSON / bundle dispatch; the actual work is
// done by importBundle which is unit-testable without the http stack.
//
// Bundle layout (matches handlers_export_bundle.go):
//
//	pad-export.json
//	attachments/manifest.json
//	attachments/<uuid>.<ext>
//
// Two-phase flow:
//  1. Parse pad-export.json, run the existing ImportWorkspace path to
//     create the workspace + items. Returns an item-ID map (old → new).
//  2. For each manifest entry, find the matching tar entry, rehydrate
//     the blob through the storage backend (re-validate MIME + hash),
//     and insert an attachment row pointed at the remapped item.
//     Build an attachment-ID map (old → new) as we go.
//  3. Scan all imported items' content + fields for pad-attachment:OLD
//     references and rewrite to pad-attachment:NEW.
//
// Errors before phase 2 begins return a clean HTTP error. Errors mid-
// rehydrate are logged with attachment_id context; the workspace is
// kept (it has live items) and the partial attachment state is left
// for the operator to inspect. Orphan GC will eventually reclaim any
// blob whose row insertion failed — the upload-handler's "blob may be
// orphan on disk" comment applies here too.
func (s *Server) handleImportWorkspaceBundle(w http.ResponseWriter, r *http.Request) {
	if s.attachments == nil {
		writeError(w, http.StatusServiceUnavailable, "attachments_disabled",
			"Attachment storage is not configured on this server")
		return
	}
	// Bound the request body BEFORE the gzip reader spools any of it.
	r.Body = http.MaxBytesReader(w, r.Body, importBundleMaxBytes)

	gz, err := gzip.NewReader(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_bundle",
			"Could not read gzip stream: "+err.Error())
		return
	}
	defer gz.Close()

	newName := r.URL.Query().Get("name")
	userID := currentUserID(r)

	ws, err := s.importBundle(r.Context(), gz, newName, userID)
	if err != nil {
		// Errors from importBundle are already shaped with status hints —
		// surface as 400 unless the underlying error wraps an http hint.
		var statusErr *importStatusError
		if errors.As(err, &statusErr) {
			writeError(w, statusErr.status, statusErr.code, statusErr.message)
			return
		}
		writeError(w, http.StatusBadRequest, "import_failed", err.Error())
		return
	}

	// Mirror the JSON-import path's owner-attachment so the workspace
	// shows up under the importer's account.
	if userID != "" {
		_ = s.store.AddWorkspaceMember(ws.ID, userID, "owner")
	}
	writeJSON(w, http.StatusCreated, ws)
}

// importBundle reads a tar (already gzip-decompressed) from r and
// orchestrates the two-phase import. Returns the new workspace.
//
// Split out from the handler so tests can drive it with a tar.Reader
// over an in-memory bundle and assert on the resulting state without
// a live HTTP server.
func (s *Server) importBundle(ctx context.Context, r io.Reader, newName, ownerID string) (*models.Workspace, error) {
	tr := tar.NewReader(r)

	// Pass 1: walk the tar, capture pad-export.json + manifest.json
	// into memory, and remember each blob's offset so we can stream
	// it on pass 2 — but tar streams are forward-only, so we actually
	// just spool blobs into temp files keyed by tar-entry name. For
	// realistic bundle sizes (≤ 2GiB total, ≤25 MiB per blob) this is
	// fine; the temp-file lifecycle is bounded by this function.
	//
	// Future optimization: stream blobs straight through if we adopt
	// a manifest-first ordering convention. Current export already
	// writes manifest before blobs, so that's a one-line change once
	// we're confident no in-the-wild bundle violates it.
	var exportJSON []byte
	var manifestJSON []byte
	blobs := map[string][]byte{}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar entry: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA { //nolint:staticcheck // TypeRegA accepted for older bundles
			continue
		}
		// Hard cap per-blob size before we read into memory.
		if hdr.Size > importBlobMaxBytes {
			return nil, fmt.Errorf("entry %s exceeds %d-byte cap (declared %d)",
				hdr.Name, importBlobMaxBytes, hdr.Size)
		}
		buf, err := io.ReadAll(io.LimitReader(tr, hdr.Size+1))
		if err != nil {
			return nil, fmt.Errorf("read entry %s: %w", hdr.Name, err)
		}
		if int64(len(buf)) != hdr.Size {
			return nil, fmt.Errorf("entry %s: read %d bytes, header says %d", hdr.Name, len(buf), hdr.Size)
		}

		switch {
		case hdr.Name == "pad-export.json":
			exportJSON = buf
		case hdr.Name == "attachments/manifest.json":
			manifestJSON = buf
		case strings.HasPrefix(hdr.Name, "attachments/"):
			blobs[hdr.Name] = buf
		default:
			// Unknown top-level entry — ignored. Forward-compat for
			// future bundle additions (e.g. a CHANGELOG.md).
		}
	}

	if exportJSON == nil {
		return nil, &importStatusError{
			status: http.StatusBadRequest, code: "bad_bundle",
			message: "Bundle is missing pad-export.json",
		}
	}

	var export models.WorkspaceExport
	if err := json.Unmarshal(exportJSON, &export); err != nil {
		return nil, &importStatusError{
			status: http.StatusBadRequest, code: "bad_bundle",
			message: "Bundle pad-export.json could not be decoded: " + err.Error(),
		}
	}

	// Phase 1: import the workspace + items. Old→new item ID map is
	// computed inside ImportWorkspace; we don't have direct access to
	// it from outside, but we can reconstruct it by listing the items
	// after import. For TASK-885 the attachment rehydration only needs
	// the attachment-ID map; item references in markdown are resolved
	// by item slug → new id at PATCH time, which is a separate concern.
	//
	// For attachment rewrites specifically, we do need item-id mapping
	// because pad-attachment:UUID references aren't tied to items —
	// they're attachment UUIDs alone. So the rewrite scan walks every
	// imported item and replaces only attachment UUIDs.
	ws, err := s.store.ImportWorkspace(&export, newName, ownerID)
	if err != nil {
		return nil, fmt.Errorf("import workspace: %w", err)
	}

	// Phase 2: rehydrate attachments. Skip silently when the bundle
	// has no manifest — older bundles or hand-crafted ones without
	// attachments are still valid imports.
	if manifestJSON == nil {
		return ws, nil
	}

	var manifest models.AttachmentManifest
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return ws, fmt.Errorf("import manifest decode: %w (workspace created but attachments not restored)", err)
	}
	if manifest.Version > exportBundleVersion {
		return ws, fmt.Errorf("manifest version %d not supported by this server (max %d)",
			manifest.Version, exportBundleVersion)
	}

	// Build a slug→new-id map for items so we can remap manifest
	// entries (which have the OLD item id) to the NEW item id.
	// ImportWorkspace preserves item.slug, so this is straightforward.
	oldItemIDToSlug := make(map[string]string, len(export.Items))
	for _, it := range export.Items {
		oldItemIDToSlug[it.ID] = it.Slug
	}
	slugToNewID, err := s.store.WorkspaceItemSlugMap(ws.ID)
	if err != nil {
		return ws, fmt.Errorf("build slug→id map: %w", err)
	}

	oldAttachToNew := make(map[string]string, len(manifest.Entries))
	for _, e := range manifest.Entries {
		blob, ok := blobs[bundleAttachmentPath(e.ID, e.Filename)]
		if !ok {
			slog.Warn("import: manifest entry missing blob",
				"attachment_id", e.ID, "filename", e.Filename)
			continue
		}
		newAttID, err := s.rehydrateAttachment(ctx, ws.ID, &e, blob,
			oldItemIDToSlug, slugToNewID, ownerID)
		if err != nil {
			slog.Warn("import: rehydrate failed", "attachment_id", e.ID, "error", err)
			continue
		}
		oldAttachToNew[e.ID] = newAttID
	}

	// Phase 3: rewrite pad-attachment:OLD references in every imported
	// item's content + fields to pad-attachment:NEW. Done via store
	// helper so we get a single transactional pass and the FTS index
	// is updated correctly.
	if len(oldAttachToNew) > 0 {
		if err := s.store.RemapAttachmentReferencesInWorkspace(ws.ID, oldAttachToNew); err != nil {
			slog.Warn("import: attachment reference remap failed",
				"workspace_id", ws.ID, "error", err)
			// Non-fatal — items still exist with stale references.
			// Operator can re-run a remap manually if needed.
		}
	}

	// Drop the storage-usage cache — the imported attachments
	// just bumped the workspace total.
	s.storageInfoCache.invalidate(ws.ID)

	return ws, nil
}

// rehydrateAttachment runs the upload-handler's MIME validation,
// hash, and store.Put for one manifest entry, then inserts a fresh
// attachments row in the new workspace. Returns the new UUID. The
// new row points at the remapped item id when the original was
// attached to one; orphan attachments stay orphaned.
func (s *Server) rehydrateAttachment(
	ctx context.Context,
	workspaceID string,
	entry *models.AttachmentManifestEntry,
	blob []byte,
	oldItemIDToSlug, slugToNewID map[string]string,
	ownerID string,
) (string, error) {
	// Defense in depth: re-validate the MIME against the allowlist on
	// the first 512 bytes. Trusting the manifest's mime field would
	// let a malicious bundle smuggle a blocked type past the upload
	// gate. If the actual bytes don't sniff to the manifest's mime,
	// trust the sniffed value (matches the upload handler's policy).
	head := blob
	if len(head) > 512 {
		head = head[:512]
	}
	allowed, code, vErr := attachments.ValidateUpload(head, entry.Filename)
	if vErr != nil {
		return "", fmt.Errorf("mime validation (%s): %w", code, vErr)
	}

	// Hash the blob ourselves rather than trusting the manifest. A
	// bundle could lie about content_hash; the storage layer's
	// hash-verify guards us at write time, but hashing locally lets
	// the dedupe path work even when the supplied hash is wrong.
	sum := sha256.Sum256(blob)
	hash := hex.EncodeToString(sum[:])

	// Hand the bytes to the configured backend. Same path the upload
	// handler uses; FSStore re-hashes defensively.
	store, err := s.attachments.Resolve(attachments.FSPrefix + ":" + hash)
	if err != nil {
		return "", fmt.Errorf("resolve attachment store: %w", err)
	}
	storageKey, err := store.Put(ctx, hash, allowed.MIME, strings.NewReader(string(blob)))
	if err != nil {
		return "", fmt.Errorf("store.Put: %w", err)
	}

	// Translate the old item id (from the manifest) into the new id
	// via item.slug, which ImportWorkspace preserves.
	var newItemIDPtr *string
	if entry.ItemID != "" {
		if slug, ok := oldItemIDToSlug[entry.ItemID]; ok {
			if newID, ok := slugToNewID[slug]; ok && newID != "" {
				newItemIDPtr = &newID
			}
		}
	}

	uploadedBy := entry.UploadedBy
	if uploadedBy == "" {
		uploadedBy = ownerID
	}
	if uploadedBy == "" {
		uploadedBy = "system"
	}

	att := &models.Attachment{
		WorkspaceID: workspaceID,
		ItemID:      newItemIDPtr,
		UploadedBy:  uploadedBy,
		StorageKey:  storageKey,
		ContentHash: hash,
		MimeType:    allowed.MIME,
		SizeBytes:   int64(len(blob)),
		Filename:    entry.Filename,
		Width:       entry.Width,
		Height:      entry.Height,
	}
	if err := s.store.CreateAttachment(att); err != nil {
		return "", fmt.Errorf("create attachment row: %w", err)
	}

	// Re-derive thumbnails for image originals. Mirrors the upload
	// handler — runs async via goAsync so the import handler doesn't
	// stall on imaging work, and Server.Stop() waits for in-flight
	// derivation before close.
	if allowed.Category == attachments.CategoryImage && s.imageProcessor != nil {
		original := att.ID
		s.goAsync(func() { s.deriveThumbnails(original) })
	}

	return att.ID, nil
}

// importStatusError lets importBundle return errors with HTTP-status
// hints attached, so the handler doesn't have to repeat the
// classification. Keeps importBundle pure-Go-testable.
type importStatusError struct {
	status  int
	code    string
	message string
}

func (e *importStatusError) Error() string { return e.message }
