package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/store"
)

type attachRequest struct {
	// Item is the target item's ref, slug or id, resolved like the item routes.
	Item string `json:"item"`
}

type attachResponse struct {
	ID     string `json:"id"`
	ItemID string `json:"item_id"`
}

// handleAttachAttachment binds an UNATTACHED attachment to an item
// (TASK-2247). `pad attachment upload -` leaves item_id NULL, and share
// pages serve only attachments the shared item owns, so an image uploaded
// that way and then embedded renders as a placeholder on a share. This is
// the explicit fix; nothing binds an attachment implicitly.
//
// Rules: the attachment is a live original in this workspace with no item;
// the caller can see and edit the target item; and the caller uploaded the
// attachment or is a workspace owner. An attached row is never reassigned.
//
// Ordering follows handleTransformAttachment: every outcome that would tell
// a caller about a row it cannot read is the shared attachment 404, and a
// 403 or 409 comes only once the caller can already see what it concerns.
func (s *Server) handleAttachAttachment(w http.ResponseWriter, r *http.Request) {
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
	// A variant is addressed through its original, which carries it along.
	if att == nil || att.WorkspaceID != workspaceID || att.DeletedAt != nil || att.ParentID != nil {
		writeAttachmentNotFound(w)
		return
	}

	if att.ItemID != nil {
		// Already attached. Say so only to a caller who can see the owner;
		// to anyone else it is the same 404 as a bad id.
		owner, outcome, err := s.resolveAttachmentParentItem(att, true)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if outcome != attachmentParentOK {
			writeAttachmentNotFound(w)
			return
		}
		visible, err := s.checkItemVisible(workspaceID, owner, currentUser(r), workspaceRole(r), isBearerAuth(r))
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if !visible {
			writeAttachmentNotFound(w)
			return
		}
		writeError(w, http.StatusConflict, "attachment_already_attached",
			"Attachment is already attached to an item; attach only binds an unattached attachment")
		return
	}

	// Unattached rows are readable only by full-access members (PLAN-2382
	// DR-4), so a caller who cannot read one learns nothing past here.
	if !requireRole(r, "viewer") {
		writeAttachmentNotFound(w)
		return
	}
	restricted, err := s.attachmentCallerIsRestricted(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if restricted {
		writeAttachmentNotFound(w)
		return
	}

	var req attachRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	ref := strings.TrimSpace(req.Item)
	if ref == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "item is required")
		return
	}
	item, err := s.store.ResolveItem(workspaceID, ref)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return
	}
	if !s.requireEditPermission(w, r, workspaceID, item.ID, item.CollectionID) {
		return
	}

	user := currentUser(r)
	if user == nil || (att.UploadedBy != user.ID && workspaceRole(r) != "owner") {
		writeError(w, http.StatusForbidden, "forbidden",
			"Only the attachment's uploader or a workspace owner can attach it")
		return
	}

	switch err := s.store.AttachAttachmentToItem(workspaceID, att.ID, item.ID); {
	case errors.Is(err, store.ErrAttachmentParentItemGone):
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	case errors.Is(err, store.ErrAttachmentNotAttachable):
		// Lost a race: attached by another caller, or claimed by the GC,
		// after the read above.
		writeError(w, http.StatusConflict, "attachment_already_attached",
			"Attachment is no longer an unattached attachment in this workspace")
		return
	case err != nil:
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, attachResponse{ID: att.ID, ItemID: item.ID})
}
