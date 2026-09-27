package server

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3244: the read and discard doors for edits a collab schema-version
// rebuild set aside. Those rows hold the item's content_state at
// superseded_set_aside; no tab can replay them, so they leave only by being
// recovered (TASK-3246) or discarded here, or by a content write or restore
// that sends overwrite_pending_edits.

type setAsideResponse struct {
	Ref      string               `json:"ref"`
	SetAside []models.YjsSetAside `json:"set_aside"`
}

// handleListCollabSetAside returns the item's set-aside rows as raw updates
// (update_data is base64 in JSON), in their original op-log order. Anyone who
// can read the item can read them: they are that item's own edits.
func (s *Server) handleListCollabSetAside(w http.ResponseWriter, r *http.Request) {
	_, item, ok := s.resolveSetAsideItem(w, r)
	if !ok {
		return
	}
	rows, err := s.store.ListYjsSetAside(item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, setAsideResponse{Ref: itemRefOrSlug(*item), SetAside: rows})
}

// handleDiscardCollabSetAside deletes the item's set-aside rows, which clears
// the superseded_set_aside state without writing the body. It needs edit
// permission, like the content write that would otherwise discard them.
func (s *Server) handleDiscardCollabSetAside(w http.ResponseWriter, r *http.Request) {
	workspaceID, item, ok := s.resolveSetAsideItem(w, r)
	if !ok {
		return
	}
	if !s.requireEditPermission(w, r, workspaceID, item.ID, item.CollectionID) {
		return
	}
	n, err := s.store.DiscardYjsSetAside(item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if n > 0 {
		// The rows were the only copy of those edits; say who removed them.
		slog.Info("collab: set-aside edits discarded",
			"item_id", item.ID,
			"rows", n,
			"user_id", currentUserID(r),
		)
	}
	writeJSON(w, http.StatusOK, map[string]int64{"discarded": n})
}

func (s *Server) resolveSetAsideItem(w http.ResponseWriter, r *http.Request) (string, *models.Item, bool) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return "", nil, false
	}
	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItem(workspaceID, itemSlug)
	if err != nil {
		writeInternalError(w, err)
		return "", nil, false
	}
	if item == nil {
		s.writeItemResolveError(w, r, workspaceID, itemSlug)
		return "", nil, false
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return "", nil, false
	}
	return workspaceID, item, true
}
