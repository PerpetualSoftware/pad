package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/go-chi/chi/v5"
)

// Needs-you snoozes (TASK-3918, IDEA-3906 half A unit A2): the caller hides
// an item from their own needs-you inbox. Per user, so any person who can
// see the item may snooze it: viewers and guests included, with no edit
// check. A read-scoped token is refused by tokenScopeAllows (PUT and DELETE
// are writes), and an item the caller cannot see is 404.

type needsSnoozeRequest struct {
	Kind       string                    `json:"kind"`
	DeferUntil string                    `json:"defer_until,omitempty"`
	Marker     *models.NeedsSnoozeMarker `json:"marker,omitempty"`
}

// resolveNeedsSnoozeItem is the shared front half: the item, visible to the
// caller, and the caller's user id. It has written the response when ok is
// false.
func (s *Server) resolveNeedsSnoozeItem(w http.ResponseWriter, r *http.Request) (workspaceID string, item *models.Item, userID string, ok bool) {
	workspaceID, ok = s.getWorkspaceID(w, r)
	if !ok {
		return "", nil, "", false
	}
	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItem(workspaceID, itemSlug)
	if err != nil {
		writeInternalError(w, err)
		return "", nil, "", false
	}
	if item == nil {
		s.writeItemResolveError(w, r, workspaceID, itemSlug)
		return "", nil, "", false
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return "", nil, "", false
	}
	userID = currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return "", nil, "", false
	}
	return workspaceID, item, userID, true
}

// handleSnoozeNeedsItem dismisses or defers an item for the caller,
// replacing any earlier snooze.
// PUT /api/v1/workspaces/{slug}/items/{itemSlug}/needs/snooze
// Body: {"kind": "dismiss"|"defer", "defer_until"?: RFC3339, "marker"?: {...}}
func (s *Server) handleSnoozeNeedsItem(w http.ResponseWriter, r *http.Request) {
	workspaceID, item, userID, ok := s.resolveNeedsSnoozeItem(w, r)
	if !ok {
		return
	}
	var req needsSnoozeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body")
		return
	}
	var until time.Time
	if req.Kind == models.NeedsSnoozeDefer {
		t, err := time.Parse(time.RFC3339, req.DeferUntil)
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_error", "defer_until must be an RFC3339 time")
			return
		}
		until = t
	}
	sn, err := s.store.SnoozeNeedsItem(userID, workspaceID, item.ID, req.Kind, until, req.Marker)
	if errors.Is(err, store.ErrInvalidNeedsSnooze) {
		writeError(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ref":    item.Ref,
		"snooze": sn,
	})
}

// handleUnsnoozeNeedsItem removes the caller's snooze, so the item shows in
// their inbox again. Idempotent: no snooze is not an error.
// DELETE /api/v1/workspaces/{slug}/items/{itemSlug}/needs/snooze
func (s *Server) handleUnsnoozeNeedsItem(w http.ResponseWriter, r *http.Request) {
	_, item, userID, ok := s.resolveNeedsSnoozeItem(w, r)
	if !ok {
		return
	}
	if _, err := s.store.UnsnoozeNeedsItem(userID, item.ID); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ref":    item.Ref,
		"snooze": nil,
	})
}
