package server

import (
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

type commitmentAccessMode int

const (
	commitmentAccessLive commitmentAccessMode = iota
	commitmentAccessRemove
)

// buildCommitmentAccess runs only for an already admitted workspace. All reads
// use q, including when a mutation reloads permissions inside its transaction.
func (s *Server) buildCommitmentAccess(r *http.Request, q store.Queryer, workspaceID, itemID string, mode commitmentAccessMode) (models.CommitmentAccess, error) {
	access := models.CommitmentAccess{Readable: map[string]bool{}, Editable: map[string]bool{}}
	items, err := s.store.CommitmentAccessItemsQ(q, workspaceID, itemID)
	if err != nil {
		return access, err
	}
	var ownerID string
	if err := q.QueryRow(s.store.D().Rebind(`SELECT COALESCE(owner_id,'') FROM workspaces WHERE id=?`), workspaceID).Scan(&ownerID); err != nil {
		return access, err
	}
	user := currentUser(r)
	role := workspaceRole(r)
	owner := user == nil && role == "owner"
	var member *models.WorkspaceMember
	if user != nil {
		member, err = s.store.GetWorkspaceMemberQ(q, workspaceID, user.ID)
		if err != nil {
			return access, err
		}
		owner = ownerID == user.ID || (member != nil && member.Role == "owner")
		// Resolve the role for this workspace rather than borrowing a destination's role.
		role = "guest"
		if member != nil {
			role = member.Role
		}
		if owner {
			role = "owner"
		}
	}
	writes := tokenScopeAllows(TokenScopesFromContext(r.Context()), http.MethodPatch, "")
	for _, it := range items {
		if it.DeletedAt != nil && !(owner && mode == commitmentAccessRemove) {
			continue
		}
		if owner {
			access.Readable[it.ID] = true
			access.Editable[it.ID] = true
			continue
		}
		visible, err := s.checkItemVisibleQ(q, workspaceID, &models.Item{ID: it.ID, WorkspaceID: workspaceID, CollectionID: it.CollectionID}, user, role, isBearerAuth(r))
		if err != nil {
			return models.CommitmentAccess{}, err
		}
		if !visible {
			continue
		}
		access.Readable[it.ID] = true
		if !writes || !appWriteAllows(r, it.CollectionID) {
			continue
		}
		// Role reach precedes grants, matching canEditInCollection: a view grant
		// never takes editing away from an editor inside their collection access.
		reaches := user == nil && (role == "editor" || role == "owner")
		if user != nil && user.Role == "admin" && !isBearerAuth(r) {
			reaches = true
		}
		if !reaches && member != nil && (member.Role == "editor" || member.Role == "owner") {
			reaches = member.Role == "owner" || member.CollectionAccess == "" || member.CollectionAccess == "all"
			if !reaches {
				ids, err := s.store.GetMemberCollectionAccessQ(q, workspaceID, user.ID)
				if err != nil {
					return models.CommitmentAccess{}, err
				}
				for _, id := range ids {
					if id == it.CollectionID {
						reaches = true
						break
					}
				}
			}
		}
		if reaches {
			access.Editable[it.ID] = true
			continue
		}
		if user != nil {
			perm, err := s.store.ResolveUserPermissionQ(q, workspaceID, user.ID, it.ID, it.CollectionID)
			if err != nil {
				return models.CommitmentAccess{}, err
			}
			access.Editable[it.ID] = permissionLevel(perm) >= permissionLevel("edit")
		}
	}
	return access, nil
}
