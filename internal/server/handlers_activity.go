package server

import (
	"net/http"
	"strconv"

	"github.com/xarmian/pad/internal/models"
)

func (s *Server) handleListWorkspaceActivity(w http.ResponseWriter, r *http.Request) {
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
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if activities == nil {
		activities = []models.Activity{}
	}

	// Enrich activities with item titles and collection info
	s.enrichActivities(activities)

	writeJSON(w, http.StatusOK, activities)
}

func (s *Server) handleListDocumentActivity(w http.ResponseWriter, r *http.Request) {
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
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
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
