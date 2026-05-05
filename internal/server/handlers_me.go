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
	// can see in navigation when CollectionAccess == "specific". Empty (nil)
	// when "all". Server-computed via VisibleCollectionIDs /
	// GuestVisibleCollectionIDs so it includes system collections, direct
	// collection grants, and collections that contain item-granted items
	// (so the collection appears in nav for guests).
	//
	// IMPORTANT: this set is intentionally broader than "items in this
	// collection are all accessible". For per-item visibility, use
	// FullAccessCollectionIDs — see field below.
	VisibleCollectionIDs []string `json:"visible_collection_ids"`

	// FullAccessCollectionIDs is the strict set of collections in which
	// every item is accessible to the user — i.e. the collection itself
	// confers access, not just one item inside it. This mirrors the
	// `fullCollIDs` set used by guestResourceFilter / isItemVisibleToGuest
	// in handlers: direct collection grants + member_collection_access +
	// system collections (for restricted members).
	//
	// Empty (nil) when CollectionAccess == "all" (the user has full access
	// everywhere). For guests with only item grants, this is empty even when
	// VisibleCollectionIDs is non-empty — the guest's nav shows the
	// item-grant collection, but only the granted item itself is accessible.
	FullAccessCollectionIDs []string `json:"full_access_collection_ids"`

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

	// Always attach the user's grants — used by the frontend helpers to
	// override role-based decisions per the server cascade. We need the
	// grants in scope before computing FullAccessCollectionIDs.
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

	if member != nil {
		// Member: collection_access mode comes from the member row.
		if member.CollectionAccess == "specific" {
			resp.CollectionAccess = "specific"

			// Nav visibility — broader, includes item-grant collections so
			// the collection appears in the sidebar even if the user only
			// has access to one item inside it.
			navIDs, err := s.store.VisibleCollectionIDs(workspaceID, user.ID)
			if err != nil {
				writeInternalError(w, err)
				return
			}
			if navIDs == nil {
				navIDs = []string{}
			}
			resp.VisibleCollectionIDs = navIDs

			// Strict full-access set — used for per-item visibility
			// decisions. Mirrors guestResourceFilter's fullCollIDs for
			// restricted members: explicit member_collection_access +
			// system collections + direct collection grants. Item-grant
			// collections are intentionally excluded — having an item grant
			// in a collection does NOT confer access to siblings.
			fullSet := make(map[string]struct{})
			memberColls, err := s.store.GetMemberCollectionAccess(workspaceID, user.ID)
			if err != nil {
				writeInternalError(w, err)
				return
			}
			for _, id := range memberColls {
				fullSet[id] = struct{}{}
			}
			sysColls, err := s.store.ListSystemCollectionIDs(workspaceID)
			if err != nil {
				writeInternalError(w, err)
				return
			}
			for _, id := range sysColls {
				fullSet[id] = struct{}{}
			}
			for _, g := range collGrants {
				fullSet[g.CollectionID] = struct{}{}
			}
			fullIDs := make([]string, 0, len(fullSet))
			for id := range fullSet {
				fullIDs = append(fullIDs, id)
			}
			resp.FullAccessCollectionIDs = fullIDs
		}
	} else if role == "guest" {
		// Guest: no membership row; visibility is grant-derived only.
		resp.CollectionAccess = "specific"

		// Nav: collections with direct grants + collections that contain
		// item-granted items.
		navIDs, err := s.store.GuestVisibleCollectionIDs(workspaceID, user.ID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if navIDs == nil {
			navIDs = []string{}
		}
		resp.VisibleCollectionIDs = navIDs

		// Strict full-access: only collections with direct collection grants
		// confer access to all items inside; item grants do not promote.
		fullSet := make(map[string]struct{})
		for _, g := range collGrants {
			fullSet[g.CollectionID] = struct{}{}
		}
		fullIDs := make([]string, 0, len(fullSet))
		for id := range fullSet {
			fullIDs = append(fullIDs, id)
		}
		resp.FullAccessCollectionIDs = fullIDs
	}

	writeJSON(w, http.StatusOK, resp)
}
