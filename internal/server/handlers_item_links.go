package server

import (
	"database/sql"
	"net/http"
	"strings"

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

	links, err := s.store.GetItemLinks(item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if links == nil {
		links = []models.ItemLink{}
	}

	// Filter out links where the linked item is in a hidden collection
	visibleIDs, _ := s.visibleCollectionIDs(r, workspaceID)
	if visibleIDs != nil {
		filtered := links[:0]
		for _, link := range links {
			// Check the "other side" of the link
			otherID := link.TargetID
			if otherID == item.ID {
				otherID = link.SourceID
			}
			if other, err := s.store.GetItem(otherID); err == nil && other != nil {
				if isCollectionVisible(other.CollectionID, visibleIDs) {
					filtered = append(filtered, link)
				}
			}
		}
		links = filtered
	}

	writeJSON(w, http.StatusOK, links)
}

// handleCreateItemLink creates a new link between two items.
func (s *Server) handleCreateItemLink(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
		return
	}
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

	var input models.ItemLinkCreate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.TargetID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "target_id is required")
		return
	}

	linkType, err := models.NormalizeItemLinkType(input.LinkType)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	input.LinkType = linkType

	// Verify target item exists and is in a visible collection
	target, err := s.store.GetItem(input.TargetID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if target == nil || target.WorkspaceID != workspaceID {
		writeError(w, http.StatusBadRequest, "bad_request", "Target item not found")
		return
	}
	if !s.requireItemVisible(w, r, workspaceID, target) {
		return
	}
	if target.ID == item.ID {
		writeError(w, http.StatusBadRequest, "bad_request", "cannot link an item to itself")
		return
	}

	// Parent links enforce single-parent: an item can only belong to one parent.
	// Use SetParentLink which handles upsert and cycle detection.
	if input.LinkType == models.ItemLinkTypeParent {
		actor, _ := actorFromRequest(r)
		link, err := s.store.SetParentLink(workspaceID, item.ID, target.ID, actor)
		if err != nil {
			if strings.Contains(err.Error(), "cycle") {
				writeError(w, http.StatusBadRequest, "bad_request", err.Error())
				return
			}
			writeInternalError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, link)
		return
	}

	link, err := s.store.CreateItemLink(workspaceID, input, item.ID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			writeError(w, http.StatusConflict, "conflict", "Link already exists")
			return
		}
		if strings.Contains(err.Error(), "invalid link type") || strings.Contains(err.Error(), "cannot link an item to itself") {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, link)
}

// handleDeleteItemLink removes a link between items.
func (s *Server) handleDeleteItemLink(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
		return
	}
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
		writeInternalError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
