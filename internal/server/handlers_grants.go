package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/xarmian/pad/internal/models"
)

// --- Collection Grant handlers ---

func (s *Server) handleListCollectionGrants(w http.ResponseWriter, r *http.Request) {
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

	grants, err := s.store.ListCollectionGrants(coll.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, grants)
}

func (s *Server) handleCreateCollectionGrant(w http.ResponseWriter, r *http.Request) {
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

	var input struct {
		UserID     string `json:"user_id"`
		Email      string `json:"email"`
		Permission string `json:"permission"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Permission == "" {
		input.Permission = "view"
	}
	if input.Permission != "view" && input.Permission != "edit" {
		writeError(w, http.StatusBadRequest, "validation_error", "Permission must be 'view' or 'edit'")
		return
	}

	// Resolve user by ID or email
	userID := input.UserID
	if userID == "" && input.Email != "" {
		user, err := s.store.GetUserByEmail(input.Email)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if user == nil {
			writeError(w, http.StatusNotFound, "not_found", "User not found")
			return
		}
		userID = user.ID
	}
	if userID == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "user_id or email is required")
		return
	}

	grant, err := s.store.CreateCollectionGrant(workspaceID, coll.ID, userID, input.Permission, currentUserID(r))
	if err != nil {
		writeError(w, http.StatusConflict, "conflict", "Grant already exists or failed to create")
		return
	}

	writeJSON(w, http.StatusCreated, grant)
}

func (s *Server) handleDeleteCollectionGrant(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	grantID := chi.URLParam(r, "grantID")
	if err := s.store.DeleteCollectionGrant(grantID, workspaceID); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Grant not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Item Grant handlers ---

func (s *Server) handleListItemGrants(w http.ResponseWriter, r *http.Request) {
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

	grants, err := s.store.ListItemGrants(item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, grants)
}

func (s *Server) handleCreateItemGrant(w http.ResponseWriter, r *http.Request) {
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
		UserID     string `json:"user_id"`
		Email      string `json:"email"`
		Permission string `json:"permission"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Permission == "" {
		input.Permission = "view"
	}
	if input.Permission != "view" && input.Permission != "edit" {
		writeError(w, http.StatusBadRequest, "validation_error", "Permission must be 'view' or 'edit'")
		return
	}

	userID := input.UserID
	if userID == "" && input.Email != "" {
		user, err := s.store.GetUserByEmail(input.Email)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if user == nil {
			writeError(w, http.StatusNotFound, "not_found", "User not found")
			return
		}
		userID = user.ID
	}
	if userID == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "user_id or email is required")
		return
	}

	grant, err := s.store.CreateItemGrant(workspaceID, item.ID, userID, input.Permission, currentUserID(r))
	if err != nil {
		writeError(w, http.StatusConflict, "conflict", "Grant already exists or failed to create")
		return
	}

	writeJSON(w, http.StatusCreated, grant)
}

func (s *Server) handleDeleteItemGrant(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	grantID := chi.URLParam(r, "grantID")
	if err := s.store.DeleteItemGrant(grantID, workspaceID); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Grant not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- User grants (cross-cutting) ---

func (s *Server) handleListUserGrants(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	userID := chi.URLParam(r, "userID")

	// Only workspace owners or the user themselves can view grants
	if !requireRole(r, "owner") && currentUserID(r) != userID {
		writeError(w, http.StatusForbidden, "forbidden", "Insufficient permissions")
		return
	}

	collGrants, itemGrants, err := s.store.ListUserGrants(workspaceID, userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if collGrants == nil {
		collGrants = []models.CollectionGrant{}
	}
	if itemGrants == nil {
		itemGrants = []models.ItemGrant{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"collection_grants": collGrants,
		"item_grants":       itemGrants,
	})
}
