package server

import (
	"errors"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/go-chi/chi/v5"
)

// handleStampCollabWatermark is POST /workspaces/{ws}/items/{itemSlug}/collab-watermark
// (BUG-3124 unit B): a caught-up browser tab whose document still renders to
// the stored body tells the server so, and the server advances the flush
// watermark without writing content. See store.StampContentWatermarkIfCaughtUp
// for the proof and for why this is not a PATCH.
//
// Edit permission, like the collab-snapshot flush it stands in for: moving the
// watermark makes op-log rows eligible for GC, which is a write decision even
// though the proof makes it safe.
//
// A stamp that does not apply is 200 {"advanced": false}, not an error: a
// cursor behind MAX or a body that changed means the tab is not caught up, and
// the tab's next flush or stamp is the retry. Only malformed input is a 400.
func (s *Server) handleStampCollabWatermark(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItem(workspaceID, itemSlug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if item == nil {
		s.writeItemResolveError(w, r, workspaceID, itemSlug)
		return
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return
	}
	if !s.requireEditPermission(w, r, workspaceID, item.ID, item.CollectionID) {
		return
	}

	var input struct {
		OpLogCursor   int64  `json:"op_log_cursor"`
		ContentSHA256 string `json:"content_sha256"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	var advanced bool
	stamp := func() error {
		var serr error
		advanced, serr = s.store.StampContentWatermarkIfCaughtUp(item.ID, input.OpLogCursor, input.ContentSHA256)
		return serr
	}
	// Under the per-item collab lock when collab is on, for the same reason the
	// snapshot PATCH takes it: a prune must not land between the read and the
	// conditional write. The UPDATE's predicate already refuses a moved MAX or
	// body; the lock keeps this door ordered with the ones that move them.
	if s.collab != nil {
		err = s.collab.UnderItemLock(item.ID, stamp)
	} else {
		err = stamp()
	}
	if err != nil {
		if errors.Is(err, store.ErrWatermarkStampBadInput) {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"advanced": advanced})
}
