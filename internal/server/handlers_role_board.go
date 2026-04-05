package server

import (
	"net/http"

	"github.com/xarmian/pad/internal/store"
)

// handleRoleBoardReorder updates role_sort_order for items within a lane.
func (s *Server) handleRoleBoardReorder(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	var updates []store.RoleSortUpdate
	if err := decodeJSON(r, &updates); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if err := s.store.UpdateRoleSortOrder(workspaceID, updates); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleRoleBoardLaneReorder updates sort_order for roles (lane ordering).
func (s *Server) handleRoleBoardLaneReorder(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	var updates []store.RoleOrderUpdate
	if err := decodeJSON(r, &updates); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if err := s.store.UpdateAgentRoleOrder(workspaceID, updates); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleRoleBoard returns items across all collections grouped by agent role.
// This powers the standalone role board page in the web UI.
func (s *Server) handleRoleBoard(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	params := store.RoleBoardParams{
		AssignedUserID: r.URL.Query().Get("assigned_user_id"),
	}

	lanes, err := s.store.GetRoleBoardItems(workspaceID, params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"lanes": lanes,
	})
}
