package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/xarmian/pad/internal/models"
)

// handleCreateShareLink creates a new share link for an item or collection.
// POST /workspaces/{ws}/items/{slug}/share-links or
// POST /workspaces/{ws}/collections/{collSlug}/share-links
func (s *Server) handleCreateItemShareLink(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
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

	link, err := s.store.CreateShareLink(workspaceID, "item", item.ID, "view", currentUserID(r))
	if err != nil {
		writeInternalError(w, err)
		return
	}

	if s.baseURL != "" {
		link.URL = s.baseURL + "/s/" + link.Token
	}
	link.TargetTitle = item.Title

	writeJSON(w, http.StatusCreated, link)
}

func (s *Server) handleCreateCollectionShareLink(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
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

	link, err := s.store.CreateShareLink(workspaceID, "collection", coll.ID, "view", currentUserID(r))
	if err != nil {
		writeInternalError(w, err)
		return
	}

	if s.baseURL != "" {
		link.URL = s.baseURL + "/s/" + link.Token
	}
	link.TargetTitle = coll.Name

	writeJSON(w, http.StatusCreated, link)
}

// handleListItemShareLinks lists share links for an item.
func (s *Server) handleListItemShareLinks(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
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

	links, err := s.store.ListShareLinks("item", item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if links == nil {
		links = []models.ShareLink{}
	}
	writeJSON(w, http.StatusOK, links)
}

func (s *Server) handleListCollectionShareLinks(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
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

	links, err := s.store.ListShareLinks("collection", coll.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if links == nil {
		links = []models.ShareLink{}
	}
	writeJSON(w, http.StatusOK, links)
}

// handleDeleteShareLink deletes a share link.
func (s *Server) handleDeleteShareLink(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	linkID := chi.URLParam(r, "linkID")
	if err := s.store.DeleteShareLink(linkID, workspaceID); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Share link not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleResolveShareLink is the /s/{token} route. It resolves a share link
// token and returns the shared content. Anonymous users are ALWAYS read-only (D8).
func (s *Server) handleResolveShareLink(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if token == "" {
		writeError(w, http.StatusNotFound, "not_found", "Not found")
		return
	}

	// Look up the share link by token hash
	link, err := s.store.GetShareLinkByToken(token)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if link == nil {
		// Generic 404 — no info leakage about valid tokens
		writeError(w, http.StatusNotFound, "not_found", "Not found")
		return
	}

	// Validate constraints (expiry, max views)
	if err := s.store.ValidateShareLink(link); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Not found")
		return
	}

	// Require auth check
	if link.RequireAuth {
		user := currentUser(r)
		if user == nil {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"require_auth": true,
				"message":      "Authentication required to view this content",
			})
			return
		}
		// Restrict to specific email
		if link.RestrictToEmail != "" && user.Email != link.RestrictToEmail {
			writeError(w, http.StatusForbidden, "forbidden", "This link is restricted")
			return
		}
	}

	// Record the view
	fingerprint := r.Header.Get("X-Forwarded-For")
	if fingerprint == "" {
		fingerprint = r.RemoteAddr
	}
	userID := ""
	if user := currentUser(r); user != nil {
		userID = user.ID
	}
	_ = s.store.RecordShareLinkView(link.ID, fingerprint, userID)

	// Resolve and return the shared content
	switch link.TargetType {
	case "item":
		item, err := s.store.GetItem(link.TargetID)
		if err != nil || item == nil {
			writeError(w, http.StatusNotFound, "not_found", "Not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"type":       "item",
			"item":       item,
			"permission": "view", // D8: anonymous always read-only
			"share_link": map[string]interface{}{
				"id":          link.ID,
				"target_type": link.TargetType,
			},
		})

	case "collection":
		coll, err := s.store.GetCollection(link.TargetID)
		if err != nil || coll == nil {
			writeError(w, http.StatusNotFound, "not_found", "Not found")
			return
		}
		// Get items in the collection
		items, err := s.store.ListItems(link.WorkspaceID, models.ItemListParams{
			CollectionSlug: coll.Slug,
		})
		if err != nil {
			items = []models.Item{}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"type":       "collection",
			"collection": coll,
			"items":      items,
			"permission": "view", // D8: anonymous always read-only
			"share_link": map[string]interface{}{
				"id":          link.ID,
				"target_type": link.TargetType,
			},
		})

	default:
		writeError(w, http.StatusNotFound, "not_found", "Not found")
	}
}
