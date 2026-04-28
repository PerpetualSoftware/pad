package server

import (
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
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

	if s.events != nil {
		actor, source := actorFromRequest(r)
		s.events.Publish(events.Event{
			Type:        events.ItemStarred,
			WorkspaceID: workspaceID,
			ItemID:      item.ID,
			Title:       item.Title,
			Collection:  item.CollectionSlug,
			Actor:       actor,
			ActorName:   actorNameFromRequest(r),
			Source:      source,
			UserID:      userID,
		})
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

	if s.events != nil {
		actor, source := actorFromRequest(r)
		s.events.Publish(events.Event{
			Type:        events.ItemUnstarred,
			WorkspaceID: workspaceID,
			ItemID:      item.ID,
			Title:       item.Title,
			Collection:  item.CollectionSlug,
			Actor:       actor,
			ActorName:   actorNameFromRequest(r),
			Source:      source,
			UserID:      userID,
		})
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

	// Apply RBAC visibility filtering (same as handleListItems)
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Build visibility sets for filtering
	visibleSet := make(map[string]bool, len(visibleIDs))
	if visibleIDs != nil {
		for _, id := range visibleIDs {
			visibleSet[id] = true
		}
	}

	// Apply item-level grant filtering for guests/restricted members
	fullCollIDs, grantedItemIDs, grantErr := s.guestResourceFilter(r, workspaceID)
	if grantErr != nil {
		writeInternalError(w, grantErr)
		return
	}

	fullCollSet := make(map[string]bool, len(fullCollIDs))
	for _, id := range fullCollIDs {
		fullCollSet[id] = true
	}
	grantedItemSet := make(map[string]bool, len(grantedItemIDs))
	for _, id := range grantedItemIDs {
		grantedItemSet[id] = true
	}

	// Filter items by visibility
	filtered := make([]models.Item, 0, len(items))
	for _, item := range items {
		if len(grantedItemIDs) > 0 {
			// Guest with item-level grants: allow if collection has full access OR item is explicitly granted
			if fullCollSet[item.CollectionID] || grantedItemSet[item.ID] {
				filtered = append(filtered, item)
			}
		} else if visibleIDs == nil || visibleSet[item.CollectionID] {
			// Regular member: allow if no visibility filter (full access) or collection is visible
			filtered = append(filtered, item)
		}
	}
	items = filtered

	// Hydrate items with parent links and computed refs
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
