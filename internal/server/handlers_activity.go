package server

import (
	"net/http"
	"strconv"

	"github.com/xarmian/pad/internal/models"
)

func (s *Server) handleListWorkspaceActivity(w http.ResponseWriter, r *http.Request) {
	// Guests should not see workspace-level activity, which includes
	// audit events (member invites, role changes, etc.) with operational metadata.
	if !requireMinRole(w, r, "viewer") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	params := models.ActivityListParams{
		Action: r.URL.Query().Get("action"),
		Actor:  r.URL.Query().Get("actor"),
		Source: r.URL.Query().Get("source"),
	}

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil {
			params.Limit = l
		}
	}
	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		if o, err := strconv.Atoi(offsetStr); err == nil {
			params.Offset = o
		}
	}

	activities, err := s.store.ListWorkspaceActivity(workspaceID, params)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if activities == nil {
		activities = []models.Activity{}
	}

	// Enrich activities with item titles and collection info
	s.enrichActivities(activities)

	// Filter by collection visibility and item-level grants
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	fullCollIDs, grantedItemIDs, grantErr := s.guestResourceFilter(r, workspaceID)
	if grantErr != nil {
		writeInternalError(w, grantErr)
		return
	}
	grantedItemSet := make(map[string]bool, len(grantedItemIDs))
	for _, id := range grantedItemIDs {
		grantedItemSet[id] = true
	}
	fullCollSet := make(map[string]bool, len(fullCollIDs))
	for _, id := range fullCollIDs {
		fullCollSet[id] = true
	}
	if visibleIDs != nil {
		// Build slug lookup from visible collection IDs
		visibleSlugs := make(map[string]bool)
		// Also build collSlug→collID map for item-level checks
		slugToID := make(map[string]string)
		for _, id := range visibleIDs {
			coll, _ := s.store.GetCollection(id)
			if coll != nil {
				visibleSlugs[coll.Slug] = true
				slugToID[coll.Slug] = id
			}
		}
		filtered := make([]models.Activity, 0, len(activities))
		for _, a := range activities {
			if a.CollectionSlug != "" && visibleSlugs[a.CollectionSlug] {
				// For guests with item-level grants, check the specific item
				if len(grantedItemIDs) > 0 && !fullCollSet[slugToID[a.CollectionSlug]] && a.DocumentID != "" {
					if !grantedItemSet[a.DocumentID] {
						continue
					}
				}
				filtered = append(filtered, a)
			} else if a.CollectionSlug == "" && a.ItemSlug == "" {
				// Workspace-level activity (no item) — include
				filtered = append(filtered, a)
			}
			// Drop: empty collection slug with an item (unresolved hidden item),
			// or known collection slug that's not visible.
		}
		activities = filtered
	}

	writeJSON(w, http.StatusOK, activities)
}

func (s *Server) handleListDocumentActivity(w http.ResponseWriter, r *http.Request) {
	// Legacy documents are not covered by the grants model — block guests.
	if !requireMinRole(w, r, "viewer") {
		return
	}
	_, doc, ok := s.getWorkspaceDocument(w, r)
	if !ok {
		return
	}

	params := models.ActivityListParams{
		Action: r.URL.Query().Get("action"),
		Actor:  r.URL.Query().Get("actor"),
	}

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil {
			params.Limit = l
		}
	}
	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		if o, err := strconv.Atoi(offsetStr); err == nil {
			params.Offset = o
		}
	}

	activities, err := s.store.ListDocumentActivity(doc.ID, params)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if activities == nil {
		activities = []models.Activity{}
	}

	// Enrich activities with item titles and collection info
	s.enrichActivities(activities)

	writeJSON(w, http.StatusOK, activities)
}

// enrichActivities populates ItemTitle, ItemSlug, and CollectionSlug
// on each activity by looking up the referenced item.
func (s *Server) enrichActivities(activities []models.Activity) {
	for i := range activities {
		if activities[i].DocumentID == "" {
			continue
		}
		item, err := s.store.GetItem(activities[i].DocumentID)
		if err != nil || item == nil {
			continue
		}
		activities[i].ItemTitle = item.Title
		activities[i].ItemSlug = item.Slug
		activities[i].CollectionSlug = item.CollectionSlug
	}
}
