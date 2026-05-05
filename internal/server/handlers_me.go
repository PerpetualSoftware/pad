package server

import (
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// meResponse is the shape returned by GET /api/v1/workspaces/{ws}/me.
//
// It provides the current authenticated user's effective workspace context in
// a single round-trip so the web UI can decide which affordances to render
// without needing to compose role + member_collection_access + grants on the
// client. The frontend's permission helpers in workspace.svelte.ts mirror the
// server's ResolveUserPermission cascade (owner → item grant → collection
// grant → membership role + visibility) using these fields.
type meResponse struct {
	// Role is the user's effective workspace role: owner, editor, viewer, or guest.
	// Admin platform users are normalized to "owner" by the middleware. Legacy
	// workspace-scoped API tokens (no user context) are normalized to "editor".
	Role string `json:"role"`

	// CollectionAccess is the user's collection access mode.
	// "all" means no per-collection filter (owners, admins, and members with
	// CollectionAccess="all" or unset). "specific" means visibility is gated
	// by visible_collection_ids (restricted members and guests).
	CollectionAccess string `json:"collection_access"`

	// VisibleCollectionIDs is the explicit list of collection IDs the user
	// can see when CollectionAccess == "specific". Empty (nil) when "all".
	// Server-computed via VisibleCollectionIDs / GuestVisibleCollectionIDs so
	// it includes system collections, direct collection grants, and
	// collections containing item-granted items.
	VisibleCollectionIDs []string `json:"visible_collection_ids"`

	// CollectionGrants are the user's direct per-collection grants in this
	// workspace.
	CollectionGrants []models.CollectionGrant `json:"collection_grants"`

	// ItemGrants are the user's direct per-item grants in this workspace.
	ItemGrants []models.ItemGrant `json:"item_grants"`
}

// handleGetMe returns the current authenticated user's workspace context.
//
// This is the foundation primitive for client-side permission gating
// (PLAN-1100 / TASK-1101). It is open to any authenticated principal that the
// workspace-access middleware admits — owners, members of any role, and
// guests with grants. Non-members with no grants are already rejected
// upstream by RequireWorkspaceAccess.
func (s *Server) handleGetMe(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	role := workspaceRole(r)

	resp := meResponse{
		Role:             role,
		CollectionAccess: "all",
	}

	user := currentUser(r)

	// Legacy workspace-scoped API tokens have no user context — they get
	// editor role with no per-resource grants. Return a minimal response.
	if user == nil {
		resp.CollectionGrants = []models.CollectionGrant{}
		resp.ItemGrants = []models.ItemGrant{}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Admin platform users get owner-equivalent access regardless of
	// membership — surface that as "all" with no filtering.
	if user.Role == "admin" {
		resp.CollectionGrants = []models.CollectionGrant{}
		resp.ItemGrants = []models.ItemGrant{}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Resolve member collection access (only meaningful for members; guests
	// fall through to GuestVisibleCollectionIDs below).
	member, err := s.store.GetWorkspaceMember(workspaceID, user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	if member != nil {
		// Member: collection_access mode comes from the member row.
		// VisibleCollectionIDs returns nil for "all" access (signalling no
		// filter) and an explicit list for "specific" access (which already
		// includes system collections + direct collection grants + item-grant
		// collections per workspace_members.go:139).
		if member.CollectionAccess == "specific" {
			resp.CollectionAccess = "specific"
			ids, err := s.store.VisibleCollectionIDs(workspaceID, user.ID)
			if err != nil {
				writeInternalError(w, err)
				return
			}
			if ids == nil {
				ids = []string{}
			}
			resp.VisibleCollectionIDs = ids
		}
	} else if role == "guest" {
		// Guest: no membership row; visibility is grant-derived only.
		resp.CollectionAccess = "specific"
		ids, err := s.store.GuestVisibleCollectionIDs(workspaceID, user.ID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if ids == nil {
			ids = []string{}
		}
		resp.VisibleCollectionIDs = ids
	}

	// Always attach the user's grants — used by the frontend helpers to
	// override role-based decisions per the server cascade.
	collGrants, itemGrants, err := s.store.ListUserGrants(workspaceID, user.ID)
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
	resp.CollectionGrants = collGrants
	resp.ItemGrants = itemGrants

	writeJSON(w, http.StatusOK, resp)
}
