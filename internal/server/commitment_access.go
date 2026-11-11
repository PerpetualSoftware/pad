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

// commitmentOwnerQ reports whether the caller owns the workspace, by owner_id
// or by an owner membership, and returns that membership.
func (s *Server) commitmentOwnerQ(r *http.Request, q store.Queryer, workspaceID string) (bool, *models.WorkspaceMember, error) {
	user := currentUser(r)
	if user == nil {
		return workspaceRole(r) == "owner", nil, nil
	}
	var ownerID string
	if err := q.QueryRow(s.store.D().Rebind(`SELECT COALESCE(owner_id,'') FROM workspaces WHERE id=?`), workspaceID).Scan(&ownerID); err != nil {
		return false, nil, err
	}
	member, err := s.store.GetWorkspaceMemberQ(q, workspaceID, user.ID)
	if err != nil {
		return false, nil, err
	}
	return ownerID == user.ID || (member != nil && member.Role == "owner"), member, nil
}

// commitmentOwnerRemoves is the one predicate for reaching past collection
// restrictions and onto deleted ends. It applies only to removal, and an app
// principal never takes it past its install ceiling. The hidden-partner
// refusal uses it too, so only a caller who can remove is told they can.
func commitmentOwnerRemoves(owner bool, user *models.User) bool {
	return owner && !user.IsApp()
}

// buildCommitmentAccess runs only for an already admitted workspace. All reads
// use q, including when a mutation reloads permissions inside its transaction.
func (s *Server) buildCommitmentAccess(r *http.Request, q store.Queryer, workspaceID, itemID string, mode commitmentAccessMode) (models.CommitmentAccess, error) {
	access := models.CommitmentAccess{Readable: map[string]bool{}, Editable: map[string]bool{}}
	items, err := s.store.CommitmentAccessItemsQ(q, workspaceID, itemID)
	if err != nil {
		return access, err
	}
	owner, member, err := s.commitmentOwnerQ(r, q, workspaceID)
	if err != nil {
		return access, err
	}
	user := currentUser(r)
	role := workspaceRole(r)
	if user != nil {
		// Resolve the role for this workspace rather than borrowing a destination's role.
		role = "guest"
		if member != nil {
			role = member.Role
		}
		if owner {
			role = "owner"
		}
	}
	var memberCollections map[string]bool
	writes := tokenScopeAllows(TokenScopesFromContext(r.Context()), http.MethodPatch, "")
	ownerRemove := mode == commitmentAccessRemove && commitmentOwnerRemoves(owner, user)
	for _, it := range items {
		if it.DeletedAt != nil && !ownerRemove {
			continue
		}
		if ownerRemove {
			access.Readable[it.ID] = true
			access.Editable[it.ID] = writes && appWriteAllows(r, it.CollectionID)
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
				// The member's collection list depends only on the member: read it once.
				if memberCollections == nil {
					ids, err := s.store.GetMemberCollectionAccessQ(q, workspaceID, user.ID)
					if err != nil {
						return models.CommitmentAccess{}, err
					}
					memberCollections = make(map[string]bool, len(ids))
					for _, id := range ids {
						memberCollections[id] = true
					}
				}
				reaches = memberCollections[it.CollectionID]
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
