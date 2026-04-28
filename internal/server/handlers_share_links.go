package server

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/go-chi/chi/v5"
)

// validateShareLinkOpts checks that share link creation constraints are sane.
// Returns an error message string (empty if valid).
func validateShareLinkOpts(expiresAt *string, maxViews *int) string {
	if expiresAt != nil {
		if _, err := time.Parse(time.RFC3339, *expiresAt); err != nil {
			return "expires_at must be a valid RFC3339 timestamp"
		}
	}
	if maxViews != nil && *maxViews <= 0 {
		return "max_views must be a positive integer"
	}
	return ""
}

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

	var input struct {
		Password        string  `json:"password,omitempty"`
		ExpiresAt       *string `json:"expires_at,omitempty"`
		MaxViews        *int    `json:"max_views,omitempty"`
		RequireAuth     bool    `json:"require_auth,omitempty"`
		RestrictToEmail string  `json:"restrict_to_email,omitempty"`
	}
	if err := decodeJSON(r, &input); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body")
		return
	}

	// Normalize and force require_auth when restrict_to_email is set —
	// otherwise the email restriction would be silently ignored.
	if input.RestrictToEmail != "" {
		input.RestrictToEmail = strings.ToLower(strings.TrimSpace(input.RestrictToEmail))
		input.RequireAuth = true
	}

	if msg := validateShareLinkOpts(input.ExpiresAt, input.MaxViews); msg != "" {
		writeError(w, http.StatusBadRequest, "bad_request", msg)
		return
	}

	opts := &store.ShareLinkOptions{
		Password:        input.Password,
		ExpiresAt:       input.ExpiresAt,
		MaxViews:        input.MaxViews,
		RequireAuth:     input.RequireAuth,
		RestrictToEmail: input.RestrictToEmail,
	}

	link, err := s.store.CreateShareLink(workspaceID, "item", item.ID, "view", currentUserID(r), opts)
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

	var collInput struct {
		Password        string  `json:"password,omitempty"`
		ExpiresAt       *string `json:"expires_at,omitempty"`
		MaxViews        *int    `json:"max_views,omitempty"`
		RequireAuth     bool    `json:"require_auth,omitempty"`
		RestrictToEmail string  `json:"restrict_to_email,omitempty"`
	}
	if err := decodeJSON(r, &collInput); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body")
		return
	}

	// Normalize and force require_auth when restrict_to_email is set
	if collInput.RestrictToEmail != "" {
		collInput.RestrictToEmail = strings.ToLower(strings.TrimSpace(collInput.RestrictToEmail))
		collInput.RequireAuth = true
	}

	if msg := validateShareLinkOpts(collInput.ExpiresAt, collInput.MaxViews); msg != "" {
		writeError(w, http.StatusBadRequest, "bad_request", msg)
		return
	}

	collOpts := &store.ShareLinkOptions{
		Password:        collInput.Password,
		ExpiresAt:       collInput.ExpiresAt,
		MaxViews:        collInput.MaxViews,
		RequireAuth:     collInput.RequireAuth,
		RestrictToEmail: collInput.RestrictToEmail,
	}

	link, err := s.store.CreateShareLink(workspaceID, "collection", coll.ID, "view", currentUserID(r), collOpts)
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
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "Share link not found")
		} else {
			writeInternalError(w, err)
		}
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

	// Auth check first — prevent unauthenticated callers from probing
	// passwords (which burns bcrypt CPU) before being rejected by the gate.
	if link.RequireAuth {
		user := currentUser(r)
		if user == nil {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"require_auth": true,
				"message":      "Authentication required to view this content",
			})
			return
		}
		// Restrict to specific email (stored normalized; normalize user email too)
		if link.RestrictToEmail != "" && strings.ToLower(strings.TrimSpace(user.Email)) != link.RestrictToEmail {
			writeError(w, http.StatusForbidden, "forbidden", "This link is restricted")
			return
		}
	}

	// Password check — via X-Share-Password header (never query string, to
	// avoid leaking passwords in logs, browser history, and referrers).
	if link.HasPassword {
		password := r.Header.Get("X-Share-Password")
		if password == "" {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"require_password": true,
				"message":          "Password required to view this content",
			})
			return
		}
		if !s.store.ValidateShareLinkPassword(link, password) {
			writeError(w, http.StatusForbidden, "forbidden", "Incorrect password")
			return
		}
	}

	// Atomically record the view and enforce max_views
	fingerprint := clientIP(r)
	userID := ""
	if user := currentUser(r); user != nil {
		userID = user.ID
	}
	allowed, err := s.store.RecordShareLinkView(link.ID, fingerprint, userID, link.MaxViews)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if !allowed {
		// max_views reached — treat as expired
		writeError(w, http.StatusNotFound, "not_found", "Not found")
		return
	}

	// Resolve and return the shared content using public DTOs
	// to avoid leaking internal IDs, creator info, and other sensitive fields.
	switch link.TargetType {
	case "item":
		item, err := s.store.GetItem(link.TargetID)
		if err != nil || item == nil {
			writeError(w, http.StatusNotFound, "not_found", "Not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"type": "item",
			"item": map[string]interface{}{
				"title":           item.Title,
				"content":         item.Content,
				"fields":          item.Fields,
				"ref":             item.Ref,
				"collection_name": item.CollectionName,
				"collection_icon": item.CollectionIcon,
			},
			"permission": "view",
			"share_link": map[string]interface{}{
				"target_type": link.TargetType,
			},
		})

	case "collection":
		coll, err := s.store.GetCollection(link.TargetID)
		if err != nil || coll == nil {
			writeError(w, http.StatusNotFound, "not_found", "Not found")
			return
		}
		items, err := s.store.ListItems(link.WorkspaceID, models.ItemListParams{
			CollectionSlug: coll.Slug,
		})
		if err != nil {
			writeInternalError(w, err)
			return
		}
		// Build public item list with only safe fields
		publicItems := make([]map[string]interface{}, 0, len(items))
		for _, it := range items {
			publicItem := map[string]interface{}{
				"title":  it.Title,
				"ref":    it.Ref,
				"fields": it.Fields,
			}
			publicItems = append(publicItems, publicItem)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"type": "collection",
			"collection": map[string]interface{}{
				"name":        coll.Name,
				"icon":        coll.Icon,
				"description": coll.Description,
			},
			"items":      publicItems,
			"permission": "view",
			"share_link": map[string]interface{}{
				"target_type": link.TargetType,
			},
		})

	default:
		writeError(w, http.StatusNotFound, "not_found", "Not found")
	}
}

// handleShareLinkViews returns view history for a share link.
// GET /workspaces/{ws}/share-links/{linkID}/views
func (s *Server) handleShareLinkViews(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	linkID := chi.URLParam(r, "linkID")
	link, err := s.store.GetShareLink(linkID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if link == nil || link.WorkspaceID != workspaceID {
		writeError(w, http.StatusNotFound, "not_found", "Share link not found")
		return
	}

	const maxViewLimit = 1000
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > maxViewLimit {
		limit = maxViewLimit
	}

	views, err := s.store.ListShareLinkViews(linkID, limit)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if views == nil {
		views = []models.ShareLinkView{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"views":          views,
		"total_views":    link.ViewCount,
		"unique_viewers": link.UniqueViewers,
		"last_viewed_at": link.LastViewedAt,
	})
}
