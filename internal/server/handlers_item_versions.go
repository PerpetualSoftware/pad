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
	item, err := s.store.ResolveItemIncludeDeleted(workspaceID, itemSlug)
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

// handleGetItemVersion returns a single version with its diff resolved to full
// content. The paginated timeline serves raw reverse-patch text (it can't resolve
// a partial window), so the timeline card calls this to reconstruct real content
// when a diff version is expanded — see BUG-1612.
func (s *Server) handleGetItemVersion(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	versionID := chi.URLParam(r, "versionID")
	item, err := s.store.ResolveItemIncludeDeleted(workspaceID, itemSlug)
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

	version, err := s.store.GetItemVersionResolved(item.ID, versionID, item.Content)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if version == nil {
		writeError(w, http.StatusNotFound, "not_found", "Version not found")
		return
	}

	writeJSON(w, http.StatusOK, version)
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
		s.writeItemResolveError(w, r, workspaceID, itemSlug)
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

	// Emit event. Carry the post-update `seq` so SSE consumers
	// (PLAN-1343 / TASK-1358 — localIndex.classifySSEEvent) can
	// detect contiguity vs. gap rather than blindly falling back
	// to a generic /items-changes refetch.
	if s.events != nil {
		s.events.Publish(events.Event{
			Type:        "item_updated",
			WorkspaceID: workspaceID,
			Collection:  item.CollectionSlug,
			ItemID:      item.ID,
			Title:       item.Title,
			Seq:         updated.Seq,
		})
	}

	writeJSON(w, http.StatusOK, updated)
}
