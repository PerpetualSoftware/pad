package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/go-chi/chi/v5"
)

// handleStampCollabWatermark is POST /workspaces/{ws}/items/{itemSlug}/collab-watermark
// (BUG-3124 unit B): a caught-up browser tab whose document still renders to
// the stored body (or to the editor's own serialization of it, BUG-3197) tells
// the server so, and the server advances the flush
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

	var res store.WatermarkStampResult
	stamp := func() error {
		var serr error
		res, serr = s.store.StampContentWatermarkIfCaughtUpCounted(item.ID, input.OpLogCursor, input.ContentSHA256)
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
	if res.Advanced {
		s.recordWatermarkStamp(r, item.ID, input.OpLogCursor, res)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"advanced": res.Advanced})
}

// recordWatermarkStamp measures what an advancing stamp covered (TASK-3541
// step 0): nothing about the answer changes. A stamp over content-bearing rows
// is the server accepting, unverified, a tab's claim that rows it cannot read
// already render to items.content; a buggy, outdated or hostile tab can make
// unstored edits read as flushed that way. It is logged at WARN with enough to
// find the tab, so the rate can be judged before the materializer check
// (TASK-3541 steps 1+) is built.
func (s *Server) recordWatermarkStamp(r *http.Request, itemID string, cursor int64, res store.WatermarkStampResult) {
	covers := "view_only"
	if res.CoveredContentRows > 0 {
		covers = "content"
	}
	if s.metrics != nil {
		s.metrics.CollabWatermarkStampsTotal.WithLabelValues(covers).Inc()
	}
	if res.CoveredContentRows == 0 {
		return
	}
	slog.Warn("collab watermark stamp covered content-bearing rows the server did not verify",
		"item_id", itemID,
		"from", res.PrevWatermark,
		"to", cursor,
		"content_rows", res.CoveredContentRows,
		"user_id", currentUserID(r),
		"user_agent", r.UserAgent(),
	)
}
