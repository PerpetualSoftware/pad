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

// importMetadataMaxBytes is the size ceiling for the small JSON
// payloads inside a bundle (pad-export.json + attachments/manifest.json).
// Independent of the per-blob cap so a deployment that LOWERS
// PAD_ATTACHMENT_MAX_BYTES (e.g. to 1 MiB for a tightly-controlled
// host) doesn't inadvertently reject metadata for a workspace
// nobody intended to gate on attachment-blob limits. (Codex P2 on
// PR #306 round 5.)
//
// 100 MiB comfortably holds a workspace with many thousands of
// items + version history. Bumped via the import bundle cap, not
// per-attachment cap, since metadata size scales with the export's
// item count, not attachment sizes.
const importMetadataMaxBytes int64 = 100 << 20 // 100 MiB

// effectiveBlobMaxBytes returns the per-blob ceiling for bundle
// import — matches whatever the upload handler accepts so an
// operator who raised PAD_ATTACHMENT_MAX_BYTES on the source can
// re-import the resulting export on a destination configured the
// same way. Codex flagged the hard-coded 25 MiB cap on PR #306
// round 4: a workspace with attachments uploaded under a larger
// cap would round-trip the export but fail the re-import.
func (s *Server) effectiveBlobMaxBytes() int64 {
	if s.attachmentMaxBytes > 0 {
		return s.attachmentMaxBytes
	}
	return defaultAttachmentMaxBytes
}

