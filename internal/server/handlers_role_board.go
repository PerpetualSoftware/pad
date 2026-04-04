package server

import (
	"net/http"

	"github.com/xarmian/pad/internal/store"
)

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
