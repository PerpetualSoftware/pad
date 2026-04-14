package server

import (
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/xarmian/pad/internal/models"
)

// handleStarItem stars an item for the authenticated user (idempotent).
// POST /api/v1/workspaces/{slug}/items/{itemSlug}/star
func (s *Server) handleStarItem(w http.ResponseWriter, r *http.Request) {
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
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
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

	if err := s.store.StarItem(userID, item.ID); err != nil {
		writeInternalError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleUnstarItem removes a star from an item for the authenticated user.
// DELETE /api/v1/workspaces/{slug}/items/{itemSlug}/star
func (s *Server) handleUnstarItem(w http.ResponseWriter, r *http.Request) {
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
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
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

	if err := s.store.UnstarItem(userID, item.ID); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "Item is not starred")
			return
		}
		writeInternalError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleListStarredItems returns all starred items for the authenticated user in a workspace.
// GET /api/v1/workspaces/{slug}/starred
// Query params:
//   - include_terminal=true — include items in terminal statuses (default: false)
func (s *Server) handleListStarredItems(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}

	includeTerminal := r.URL.Query().Get("include_terminal") == "true"

	items, err := s.store.ListStarredItems(userID, workspaceID, includeTerminal)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Hydrate items with parent links and computed refs
	visibleIDs, _ := s.visibleCollectionIDs(r, workspaceID)
	s.enrichItemsWithParent(workspaceID, items, visibleIDs)

	if len(items) == 0 {
		items = []models.Item{}
	}

	writeJSON(w, http.StatusOK, items)
}

// handleGetItemStarStatus returns whether the authenticated user has starred a specific item.
// GET /api/v1/workspaces/{slug}/items/{itemSlug}/star
func (s *Server) handleGetItemStarStatus(w http.ResponseWriter, r *http.Request) {
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
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
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

	starred, err := s.store.IsItemStarred(userID, item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"starred": starred})
}
