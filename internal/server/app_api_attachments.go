package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/appstore"
	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// App attachments over the app API (TASK-3401 U6c, DOC-3371 §4). The store
// side, the upload sequence and the read rule are appstore's (U7a); these
// handlers add the request shape, the write gate, the visibility and
// re-check registration, and serving.

// appAttachmentRouteDeadline bounds an upload or a download's work. It is not
// a correctness proof (DOC-3371 §4): a download that passed its gate streams
// for at most this long after it, which is the one place an app read is
// ordered against revocation only up to its first byte.
var appAttachmentRouteDeadline = 5 * time.Minute

func (s *Server) appUploadAttachment(w http.ResponseWriter, r *http.Request) {
	ac := appContextFrom(r)
	// Size first (DOC-3371 §4 step 1), before any lookup: a declared length
	// is required, so a chunked or unsized body is refused.
	if r.ContentLength < 0 || len(r.TransferEncoding) > 0 {
		writeError(w, http.StatusLengthRequired, "length_required", "Content-Length is required")
		return
	}
	if r.ContentLength == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "A non-empty body is required")
		return
	}
	if r.ContentLength > appstore.AppAttachmentMaxFileBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "The file exceeds the 25 MiB upload limit")
		return
	}
	// The write gate is an app field edit's: the item visible under the
	// ceiling, a companion the app may write, and the subject's edit right.
	// That the item was created by this install is the FencedTx's check
	// (requireCreatedHere), at admission and again at the row.
	item, _, ok := s.appVisibleItem(w, r)
	if !ok {
		return
	}
	if !s.requireEditPermission(w, r, ac.WorkspaceID, item.ID, item.CollectionID) {
		return
	}
	as, err := s.appStore()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), appAttachmentRouteDeadline)
	defer cancel()
	itemID, collectionID := item.ID, item.CollectionID
	pending, err := as.StageUpload(ctx, ac.appFenceSpec(), item.ID, appstore.AppUpload{
		Body: r.Body, DeclaredSize: r.ContentLength, Filename: r.URL.Query().Get("filename"),
	}, appActor(ac))
	if err != nil {
		writeAppStoreError(w, err)
		return
	}
	defer pending.Close()
	// The subject's part of the write gate again, once the body is in (codex
	// U6c r3): a body can take minutes. A full re-admission (the credential,
	// the install, the membership and its role as they stand now, and the item
	// re-check appVisibleItem registered, replayed under the fresh context),
	// then the edit right, which the unchanged role keeps current. The fence
	// checks the companion and the creator in the row's own transaction; what
	// remains is the gap between this check and that transaction, the one an
	// item write has.
	if !s.appReadmitBeforeWrite(w, r, itemID, collectionID) {
		return
	}
	att, err := pending.Insert(ctx)
	if err != nil {
		writeAppStoreError(w, err)
		return
	}
	// The DTO is the row the upload's own transaction inserted. An upload
	// writes no item row and, like the human upload, publishes no event.
	writeAppJSON(w, http.StatusCreated, att)
}

// appVisibleAttachment authorizes one attachment for a read: the app read
// rule (a live row bound to a live item in a companion, the FencedTx's
// check), the bound item visible to the subject, and re-checks of both
// registered for the response's re-admission. Every refusal is the same
// attachment 404.
func (s *Server) appVisibleAttachment(w http.ResponseWriter, r *http.Request, att *appstore.AppAttachment) bool {
	ac := appContextFrom(r)
	it, err := s.store.GetItem(att.ItemID)
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	if it == nil || it.WorkspaceID != ac.WorkspaceID || it.DeletedAt != nil {
		writeAppNotFound(w, "Attachment")
		return false
	}
	visible, err := s.checkItemVisible(ac.WorkspaceID, it, currentUser(r), workspaceRole(r), isBearerAuth(r))
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	if !visible {
		writeAppNotFound(w, "Attachment")
		return false
	}
	id, itemID, coll := att.ID, it.ID, it.CollectionID
	appAddRecheck(r, func(r2 *http.Request) error { return s.appRecheckItem(r2, itemID, coll) })
	appAddRecheck(r, func(r2 *http.Request) error { return s.appRecheckAttachment(r2, id, itemID) })
	return true
}

// appRecheckAttachment replays the read rule under the re-admitted context:
// the attachment must still be visible to the app and still bound to the
// same item.
func (s *Server) appRecheckAttachment(r *http.Request, id, itemID string) error {
	as, err := s.appStore()
	if err != nil {
		return appFault(err)
	}
	att, err := as.GetAttachment(r.Context(), appContextFrom(r).appFenceSpec(), id)
	switch {
	case errors.Is(err, store.ErrAppAttachmentNotFound), errors.Is(err, store.ErrFenceStale),
		errors.Is(err, store.ErrNotCompanion), errors.Is(err, store.ErrFenceClosed):
		return errAppRecheck
	case err != nil:
		return appFault(err)
	}
	if att.ItemID != itemID {
		return errors.New("the attachment moved")
	}
	return nil
}

