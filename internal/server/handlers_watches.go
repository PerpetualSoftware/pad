package server

import (
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// watchCreateRequest is the body of POST .../items/{itemSlug}/watch.
// Predicate is optional — DOC-2479's `--until field=value` grammar
// (e.g. "status=done"). Empty means an unconditional watch: any
// status-change/assignment/comment on the item notifies the caller.
type watchCreateRequest struct {
	Predicate string `json:"predicate,omitempty"`
}

// handleCreateWatch creates (or replaces the predicate on) a durable
// watch for the authenticated user on this item (TASK-2533). Re-running
// this on an already-watched item is an upsert — see
// Store.CreateWatch's doc comment.
// POST /api/v1/workspaces/{slug}/items/{itemSlug}/watch
func (s *Server) handleCreateWatch(w http.ResponseWriter, r *http.Request) {
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

	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}

	var input watchCreateRequest
	// A watch with no body is valid (unconditional watch) — only reject
	// a body that's present but malformed JSON, mirroring the tolerant-
	// empty-body pattern other optional-body endpoints use.
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	if input.Predicate != "" {
		if _, _, ok := parseWatchPredicate(input.Predicate); !ok {
			writeError(w, http.StatusBadRequest, "bad_request",
				`predicate must be in "field=value" form, e.g. "status=done"`)
			return
		}
	}

	watch, err := s.store.CreateWatch(workspaceID, userID, item.ID, input.Predicate)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	watch.ItemRef = item.Ref
	watch.ItemTitle = item.Title
	watch.ItemSlug = item.Slug

	writeJSON(w, http.StatusOK, watch)
}

// handleDeleteWatch removes the authenticated user's watch on an item.
// DELETE /api/v1/workspaces/{slug}/items/{itemSlug}/watch
func (s *Server) handleDeleteWatch(w http.ResponseWriter, r *http.Request) {
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

	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}

	if err := s.store.DeleteWatch(userID, item.ID); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "No watch on this item")
			return
		}
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ref":     item.Ref,
		"watched": false,
	})
}

// handleListWatches lists every watch the authenticated user holds,
// across all workspaces they belong to (TASK-2533 — a watch is personal,
// not workspace-scoped; see Store.ListWatchesForUser's doc comment).
// GET /api/v1/watches
func (s *Server) handleListWatches(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}

	watches, err := s.store.ListWatchesForUser(userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// TASK-2533 codex round 1 finding 1: re-check current access before
	// listing — a watch row survives a revoked workspace membership or
	// grant, so without this a caller could see item/workspace metadata
	// for access they no longer have. See filterWatchesByCurrentAccess.
	watches = s.filterWatchesByCurrentAccess(userID, watches)
	if watches == nil {
		watches = []models.Watch{}
	}

	writeJSON(w, http.StatusOK, watches)
}
