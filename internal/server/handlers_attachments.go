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
	"github.com/PerpetualSoftware/pad/internal/store"

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

// multipartValues reads a field's values from the parsed multipart form
// ONLY. Unlike (*http.Request).FormValue it does not fall back to the
// URL query string, which keeps the two item_id input channels distinct
// so they can be resolved and cross-checked independently.
// multipartValues returns a multipart form field's TEXT values, dropping any
// that are not bindable text.
//
// The multipart body is exempt from BUG-2803's JSON rule for a good reason —
// its payload is binary blob content and must not be scanned for text
// validity — but its TEXT fields are a different thing: item_id goes to
// ResolveItem and into a database comparison exactly as the query-string
// channel does, and that channel has been validated at the transport since
// BUG-2784. A raw NUL arriving through the form instead of the query reached
// the store unchecked (codex round 4, BUG-2803).
//
// Dropping rather than erroring keeps this helper's signature and matches
// what callers already do with absent values: resolveUploadItemID treats
// absent and explicitly-empty alike, so an unusable value becomes "no value"
// and the request is answered by the same path that handles a missing one. A
// value that cannot name anything and one that was never sent are the same
// thing to the caller.
func multipartValues(r *http.Request, key string) []string {
	if r.MultipartForm == nil {
		return nil
	}
	raw := r.MultipartForm.Value[key]
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if bindableText(v) {
			out = append(out, v)
		}
	}
	return out
}

// maxUploadItemIDValues bounds how many item_id values one input channel
// may carry. Every distinct value costs a ResolveItem lookup, and semantic
// aliases of the same item (TASK-7 / task-7 / TASK-0007 all resolve alike)
// defeat exact-string deduplication — so the count is capped rather than
// relying on dedup to bound the work. No real client sends more than one.
const maxUploadItemIDValues = 8

// resolveUploadItemID resolves every distinct non-empty value a single
// item_id input channel carried, and returns the one item they all denote.
//
// A channel can legitimately carry several spellings of the same item (a
// UUID and a ref resolve identically), and net/http silently keeps only the
// first of a repeated field — so rather than first-wins, every value is
// resolved and they must agree. Returns (nil, true) when the channel
// carried no value at all (absent and explicitly-empty are the same thing,
// compared after TrimSpace); (nil, false) means a response was already
// written and the caller must return.
//
// Each resolved item is visibility-gated HERE, before the values are
// compared. That ordering matters: requireItemVisible writes a 404, so an
// item the caller cannot see is indistinguishable from one that does not
// exist. Comparing first would leak a hidden item's existence through the
// 400-vs-404 split (send a visible id plus the id being probed: a conflict
// means it exists, a 404 means it does not).
func (s *Server) resolveUploadItemID(w http.ResponseWriter, r *http.Request, workspaceID string, raw []string) (*models.Item, bool) {
	if len(raw) > maxUploadItemIDValues {
		writeError(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("At most %d item_id values are accepted", maxUploadItemIDValues))
		return nil, false
	}

	var resolved *models.Item
	seen := make(map[string]struct{}, len(raw))
	for _, value := range raw {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}

		item, err := s.store.ResolveItem(workspaceID, value)
		if err != nil {
			writeInternalError(w, err)
			return nil, false
		}
		if item == nil {
			// Deliberately identical to requireItemVisible's 404 below
			// (server.go). A distinct code/message here would restore the
			// existence oracle the visibility-before-compare ordering was
			// added to close: "hidden" and "does not exist" must be
			// indistinguishable to the caller.
			writeError(w, http.StatusNotFound, "not_found", "Item not found")
			return nil, false
		}
		if !s.requireItemVisible(w, r, workspaceID, item) {
			return nil, false
		}
		if resolved == nil {
			resolved = item
			continue
		}
		if item.ID != resolved.ID {
			writeError(w, http.StatusBadRequest, "item_id_conflict",
				"Conflicting item_id values refer to different items")
			return nil, false
		}
	}
	return resolved, true
}

// requireUploadItemEdit applies the edit gate to an upload's resolved
// parent item. Visibility was already established by resolveUploadItemID;
// this adds the 403 for an item the caller can see but not edit.
//
// Note that requireEditPermission's editor/owner fast path does not consult
// collection visibility on its own — the visibility gate ahead of it is what
// stops a member with collection_access="specific" attaching to an item in a
// collection hidden from them.
func (s *Server) requireUploadItemEdit(w http.ResponseWriter, r *http.Request, workspaceID string, item *models.Item) bool {
	return s.requireEditPermission(w, r, workspaceID, item.ID, item.CollectionID)
}

