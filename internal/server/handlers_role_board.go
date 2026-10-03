package server

import (
	"errors"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// handleRoleBoardReorder updates role_sort_order for items within a lane.
// Permission is checked per-item via requireEditPermission (grant-aware),
// so guests/viewers with edit grants can reorder their granted items.
func (s *Server) handleRoleBoardReorder(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	var updates []store.RoleSortUpdate
	if err := decodeJSON(r, &updates); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// Verify all items being reordered are visible and editable.
	// Uses grant-aware edit check so restricted members/guests with
	// item-only grants can't reorder items they don't have edit access to.
	for _, u := range updates {
		item, err := s.store.GetItem(u.ItemID)
		// GetItem is not workspace-scoped, so an item from another
		// workspace is answered exactly as a missing one (BUG-3342).
		if err != nil || item == nil || item.WorkspaceID != workspaceID {
			writeError(w, http.StatusForbidden, "forbidden", "Cannot reorder items in hidden collections")
			return
		}
		if !s.requireItemVisible(w, r, workspaceID, item) {
			return
		}
		if !s.requireEditPermission(w, r, workspaceID, item.ID, item.CollectionID) {
			return
		}
	}

	if err := s.store.UpdateRoleSortOrder(workspaceID, updates); err != nil {
		// An item deleted between the check above and the write: same
		// answer as one that was never there, and nothing was written.
		if errors.Is(err, store.ErrRoleSortItemNotFound) {
			writeError(w, http.StatusForbidden, "forbidden", "Cannot reorder items in hidden collections")
			return
		}
		writeInternalError(w, err)
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
		writeInternalError(w, err)
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

	visibleIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
	if visErr != nil {
		writeInternalError(w, visErr)
		return
	}

	// For users with item-level grants, use item-level filtering
	rbCollIDs := visibleIDs
	var rbItemIDs []string
	rbFullCollIDs, rbGrantedItemIDs, rbGrantErr := s.guestResourceFilter(r, workspaceID)
	if rbGrantErr != nil {
		writeInternalError(w, rbGrantErr)
		return
	}
	if len(rbGrantedItemIDs) > 0 {
		rbCollIDs = rbFullCollIDs
		rbItemIDs = rbGrantedItemIDs
	}

	params := store.RoleBoardParams{
		AssignedUserID: r.URL.Query().Get("assigned_user_id"),
		CollectionIDs:  rbCollIDs,
		ItemIDs:        rbItemIDs,
	}

	lanes, err := s.store.GetRoleBoardItems(workspaceID, params)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Each lane embeds its role, whose item_count the store fills from every
	// item in the workspace. The lane's items were filtered above and its
	// count was not (BUG-3257), so recompute it from what the caller may see.
	counts, restricted, err := s.visibleRoleItemCounts(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if restricted {
		for _, lane := range lanes {
			if lane.Role != nil {
				lane.Role.ItemCount = counts[lane.Role.ID]
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"lanes": lanes,
	})
}
