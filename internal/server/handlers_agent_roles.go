package server

import (
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/xarmian/pad/internal/models"
)

// handleListAgentRoles returns all agent roles for a workspace.
func (s *Server) handleListAgentRoles(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	roles, err := s.store.ListAgentRoles(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// When visibility is restricted, recompute item counts from visible items only
	visibleIDs, _ := s.visibleCollectionIDs(r, workspaceID)
	if visibleIDs != nil {
		visibleItems, _ := s.store.ListItems(workspaceID, models.ItemListParams{CollectionIDs: visibleIDs})
		// Build role → count map from visible items
		roleCounts := make(map[string]int)
		for _, item := range visibleItems {
			if item.AgentRoleID != nil && *item.AgentRoleID != "" {
				roleCounts[*item.AgentRoleID]++
			}
		}
		for i := range roles {
			roles[i].ItemCount = roleCounts[roles[i].ID]
		}
	}

	writeJSON(w, http.StatusOK, roles)
}

// handleCreateAgentRole creates a new agent role in a workspace.
func (s *Server) handleCreateAgentRole(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	var input models.AgentRoleCreate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}

	role, err := s.store.CreateAgentRole(workspaceID, input)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, role)
}

// handleGetAgentRole returns a single agent role by ID or slug.
func (s *Server) handleGetAgentRole(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	roleID := chi.URLParam(r, "roleID")
	role, err := s.store.GetAgentRole(workspaceID, roleID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if role == nil {
		writeError(w, http.StatusNotFound, "not_found", "Agent role not found")
		return
	}

	writeJSON(w, http.StatusOK, role)
}

// handleUpdateAgentRole updates an existing agent role.
func (s *Server) handleUpdateAgentRole(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	roleID := chi.URLParam(r, "roleID")
	var input models.AgentRoleUpdate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	role, err := s.store.UpdateAgentRole(workspaceID, roleID, input)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if role == nil {
		writeError(w, http.StatusNotFound, "not_found", "Agent role not found")
		return
	}

	writeJSON(w, http.StatusOK, role)
}

// handleDeleteAgentRole removes an agent role from a workspace.
func (s *Server) handleDeleteAgentRole(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	roleID := chi.URLParam(r, "roleID")
	if err := s.store.DeleteAgentRole(workspaceID, roleID); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "Agent role not found")
			return
		}
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
