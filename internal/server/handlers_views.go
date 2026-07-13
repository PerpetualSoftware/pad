package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// handleListViews returns all saved views for a collection.
func (s *Server) handleListViews(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	collSlug := chi.URLParam(r, "collSlug")
	coll, err := s.store.GetCollectionBySlug(workspaceID, collSlug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if coll == nil {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}

	// Check collection visibility
	visibleIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
	if visErr != nil {
		writeInternalError(w, visErr)
		return
	}
	if !isCollectionVisible(coll.ID, visibleIDs) {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}

	views, err := s.store.ListViews(workspaceID, coll.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if views == nil {
		views = []models.View{}
	}
	if visibleIDs != nil {
		stripReservedUnparentedFromViews(views)
	}

	writeJSON(w, http.StatusOK, views)
}

// handleCreateView creates a new saved view for a collection.
func (s *Server) handleCreateView(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	collSlug := chi.URLParam(r, "collSlug")
	coll, err := s.store.GetCollectionBySlug(workspaceID, collSlug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if coll == nil {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}

	// Check collection visibility and edit permission (grant-aware)
	visibleIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
	if visErr != nil {
		writeInternalError(w, visErr)
		return
	}
	if !isCollectionVisible(coll.ID, visibleIDs) {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}
	if !s.requireEditPermission(w, r, workspaceID, "", coll.ID) {
		return
	}

	var input models.ViewCreate
	if err := decodeJSON(r, &input); err != nil {
		// IDEA-1488: surface the domain-level error from
		// ViewCreate.UnmarshalJSON without the "invalid JSON: ..."
		// wrapper from decodeJSON, so callers see a clean message
		// naming the field (mirrors handlers_items.go:641 precedent).
		if errors.Is(err, models.ErrInvalidConfigType) {
			writeError(w, http.StatusBadRequest, "bad_request", models.ErrInvalidConfigType.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Name is required")
		return
	}
	if visibleIDs != nil {
		input.Config = stripReservedUnparentedViewFilter(input.Config)
	}

	input.CollectionID = &coll.ID

	view, err := s.store.CreateView(workspaceID, input)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, view)
}

// requireViewVisible looks up a view by ID, verifies it belongs to the
// workspace, and checks that its collection is visible. Returns the view
// or writes an error and returns nil.
func (s *Server) requireViewVisible(w http.ResponseWriter, r *http.Request, workspaceID, viewID string) *models.View {
	view, err := s.store.GetView(viewID)
	if err != nil || view == nil || view.WorkspaceID != workspaceID {
		writeError(w, http.StatusNotFound, "not_found", "View not found")
		return nil
	}
	if view.CollectionID != nil {
		visibleIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
		if visErr != nil {
			writeInternalError(w, visErr)
			return nil
		}
		if !isCollectionVisible(*view.CollectionID, visibleIDs) {
			writeError(w, http.StatusNotFound, "not_found", "View not found")
			return nil
		}
	}
	return view
}

// requireViewEditable is like requireViewVisible but also checks edit permission
// on the view's collection (grant-aware for guests/restricted members).
func (s *Server) requireViewEditable(w http.ResponseWriter, r *http.Request, workspaceID, viewID string) *models.View {
	view := s.requireViewVisible(w, r, workspaceID, viewID)
	if view == nil {
		return nil
	}
	if view.CollectionID != nil {
		if !s.requireEditPermission(w, r, workspaceID, "", *view.CollectionID) {
			return nil
		}
	}
	return view
}

// handleUpdateView modifies an existing saved view.
func (s *Server) handleUpdateView(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	viewID := chi.URLParam(r, "viewID")
	if s.requireViewEditable(w, r, workspaceID, viewID) == nil {
		return
	}

	var input models.ViewUpdate
	if err := decodeJSON(r, &input); err != nil {
		// IDEA-1488: surface the domain-level error from
		// ViewUpdate.UnmarshalJSON without the "invalid JSON: ..."
		// wrapper from decodeJSON, so callers see a clean message
		// naming the field (mirrors handlers_items.go:641 precedent).
		if errors.Is(err, models.ErrInvalidConfigType) {
			writeError(w, http.StatusBadRequest, "bad_request", models.ErrInvalidConfigType.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	visibleIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
	if visErr != nil {
		writeInternalError(w, visErr)
		return
	}
	if visibleIDs != nil && input.Config != nil {
		config := stripReservedUnparentedViewFilter(*input.Config)
		input.Config = &config
	}

	view, err := s.store.UpdateView(viewID, input)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "View not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	if visibleIDs != nil {
		view.Config = stripReservedUnparentedViewFilter(view.Config)
	}

	writeJSON(w, http.StatusOK, view)
}

// handleDeleteView removes a saved view.
func (s *Server) handleDeleteView(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	viewID := chi.URLParam(r, "viewID")
	if s.requireViewEditable(w, r, workspaceID, viewID) == nil {
		return
	}

	if err := s.store.DeleteView(viewID); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "View not found")
			return
		}
		writeInternalError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// reservedUnparentedViewField is the saved-view pseudo-field the web phase
// uses to persist the structural filter. It is not a collection-schema field.
// Restricted and public consumers must never receive it because evaluating it
// would expose whether hidden structural relationships exist.
const reservedUnparentedViewField = "$unparented"

func stripReservedUnparentedFromViews(views []models.View) {
	for i := range views {
		views[i].Config = stripReservedUnparentedViewFilter(views[i].Config)
	}
}

func stripReservedUnparentedViewFilter(config string) string {
	if config == "" {
		return config
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(config), &doc); err != nil {
		return config
	}
	rawFiltersValue, hasFilters := doc["filters"]
	if !hasFilters {
		return config
	}
	rawFilters, ok := rawFiltersValue.([]any)
	if !ok {
		// View config is intentionally flexible, but the filter evaluator only
		// accepts arrays. Drop malformed shapes rather than returning their raw
		// contents to restricted/public callers, where a nested reserved field
		// could otherwise bypass the element-wise sanitizer below.
		delete(doc, "filters")
		b, err := json.Marshal(doc)
		if err != nil {
			return "{}"
		}
		return string(b)
	}
	filtered := make([]any, 0, len(rawFilters))
	changed := false
	for _, raw := range rawFilters {
		filter, ok := raw.(map[string]any)
		if ok && filter["field"] == reservedUnparentedViewField {
			changed = true
			continue
		}
		filtered = append(filtered, raw)
	}
	if !changed {
		return config
	}
	doc["filters"] = filtered
	b, err := json.Marshal(doc)
	if err != nil {
		return config
	}
	return string(b)
}
