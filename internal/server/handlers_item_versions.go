package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// handleListItemVersions returns all versions for an item with diffs resolved.
func (s *Server) handleListItemVersions(w http.ResponseWriter, r *http.Request) {
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

	versions, err := s.store.ListItemVersionsResolved(item.ID, item.Content)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if versions == nil {
		versions = []models.Version{}
	}

	writeJSON(w, http.StatusOK, versions)
}

// handleRestoreItemVersion restores an item's content from a specific version.
func (s *Server) handleRestoreItemVersion(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	versionID := chi.URLParam(r, "versionID")

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
	// Check edit permission (grant-aware for guests)
	if !s.requireEditPermission(w, r, workspaceID, item.ID, item.CollectionID) {
		return
	}

	// Get all resolved versions to find the target
	versions, err := s.store.ListItemVersionsResolved(item.ID, item.Content)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	var targetVersion *models.Version
	for _, v := range versions {
		if v.ID == versionID {
			targetVersion = &v
			break
		}
	}
	if targetVersion == nil {
		writeError(w, http.StatusNotFound, "not_found", "Version not found")
		return
	}

	// Update item content to the version's content
	content := targetVersion.Content
	summary := "Restored from version " + targetVersion.CreatedAt.Format("Jan 2, 2006 3:04 PM")
	input := models.ItemUpdate{
		Content:        &content,
		ChangeSummary:  summary,
		LastModifiedBy: "user",
		Source:         "web",
	}

	updated, err := s.store.UpdateItem(item.ID, input)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Emit event
	if s.events != nil {
		s.events.Publish(events.Event{
			Type:        "item_updated",
			WorkspaceID: workspaceID,
			Collection:  item.CollectionSlug,
			ItemID:      item.ID,
			Title:       item.Title,
		})
	}

	writeJSON(w, http.StatusOK, updated)
}