// handleUploadAttachment accepts a multipart upload and writes it into
// the attachments table + the configured AttachmentStore.
//
// Flow:
//  1. Auth: grant-aware edit permission on the associated item, or —
//     for an upload with no item context — editor+ role on the workspace
//     (on top of the route's RequireWorkspaceAccess middleware).
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
	if s.attachments == nil {
		writeError(w, http.StatusServiceUnavailable, "attachments_disabled",
			"Attachment storage is not configured on this server")
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	// Authorize the upload. The surfaces that offer upload (rich item
	// editor, comment composer) gate their affordance on grant-aware edit
	// permission, so a guest holding an item/collection edit grant via a
	// share link — but no workspace editor role — must be able to attach
	// (BUG-1661). When the caller associates the upload with an item we
	// authorize against that item's grant chain via requireEditPermission.
	// Free-floating uploads (new-item creation, storage settings) carry no
	// item context, so they keep the flat workspace editor-role gate.
	//
	// item_id arrives on TWO channels: the query string (the web client
	// sends both, web/src/lib/api/client.ts) and the multipart form (the
	// CLI sends form-only, internal/cli/client.go). Only the query channel
	// can be read before the body is spooled, so authorization is split:
	// the query value is resolved and authorized here (cheap, so a doomed
	// upload never spools), and the form value is resolved after parsing
	// (see below). The no-item editor gate is DEFERRED until after parsing
	// — firing it here would 403 a form-only item-grant guest before the
	// association is even known (PLAN-2391 DR-2).
	resolvedItem, ok := s.resolveUploadItemID(w, r, workspaceID, r.URL.Query()["item_id"])
	if !ok {
		return
	}
	if resolvedItem != nil {
		if !s.requireUploadItemEdit(w, r, workspaceID, resolvedItem) {
			return
		}
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

	// ParseMultipartForm spills anything past multipartParseMemory to a
	// temp file under TMPDIR. file.Close() below closes the handle but
	// does NOT remove that file — only RemoveAll does. Registered before
	// the parse call (and nil-guarded) so it also covers the partial form
	// a failed parse can leave behind, and so it runs on EVERY exit path
	// including success (PLAN-2391 DR-2). Defers are LIFO, so this runs
	// after the `defer file.Close()` registered below it.
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

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

	// Second item_id channel: the multipart form. Resolved in-workspace
	// just like the query channel. Two raw strings can legitimately denote
	// the SAME item (query "TASK-12", form "<uuid>"), so the comparison is
	// on the resolved canonical IDs — not on the caller's spelling. Absent
	// and explicitly-empty both mean "no value". Deliberately reads
	// r.MultipartForm.Value rather than r.FormValue, which merges the query
	// string back in and would collapse the two channels into one.
	formItem, ok := s.resolveUploadItemID(w, r, workspaceID, multipartValues(r, "item_id"))
	if !ok {
		return
	}
	if formItem != nil {
		switch {
		case resolvedItem == nil:
			// Form-only association (the CLI's shape): authorize through
			// this item's grant chain now that we know what it is.
			if !s.requireUploadItemEdit(w, r, workspaceID, formItem) {
				return
			}
			resolvedItem = formItem
		case formItem.ID != resolvedItem.ID:
			writeError(w, http.StatusBadRequest, "item_id_conflict",
				"Query and form item_id refer to different items")
			return
		}
	}

	// No item context at all → the flat workspace editor-role gate. This
	// is the gate that used to fire before parsing; it can only run here,
	// once both channels are known to be empty.
	//
	// Accepted cost of moving it (PLAN-2391 DR-2): a caller below editor
	// now spools the body before being rejected here, where previously a
	// free-floating viewer was turned away pre-parse. Bounded by
	// attachmentMaxBytes (25 MiB default) and reachable only by an
	// authenticated workspace member, so it is temp-disk/CPU pressure —
	// never an attachment-write bypass, since no row and no blob are
	// written on this path. The gate CANNOT move back: the form channel
	// is unreadable until the body is parsed, and firing early is exactly
	// the bug that 403'd form-only item-grant guests.
	if resolvedItem == nil && !requireMinRole(w, r, "editor") {
		return
	}

	// Persist the canonical UUID, never the caller's string — ResolveItem
	// accepts a UUID, a ref, or a slug, and a ref stored in item_id is a
	// malformed row (PLAN-2391 DR-2).
	var itemIDPtr *string
	if resolvedItem != nil {
		canonicalItemID := resolvedItem.ID
		itemIDPtr = &canonicalItemID
	}

	// Sanitize the filename: strip path components so a client can't
	// sneak directory traversal through the display name. We don't
	// store this in the storage backend — only in the DB row for UI.
	// The uploaded filename is caller-supplied text bound for a text column,
	// and a multipart header can carry a NUL through the RFC 5987 encoded
	// form (filename*=UTF-8\'\'a%00.png). Same predicate as the path and
	// query rules; falling back to a generic name rather than refusing the
	// upload, because the bytes are fine and only the label is unusable
	// (codex round 4, BUG-2803).
	// Reduce to a leaf under BOTH separator conventions before anything else.
	// filepath.Base is platform-specific, so on Unix it leaves a backslash
	// alone — and the stored name is consumed cross-platform (a Windows client
	// joining it onto a directory reads "..\\evil" as a traversal). Splitting
	// on the backslash too makes the stored value a safe path component
	// everywhere rather than only on the server's own OS (codex round 28).
	//
	// This NORMALISES rather than refuses: a legitimate Unix name containing a
	// backslash keeps its last segment instead of being replaced wholesale,
	// which is less lossy than the alternative and still portable.
	filename := filepath.Base(header.Filename)
	if i := strings.LastIndexByte(filename, '\\'); i >= 0 {
		filename = filename[i+1:]
	}
	if !bindableText(filename) {
		// Keep the EXTENSION when it is itself storable. The unusable part
		// of a name like "sh<NUL>ot.png" is the stem; ".png" is ordinary
		// text, and it is what every downstream consumer dispatches on —
		// Content-Disposition, the web download anchor, bundle export naming,
		// and `pad attachment view`, whose whole contract is handing a path to
		// something that opens files by extension.
		//
		// Dropping it made this fallback lossier than the empty-name one two
		// lines below, which has always produced "upload.bin" (codex round
		// 24). Bounded to a short extension so a hostile name cannot smuggle
		// a long tail through the fallback.
		// bindableText is NOT the bar here (codex round 25). It permits
		// control characters — they are valid UTF-8 and not NUL — and a
		// control character survives storage but is STRIPPED when the name
		// is written into Content-Disposition. So ".s<VT>vg" passes the
		// extension blocklist, which sees no known extension, and reappears
		// as ".svg" at the client. attachments.SafeFallbackExtension requires
		// a KNOWN, ALLOWED, plain-alphanumeric extension instead, so a
		// synthesised name can only carry a suffix the product already
		// accepts on the ordinary path.
		if ext := filepath.Ext(filename); attachments.SafeFallbackExtension(ext) {
			filename = "upload" + strings.ToLower(ext)
		} else {
			filename = "upload"
		}
	}
	// A name that is only dots or separators is not a filename, it is a PATH
	// COMPONENT, and consumers join it onto a directory. ".." was missing
	// here: it survives bindableText, and filepath.Ext("..") is "." — non-empty
	// — so even an extension check passes it through, while filepath.Join on
	// the client side resolves it to the PARENT directory (codex round 26).
	//
	// Only "." and ".." are path components; "..." and longer runs are
	// ordinary POSIX filenames and are kept (codex round 27 — the trimmed-form
	// check I used first refused those too, which is over-refusal for no
	// gain). A backslash is likewise a legal character in a Unix filename, and
	// filepath.Base above has already reduced any "a/b" to "b", so a separator
	// test here is dead on this platform and was removed rather than left to
	// look load-bearing.
	//
	// What remains is the real hazard: a name a consumer joins onto a
	// directory and lands somewhere else.
	if filename == "" || filename == "." || filename == ".." || filename == "/" {
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
	blobStore, err := s.attachments.Resolve(attachments.FSPrefix + ":" + hash)
	if err != nil {
		writeInternalError(w, fmt.Errorf("resolve attachment store: %w", err))
		return
	}
	// Fence Put + CreateAttachment against the orphan GC. The
	// in-flight tracker keeps the GC from reclaiming a blob with
	// this hash between Put (blob lands on disk) and
	// CreateAttachment (DB row inserted) — without it, a GC sweep
	// of an old soft-deleted row sharing the same hash could delete
	// the blob and strand the new live row. Released after the
	// CreateAttachment call below (whether it succeeds or fails).
	releaseInFlight := s.markUploadInFlight(hash)
	defer releaseInFlight()
	storageKey, err := blobStore.Put(r.Context(), hash, entry.MIME, tmp)
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
	// CreateAttachmentForLiveItem, not CreateAttachment: the parent item was
	// validated near the top of this handler, but everything since — spooling
	// the body to a temp file, hashing it, probing dimensions, store.Put —
	// is unbounded work, and item deletion commits in its own transaction.
	// Without the re-check under a row lock inside the insert, an item
	// archived during the upload window leaves a quota-counted live row bound
	// to an archived parent whose bytes the read gate (DR-13) then refuses to
	// serve. Same invariant, same helper, and for the same reason as the
	// transform path (PLAN-2391 DR-14).
	//
	// Derivation is the deliberate exception and stays on the unlocked path —
	// see the comment on thumbnailParentItemLive for that trade.
	if err := s.store.CreateAttachmentForLiveItem(att); err != nil {
		if errors.Is(err, store.ErrAttachmentParentItemGone) {
			// The same 404 every other attachment denial writes. The caller
			// must not be able to tell "your item was archived mid-upload"
			// from "no such attachment", and the row would be unreadable
			// under DR-13 regardless.
			writeAttachmentNotFound(w)
			return
		}
		// Note: the blob is now ROWLESS on disk — no attachments row
		// references it, so the row-driven orphan GC (TASK-886) cannot
		// see it. The rowless-blob sweep (runRowlessBlobSweep, BUG-2406)
		// reclaims it once its mtime ages past the GC grace period. We
		// deliberately do NOT delete it here: the same hash may back a
		// concurrent upload that succeeded its DB insert, and the sweep
		// already owns the guards (any-state row check, in-flight fence,
		// age gate) that make the delete safe. The refusal above lands
		// in the same shape.
		writeInternalError(w, fmt.Errorf("create attachment row: %w", err))
		return
	}

	// Uploads count as user writes for engagement-metric purposes (PLAN-1542
	// / TASK-1543). Attachments don't go through logActivity, so the hook is
	// explicit here. Throttled + no-ops on empty userID.
	s.store.TouchUserWrite(r.Context(), currentUserID(r))

	// Bump the storage-usage cache so the next GET sees fresh used_bytes
	// without waiting for TTL expiry. Done eagerly here (and after each
	// thumbnail derivation / transform) so the Settings → Storage UI
	// stays consistent with the actual on-disk total.
	s.storageInfoCache.invalidate(workspaceID)

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

// writeAttachmentNotFound is the single denial writer for the blob read
// path. Every authorization-dependent refusal — attachment missing, wrong
// workspace, soft-deleted, foreign parent item, archived parent, parent not
// visible to the caller — emits this exact response. Byte-identical bodies
// are the point: a distinguishable error code or message would turn the
// response into an existence oracle for rows and items the caller may not
// see (the house rule TASK-2400 established).
func writeAttachmentNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "Attachment not found")
}

// handleGetAttachment streams an attachment back to the caller. Auth is
// enforced by the route's RequireWorkspaceAccess middleware; this handler
// then authorizes per-attachment.
//
// Authorization (PLAN-2391 DR-10), in this exact order:
//
//  1. Load the attachment; 404 unless it is live and belongs to the
//     request workspace.
//  2. Item-bound rows: load the parent, verify the parent's workspace
//     identity, then requireItemVisible. Item visibility IS blob-read
//     permission — a guest holding an item or collection grant can fetch
//     the bytes rendered inside the item they were shared. The handler
//     used to open with a flat requireMinRole("viewer"), and roleLevel
//     ("guest") is 0, so every grant-based guest was 403'd before any
//     item-level check ran (BUG-2386) and inline images broke for them.
//  3. Orphan (item_id IS NULL) rows keep the flat viewer+ gate — there is
//     no item context to authorize against.
//
// The parent is loaded with GetItem, not GetItemIncludeDeleted (DR-13): a
// soft-deleted parent 404s here. Archiving an item hides it, so its bytes
// should not stay downloadable behind a URL. The DELETE path deliberately
// diverges — it keeps GetItemIncludeDeleted so an owner can still reclaim
// quota from an archived item's attachments.
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
	// FIRST statement, before any lookup or gate. Every denial below is
	// authorization-dependent, and an unlabelled 404 is heuristically
	// cacheable BY URL — a CDN or browser could retain it and go on
	// denying a caller who has since been granted access. writeError →
	// writeJSON calls WriteHeader immediately, so a header set after the
	// first denial path never reaches the wire. Overwritten with the
	// positive directive only once authorization has succeeded.
	//
	// Scope note: middleware can reject before this handler ever runs
	// (missing auth, no workspace access). Those denials are outside this
	// header's coverage.
	w.Header().Set("Cache-Control", "private, no-store")

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
		writeAttachmentNotFound(w)
		return
	}

	// Item-level authorization (PLAN-2391 DR-10). Runs BEFORE the variant
	// lookup and before any byte access.
	//
	// includeArchived=false — DR-13. A soft-deleted parent reports Gone and
	// the blob 404s. The delete path is the one caller that passes true.
	item, parentOutcome, err := s.resolveAttachmentParentItem(att, false)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	switch parentOutcome {
	case attachmentParentOK:
		// checkItemVisible rather than requireItemVisible: same rule set
		// (requireItemVisible is a thin wrapper over it), but this handler
		// writes its own denial so that "no such attachment", "foreign
		// parent", "archived parent", and "item not visible to you" are
		// byte-identical responses. requireItemVisible's own 404 body says
		// "Item not found", which would distinguish the visibility denial
		// from the lookup miss and leak existence.
		visible, vErr := s.checkItemVisible(workspaceID, item, currentUser(r), workspaceRole(r), isBearerAuth(r))
		if vErr != nil {
			writeInternalError(w, vErr)
			return
		}
		if !visible {
			writeAttachmentNotFound(w)
			return
		}
	case attachmentParentOrphan:
		// Orphan attachments carry no item context to authorize against.
		//
		// Full-access only, matching the transform and DELETE paths
		// (PLAN-2382 DR-4) — and ahead of the role check, for the reason
		// those paths document: an orphan belongs to no collection, so
		// collection visibility cannot gate it, and the storage LISTING
		// already hides orphans from restricted members. Reading one must
		// not confirm it exists to a member the listing hides it from.
		// This gate was missing here while transform and DELETE both had
		// it, so a restricted editor or viewer who guessed an orphan's
		// UUID could download it.
		restricted, rErr := s.attachmentCallerIsRestricted(r, workspaceID)
		if rErr != nil {
			writeInternalError(w, rErr)
			return
		}
		if restricted {
			writeAttachmentNotFound(w)
			return
		}
		// Then the flat workspace viewer-role gate — same strength as
		// before this handler grew per-attachment authorization, only the
		// denial SHAPE changed.
		//
		// requireRole (the write-free predicate) rather than requireMinRole
		// (which writes its own 403): the attachment row has already been
		// loaded by this point, so a 403 here would mean "this id names a
		// live orphan in this workspace" while a bad id answers 404. That
		// split is an existence oracle for orphan UUIDs — one the old code
		// didn't have only because its flat role gate ran BEFORE the
		// lookup. Reordering the gate reintroduced it; routing the denial
		// through the shared 404 writer closes it again.
		if !requireRole(r, "viewer") {
			writeAttachmentNotFound(w)
			return
		}
	default: // Gone, Foreign
		writeAttachmentNotFound(w)
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
		// Workspace-scoped (DR-16). parent_id alone is not a trustworthy
		// scope: a variant row in another workspace can carry this
		// attachment's id as its parent, and serving that child after
		// authorizing this parent would defeat the gate above. The scope
		// lives in the store method so the derivation worker gets it too.
		if derived, dErr := s.store.GetAttachmentVariant(workspaceID, att.ID, variant); dErr != nil {
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
	//
	// Content-Type AND Content-Disposition are a security decision here, not a
	// convenience (BUG-2413). FAIL CLOSED: serve inline only a stored MIME that is
	// on the allowlist AND explicitly inline-safe (passive media the browser
	// renders in place, plus PDF and plain text — see MIMEEntry.ServeInline).
	// Everything else — the rest of the RenderChip bucket (xml/json/csv/office/
	// archives), the RenderForceDownload bucket, and any MIME we don't recognize
	// at all — is served as an attachment, so a legacy or mislabelled
	// image/svg+xml, an extensionless SVG stored as text/xml, or an unknown row
	// can never render as same-origin active content. The default was previously
	// inline, which is exactly the fail-open this closes.
	contentType := att.MimeType
	disposition := "attachment"
	if entry, ok := attachments.LookupMIME(att.MimeType); ok {
		if entry.ServeInline() {
			disposition = "inline"
		}
	} else {
		// Not on the allowlist at all: don't echo back a type the browser might
		// act on. Opaque bytes, paired with the attachment disposition and the
		// nosniff below, make an unrecognized row inert.
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	// Authorization has succeeded — replace the no-store denial directive
	// set at the top of the handler with the positive one. Known and
	// accepted: this one-hour positive browser cache outlives a permission
	// revocation (PLAN-2391 DR-10).
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
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
