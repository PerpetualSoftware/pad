package server

import (
	"errors"
	"io"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Needs-you "not mine" (TASK-4643): the caller marks an item as not theirs,
// hiding it from their own needs-you inbox until it comes back to them (an
// open mention, a waiting status, a move into an ask collection, an
// assignment to them, a reminder of theirs coming due). Per user, so any
// person who can see the item may mark it, viewers and guests included, with
// no edit check. The same front half as the snooze: a read-scoped token is
// refused by tokenScopeAllows, and an item the caller cannot see is 404.

type needsNotMineRequest struct {
	Marker *models.NeedsSnoozeMarker `json:"marker,omitempty"`
}

// needsSourceFromRequest is how the request authenticated, never what its
// body says: "web" for a cookie session, "cli" for a token (the MCP
// dispatcher's in-process requests included).
func needsSourceFromRequest(r *http.Request) string {
	_, source := actorFromRequest(r)
	if source == models.NeedsSourceCLI {
		return models.NeedsSourceCLI
	}
	return models.NeedsSourceWeb
}

// handleNotMineNeedsItem marks an item not mine for the caller, replacing any
// earlier mark and any snooze of theirs on it.
// PUT /api/v1/workspaces/{slug}/items/{itemSlug}/needs/not-mine
// Body (optional): {"marker"?: {...}}
func (s *Server) handleNotMineNeedsItem(w http.ResponseWriter, r *http.Request) {
	workspaceID, item, userID, ok := s.resolveNeedsSnoozeItem(w, r)
	if !ok {
		return
	}
	// The body is optional: none means "the item as it is now".
	var req needsNotMineRequest
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body")
		return
	}
	nm, err := s.store.MarkNeedsNotMine(userID, workspaceID, item.ID, needsSourceFromRequest(r), req.Marker)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ref":      item.Ref,
		"not_mine": nm,
	})
}

// handleUnNotMineNeedsItem removes the caller's "not mine", so the item
// shows in their inbox again. Idempotent: no mark is not an error.
// DELETE /api/v1/workspaces/{slug}/items/{itemSlug}/needs/not-mine
func (s *Server) handleUnNotMineNeedsItem(w http.ResponseWriter, r *http.Request) {
	_, item, userID, ok := s.resolveNeedsSnoozeItem(w, r)
	if !ok {
		return
	}
	if _, err := s.store.UnmarkNeedsNotMine(userID, item.ID); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ref":      item.Ref,
		"not_mine": nil,
	})
}
