package server

import (
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/xarmian/pad/internal/models"
)

// handleGetItemLinks returns all links (both directions) for an item.
func (s *Server) handleGetItemLinks(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.GetItemBySlug(workspaceID, itemSlug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}

	links, err := s.store.GetItemLinks(item.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if links == nil {
		links = []models.ItemLink{}
	}

	writeJSON(w, http.StatusOK, links)
}

// handleCreateItemLink creates a new link between two items.
func (s *Server) handleCreateItemLink(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.GetItemBySlug(workspaceID, itemSlug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}

	var input models.ItemLinkCreate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.TargetID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "target_id is required")
		return
	}

	// Verify target item exists
	target, err := s.store.GetItem(input.TargetID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if target == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Target item not found")
		return
	}

	link, err := s.store.CreateItemLink(workspaceID, input, item.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, link)
}

// handleDeleteItemLink removes a link between items.
func (s *Server) handleDeleteItemLink(w http.ResponseWriter, r *http.Request) {
	_, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	linkID := chi.URLParam(r, "linkID")
	if err := s.store.DeleteItemLink(linkID); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "Link not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
