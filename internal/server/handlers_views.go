package server

import (
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/xarmian/pad/internal/models"
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

	views, err := s.store.ListViews(workspaceID, coll.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if views == nil {
		views = []models.View{}
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

	var input models.ViewCreate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Name is required")
		return
	}

	input.CollectionID = &coll.ID

	view, err := s.store.CreateView(workspaceID, input)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, view)
}

// handleUpdateView modifies an existing saved view.
func (s *Server) handleUpdateView(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
		return
	}
	_, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	viewID := chi.URLParam(r, "viewID")

	var input models.ViewUpdate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
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

	writeJSON(w, http.StatusOK, view)
}

// handleDeleteView removes a saved view.
func (s *Server) handleDeleteView(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
		return
	}
	_, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	viewID := chi.URLParam(r, "viewID")

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