func (s *Server) appGetAttachment(w http.ResponseWriter, r *http.Request) {
	ac := appContextFrom(r)
	as, err := s.appStore()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	att, err := as.GetAttachment(r.Context(), ac.appFenceSpec(), chi.URLParam(r, "attachmentID"))
	if err != nil {
		writeAppStoreError(w, err)
		return
	}
	if !s.appVisibleAttachment(w, r, att) {
		return
	}
	writeAppJSON(w, http.StatusOK, att)
}

// appDownloadAttachment streams an attachment's bytes, or a variant's. It is
// NOT buffered (lead ruling, U6c): it authorizes, opens the blob, and then
// re-admits the whole request (appRevalidate, every registered re-check)
// immediately before the first byte. A revocation committed before that
// point sends nothing. One committed after it can still see the rest of
// this stream, bounded by appAttachmentRouteDeadline: the one place an app
// read is ordered against revocation only up to its first byte, stated in
// DOC-3371.
func (s *Server) appDownloadAttachment(w http.ResponseWriter, r *http.Request) {
	if s.appStreamDone != nil {
		defer s.appStreamDone()
	}
	ac := appContextFrom(r)
	as, err := s.appStore()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	deadline := time.Now().Add(appAttachmentRouteDeadline)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	id := chi.URLParam(r, "attachmentID")
	// An unknown variant name is a malformed request, as on the regular
	// download, not a silent fallback to the original (codex U6c r2).
	variant := r.URL.Query().Get("variant")
	if variant != "" && !isKnownVariant(variant) {
		writeError(w, http.StatusBadRequest, "bad_variant", "Unknown variant")
		return
	}
	// Authorize BEFORE touching storage (codex U6c r1 P2): a storage error
	// (a missing blob, a missing variant blob) on an attachment the subject
	// may not see would answer differently from an unknown id.
	meta, err := as.GetAttachment(ctx, ac.appFenceSpec(), id)
	if err != nil {
		writeAppStoreError(w, err)
		return
	}
	if !s.appVisibleAttachment(w, r, meta) {
		return
	}
	att, f, err := as.OpenAttachment(ctx, ac.appFenceSpec(), id, variant)
	if err != nil {
		writeAppStoreError(w, err)
		return
	}
	defer func() { _ = f.Close() }()
	// The open re-read the row; what it opened must be the same attachment's
	// (or its variant's) on the item just authorized.
	if att.ItemID != meta.ItemID {
		writeAppNotFound(w, "Attachment")
		return
	}
	// A variant is its own row: the gate re-checks the row whose bytes are
	// served, not only the original that was authorized (codex U6c r6).
	if att.ID != meta.ID {
		served, item := att.ID, meta.ItemID
		appAddRecheck(r, func(r2 *http.Request) error { return s.appRecheckAttachment(r2, served, item) })
	}
	if s.appBeforeFirstByte != nil {
		s.appBeforeFirstByte()
	}
	// THE GATE: nothing has been written yet.
	if err := s.appRevalidate(r); err != nil {
		writeAppRevalidateError(w, err)
		return
	}
	// The deadline also bounds the transport (codex U6c r1 P2): a context
	// stops only the next file read, so a client that stops reading would
	// otherwise hold a blocked write open past it and receive more bytes
	// later. A ResponseWriter without deadline support (a test recorder) is
	// served with the read-side bound alone.
	if err := http.NewResponseController(w).SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writeInternalError(w, err)
		return
	}
	contentType, disposition := attachmentServeType(att.MimeType)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition",
		contentDisposition(disposition, sanitizeHeaderFilename(attachments.ServedFilename(att.Filename))))
	http.ServeContent(w, r, att.Filename, att.CreatedAt, ctxReadSeeker{ctx: ctx, f: f})
}

// ctxReadSeeker stops a stream when its context ends: the route deadline, or
// the client going away. ServeContent then ends the response.
type ctxReadSeeker struct {
	ctx context.Context
	f   *os.File
}

func (c ctxReadSeeker) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.f.Read(p)
}

func (c ctxReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return c.f.Seek(offset, whence)
}

// writeAppAttachmentError maps the attachment-specific store errors; the
// rest go to writeAppStoreError.
func writeAppAttachmentError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrAppAttachmentNotFound):
		writeAppNotFound(w, "Attachment")
	case errors.Is(err, store.ErrAppAttachmentLimit):
		// No totals (DOC-3371 §4).
		writeError(w, http.StatusForbidden, "attachment_limit_reached", "The app has reached its attachment limit")
	case errors.Is(err, appstore.ErrAppSizeMismatch), errors.Is(err, io.ErrUnexpectedEOF):
		// A body that ends before its declared length: over a connection
		// the request body reports io.ErrUnexpectedEOF (codex U6c r2), the
		// same client mistake as a short body.
		writeError(w, http.StatusBadRequest, "size_mismatch", "The body does not match Content-Length")
	case errors.Is(err, appstore.ErrAppNoAttachmentStore):
		writeError(w, http.StatusServiceUnavailable, "attachments_unavailable", "Attachment storage is not available")
	case errors.Is(err, attachments.ErrNotFound):
		writeError(w, http.StatusNotFound, "blob_missing", "The attachment's file is missing")
	default:
		return false
	}
	return true
}
