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

// defaultImportBundleMaxBytes caps an uploaded bundle. Mirrors the
// upload handler's defaultAttachmentMaxBytes scaling: a workspace
// export can contain thousands of attachments, so this is much
// higher than any single-file upload limit. The cap exists primarily
// to bound the temp-file footprint on the import host. Operators
// running larger workspaces should override via
// Server.SetImportBundleMaxBytes (wired from PAD_IMPORT_BUNDLE_MAX_BYTES
// in cmd/pad/main.go).
const defaultImportBundleMaxBytes int64 = 2 << 30 // 2 GiB

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
	maxBytes := s.importBundleMaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultImportBundleMaxBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

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
// Single-pass streaming: the export bundler always writes
// pad-export.json + attachments/manifest.json BEFORE any blob, so
// we can run ImportWorkspace + parse the manifest as soon as those
// two entries land, then stream-rehydrate each subsequent blob
// without ever holding the full bundle in memory. Bundles that
// violate the ordering — e.g. a third-party tool that put blobs
// first — are rejected with a clear error.
//
// Memory footprint: at most one blob (≤ importBlobMaxBytes) held at
// a time during rehydration, plus the small JSON payloads at the
// front. A 2 GiB bundle with thousands of 25 MiB images now needs
// ~25 MiB peak rather than ~2 GiB. (Codex P1 on PR #306 round 1.)
//
// Split out from the handler so tests can drive it with a tar.Reader
// over an in-memory bundle and assert on the resulting state without
// a live HTTP server.
func (s *Server) importBundle(ctx context.Context, r io.Reader, newName, ownerID string) (*models.Workspace, error) {
	tr := tar.NewReader(r)

	var ws *models.Workspace
	var manifestByPath map[string]*models.AttachmentManifestEntry
	var oldItemIDToSlug, slugToNewID map[string]string
	oldAttachToNew := map[string]string{}
	exportSeen := false
	manifestSeen := false

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

		switch {
		case hdr.Name == "pad-export.json":
			if hdr.Size > importBlobMaxBytes*4 {
				// Allow up to 100 MiB for the JSON payload — items +
				// content + versions can grow large. Still bounded so
				// a malicious header can't force unbounded read.
				return nil, fmt.Errorf("pad-export.json exceeds 100 MiB cap (declared %d)", hdr.Size)
			}
			buf, err := readEntry(tr, hdr.Size)
			if err != nil {
				return nil, fmt.Errorf("read pad-export.json: %w", err)
			}
			var export models.WorkspaceExport
			if err := json.Unmarshal(buf, &export); err != nil {
				return nil, &importStatusError{
					status: http.StatusBadRequest, code: "bad_bundle",
					message: "Bundle pad-export.json could not be decoded: " + err.Error(),
				}
			}
			ws, err = s.store.ImportWorkspace(&export, newName, ownerID)
			if err != nil {
				return nil, fmt.Errorf("import workspace: %w", err)
			}
			oldItemIDToSlug = make(map[string]string, len(export.Items))
			for _, it := range export.Items {
				oldItemIDToSlug[it.ID] = it.Slug
			}
			slugToNewID, err = s.store.WorkspaceItemSlugMap(ws.ID)
			if err != nil {
				return ws, fmt.Errorf("build slug→id map: %w", err)
			}
			exportSeen = true

		case hdr.Name == "attachments/manifest.json":
			if !exportSeen {
				return nil, &importStatusError{
					status: http.StatusBadRequest, code: "bad_bundle",
					message: "Bundle ordering violation: manifest.json before pad-export.json",
				}
			}
			if hdr.Size > importBlobMaxBytes {
				return ws, fmt.Errorf("manifest.json exceeds %d-byte cap (declared %d)",
					importBlobMaxBytes, hdr.Size)
			}
			buf, err := readEntry(tr, hdr.Size)
			if err != nil {
				return ws, fmt.Errorf("read manifest.json: %w", err)
			}
			var manifest models.AttachmentManifest
			if err := json.Unmarshal(buf, &manifest); err != nil {
				return ws, fmt.Errorf("manifest decode: %w (workspace created but attachments not restored)", err)
			}
			if manifest.Version > exportBundleVersion {
				return ws, fmt.Errorf("manifest version %d not supported by this server (max %d)",
					manifest.Version, exportBundleVersion)
			}
			manifestByPath = make(map[string]*models.AttachmentManifestEntry, len(manifest.Entries))
			for i := range manifest.Entries {
				e := &manifest.Entries[i]
				manifestByPath[bundleAttachmentPath(e.ID, e.Filename)] = e
			}
			manifestSeen = true

		case strings.HasPrefix(hdr.Name, "attachments/"):
			if !exportSeen {
				return nil, &importStatusError{
					status: http.StatusBadRequest, code: "bad_bundle",
					message: "Bundle ordering violation: attachment blob before pad-export.json",
				}
			}
			if !manifestSeen {
				return ws, &importStatusError{
					status: http.StatusBadRequest, code: "bad_bundle",
					message: "Bundle ordering violation: attachment blob before manifest.json",
				}
			}
			entry, ok := manifestByPath[hdr.Name]
			if !ok {
				// Blob has no manifest entry — could be a stale entry
				// from a bundle the operator hand-edited. Skip the
				// bytes (consume the tar slot) and move on.
				if _, err := io.Copy(io.Discard, io.LimitReader(tr, hdr.Size)); err != nil {
					return ws, fmt.Errorf("skip unmanifested blob %s: %w", hdr.Name, err)
				}
				continue
			}
			if hdr.Size > importBlobMaxBytes {
				return ws, fmt.Errorf("blob %s exceeds %d-byte cap (declared %d)",
					hdr.Name, importBlobMaxBytes, hdr.Size)
			}
			blob, err := readEntry(tr, hdr.Size)
			if err != nil {
				return ws, fmt.Errorf("read blob %s: %w", hdr.Name, err)
			}
			newAttID, err := s.rehydrateAttachment(ctx, ws.ID, entry, blob,
				oldItemIDToSlug, slugToNewID, ownerID)
			if err != nil {
				slog.Warn("import: rehydrate failed",
					"attachment_id", entry.ID, "error", err)
				continue
			}
			oldAttachToNew[entry.ID] = newAttID

		default:
			// Unknown top-level entry — consume it so the tar reader
			// stays in sync, then forward-compat ignore. Future
			// bundle versions might add a CHANGELOG.md or schema
			// migration script we don't recognize yet.
			if _, err := io.Copy(io.Discard, io.LimitReader(tr, hdr.Size)); err != nil {
				return ws, fmt.Errorf("skip unknown entry %s: %w", hdr.Name, err)
			}
		}
	}

	if !exportSeen {
		return nil, &importStatusError{
			status: http.StatusBadRequest, code: "bad_bundle",
			message: "Bundle is missing pad-export.json",
		}
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

// readEntry reads exactly size bytes from a tar reader (the rest of
// the current entry) into a buffer, validating that the read length
// matches the header's declared Size. Tar entries are bounded by the
// caller; this helper just makes the read+verify pattern uniform.
func readEntry(tr *tar.Reader, size int64) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(tr, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) != size {
		return nil, fmt.Errorf("read %d bytes, header says %d", len(buf), size)
	}
	return buf, nil
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