// handleImportWorkspaceBundle accepts a tar.gz bundle produced by
// the export endpoint and rebuilds the workspace including all
// attachment blobs.
//
// The handler does the JSON / bundle dispatch; the actual work is
// done by importBundle which is unit-testable without the http stack.
//
// Auth: any authenticated user. The global RequireAuth middleware
// (server.go:539) gates this endpoint when users exist on the host.
// There is no per-workspace role check because import CREATES a new
// workspace — there's nothing pre-existing to authorize against.
// The importing user becomes the workspace owner via AddWorkspaceMember
// after a successful import (mirrors handleCreateWorkspace).
//
// Quota: per-user storage quotas are NOT enforced on import. The
// upload handler is also warn-only in Phase 1 (see handlers_attachments.go
// maybeWarnStorageQuota). When quota enforcement lands the import
// path needs the same gate. Tracked as a follow-up under PLAN-890.
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
		isValidationReject := errors.As(err, &statusErr)

		// If a clean validation-phase reject happens AFTER the
		// workspace has been created (e.g. a duplicate pad-export.json
		// or path-traversal entry that follows the first export
		// header), roll back the partial workspace so a malicious
		// or malformed bundle can't pile up half-imported workspaces
		// in the destination instance. Codex P1 on PR #308.
		//
		// Cascade: a duplicate manifest.json or duplicate pad-export.json
		// can fire AFTER blobs have already been rehydrated — those
		// attachment rows would otherwise stay live (deleted_at IS NULL),
		// pin their blobs from orphan-GC, and count toward the
		// importing user's storage usage. Tombstone every attachment
		// in the partial workspace BEFORE soft-deleting the workspace
		// itself so orphan-GC reclaims the blobs after the grace
		// window. Codex P1 round 2 on PR #308.
		//
		// Mid-stream errors that are NOT importStatusError (e.g.
		// manifest decode failure after items inserted) intentionally
		// keep the partial workspace — the existing comment on the
		// manifest decode path notes "workspace created but
		// attachments not restored" and that decision is tracked
		// separately under TASK-896 (partial-import design).
		if isValidationReject && ws != nil {
			if n, attErr := s.store.SoftDeleteWorkspaceAttachments(ws.ID); attErr != nil {
				slog.Warn("import: failed to tombstone partial-workspace attachments",
					"workspace_id", ws.ID, "error", attErr)
			} else if n > 0 {
				slog.Info("import: rolled back partial-workspace attachments",
					"workspace_id", ws.ID, "rows", n)
			}
			if delErr := s.store.DeleteWorkspace(ws.Slug); delErr != nil {
				slog.Warn("import: failed to roll back partial workspace after validation reject",
					"workspace_slug", ws.Slug, "error", delErr)
			}
		}

		if isValidationReject {
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
// Memory footprint: at most one blob (≤ effectiveBlobMaxBytes) held
// at a time during rehydration, plus the small JSON payloads at the
// front. A 2 GiB bundle with thousands of 25 MiB images now needs
// ~25 MiB peak rather than ~2 GiB. (Codex P1 on PR #306 round 1.)
//
// Split out from the handler so tests can drive it with a tar.Reader
// over an in-memory bundle and assert on the resulting state without
// a live HTTP server.
func (s *Server) importBundle(ctx context.Context, r io.Reader, newName, ownerID string) (*models.Workspace, error) {
	tr := tar.NewReader(r)
	blobCap := s.effectiveBlobMaxBytes()

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

		// Defense-in-depth path-traversal rejection. The bundle path
		// is hash-keyed at the storage layer (see rehydrateAttachment
		// → store.Put with the locally-computed sha256), so a tar
		// entry name with `..` or an absolute path can't actually
		// write outside the attachment store. We still reject these
		// up front so the audit story is unambiguous and so a bundle
		// that's been hand-edited to look malicious fails loudly
		// rather than silently being skipped via the `default` arm.
		//
		// Return ws here (not nil) so the handler can roll back any
		// workspace that was already created by a preceding valid
		// pad-export.json. Before pad-export.json is seen, ws is nil
		// so this falls through to the no-workspace cleanup path
		// anyway. Codex P1 round 3 on PR #308.
		if !isSafeBundleEntryName(hdr.Name) {
			return ws, &importStatusError{
				status: http.StatusBadRequest, code: "bad_bundle",
				message: "Bundle contains unsafe entry name: " + hdr.Name,
			}
		}

		switch {
		case hdr.Name == "pad-export.json":
			// Bundles must contain exactly one pad-export.json.
			// A second occurrence would call ImportWorkspace again,
			// stranding the first workspace as an orphan with no
			// attachments. Reject duplicates loudly.
			if exportSeen {
				return ws, &importStatusError{
					status: http.StatusBadRequest, code: "bad_bundle",
					message: "Bundle contains duplicate pad-export.json",
				}
			}
			// pad-export.json can grow large for content-heavy
			// workspaces (items + version history). Use the
			// metadata-specific cap so deployments that lower
			// PAD_ATTACHMENT_MAX_BYTES (e.g. to 1 MiB) don't
			// inadvertently make metadata fail.
			if hdr.Size > importMetadataMaxBytes {
				return nil, fmt.Errorf("pad-export.json exceeds %d-byte cap (declared %d)", importMetadataMaxBytes, hdr.Size)
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
			// Reject duplicate manifest.json — a second occurrence
			// would silently overwrite manifestByPath, dropping prior
			// entries and leaving any blobs that referenced them
			// looking orphaned (manifest lookup miss → consumed and
			// skipped). Same shape as the duplicate-export guard.
			if manifestSeen {
				return ws, &importStatusError{
					status: http.StatusBadRequest, code: "bad_bundle",
					message: "Bundle contains duplicate attachments/manifest.json",
				}
			}
			// Manifest size scales with attachment count, not blob
			// content, so use the metadata cap rather than the
			// per-blob one (same rationale as pad-export.json above).
			if hdr.Size > importMetadataMaxBytes {
				return ws, fmt.Errorf("manifest.json exceeds %d-byte cap (declared %d)",
					importMetadataMaxBytes, hdr.Size)
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
			if hdr.Size > blobCap {
				return ws, fmt.Errorf("blob %s exceeds %d-byte cap (declared %d) — raise PAD_ATTACHMENT_MAX_BYTES on this server to allow",
					hdr.Name, blobCap, hdr.Size)
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
	// Fence the Put + CreateAttachment pair against orphan-GC blob
	// deletion (Codex P2 on PR #307). The bundle import races GC the
	// same way uploads do — possibly more so, since a workspace
	// re-import touches thousands of hashes in quick succession.
	releaseInFlight := s.markUploadInFlight(hash)
	defer releaseInFlight()
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

// isSafeBundleEntryName rejects tar entry names that look like a
// path-traversal attempt. The bundle path is already hash-keyed at
// the storage layer (see rehydrateAttachment), so a malicious entry
// name can't actually escape the attachment store — but rejecting
// up front makes the audit story unambiguous and means hand-edited
// bundles fail loudly rather than silently slipping through the
// `default` arm of importBundle's switch.
//
// Rules:
//   - Reject absolute paths ("/etc/passwd", "\\windows\\system32").
//   - Reject any segment equal to "..". A literal "." segment is
//     rare but harmless; we only block ".." since that's the
//     traversal vector.
//   - Reject embedded NUL bytes (defense against C-style truncation
//     bugs in any downstream consumer).
//
// Returns true when the entry name is safe to consume.
func isSafeBundleEntryName(name string) bool {
	if name == "" {
		return false
	}
	if strings.ContainsRune(name, 0) {
		return false
	}
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "\\") {
		return false
	}
	// Treat both forward- and back-slashes as separators for the
	// traversal check; tar names are canonically forward-slashed but
	// a malicious bundle could mix them to bypass a naive split.
	normalized := strings.ReplaceAll(name, "\\", "/")
	for _, segment := range strings.Split(normalized, "/") {
		if segment == ".." {
			return false
		}
	}
	return true
}
