package store

import "github.com/PerpetualSoftware/pad/internal/models"

// ResolveBacklinksVisibility computes the (fullCollectionIDs,
// grantedItemIDs) access vectors for `userID` in `workspaceID`,
// REQUEST-INDEPENDENT. Mirrors the server's guestResourceFilterCore
// (internal/server/server.go::guestResourceFilterCore) but determines
// the user's effective workspace role from a fresh member+grant
// lookup instead of reading it from the HTTP request context.
//
// This separation exists for the cross-workspace backlinks query
// (PLAN-1593 / TASK-1597), which iterates over every workspace the
// requesting user has access to and needs per-workspace visibility
// for each — a request scope only knows the role for ONE workspace
// (the URL's target), so the cross-ws traversal must compute the
// rest itself.
//
// Decision tree (matches RequireWorkspaceAccess + guestResourceFilterCore):
//
//   - Admin user (`users.role = 'admin'`) AND NOT bearer-authenticated:
//     unrestricted.
//   - Admin user AND bearer-authenticated: falls through to the
//     non-admin path (BUG-1617). The bearer-borne admin identity is
//     treated as a regular user for cross-workspace visibility — a
//     platform admin's MCP / CLI / PAT token must not silently see
//     into workspaces the admin never joined.
//   - Workspace member with `collection_access = 'all'` or empty:
//     unrestricted.
//   - Workspace member with `collection_access = 'specific'`
//     (restricted member): grant-based collections ∪ explicit
//     member_collection_access ∪ system collections, plus any
//     item grants additively.
//   - Non-member with grants (guest): grant-based collections AND
//     item grants only.
//   - Non-member without grants: returns BOTH lists nil (empty
//     visibility — caller's GetBacklinks short-circuits to "see
//     nothing").
//
// Returns nil fullCollIDs and nil grantedItemIDs for the unrestricted
// cases; caller should interpret using BacklinksVisibility semantics
// (Unrestricted=true sets the no-filter shape).
//
// `includeDeletedItems` controls the grant query variant:
//   - false → GuestVisibleResources (live items only). Cross-ws
//     backlinks callers pass false.
//   - true → GuestVisibleResourcesIncludeDeleted (tombstones included).
//     The server delta-sync wrapper at guestResourceFilterIncludeDeletedItems
//     passes true; cross-ws doesn't need it.
//
// `authIsBearer` is the BUG-1616/1617 gate: true when the upstream
// HTTP request was bearer-authenticated (PAT on /api/v1, PAT or OAuth
// on /mcp, CLI session-bearer). Callers without a request context
// (tests, internal jobs) pass false — same as cookie-session admin.
// Derived via server.isBearerAuth at the HTTP boundary and threaded
// down.
//
// The returned error mirrors the underlying store call (DB error
// surfaces, "user not found" returns an empty result rather than
// erroring so the cross-ws traversal can keep going).
func (s *Store) ResolveBacklinksVisibility(userID, workspaceID string, includeDeletedItems, authIsBearer bool) (fullCollIDs, grantedItemIDs []string, err error) {
	return s.ResolveBacklinksVisibilityQ(s.db, userID, workspaceID, includeDeletedItems, authIsBearer)
}

// ResolveBacklinksVisibilityQ is ResolveBacklinksVisibility parameterized
// over its executor (see Queryer). The cross-workspace copy's attachment
// authorizer runs it on the copy transaction's connection.
func (s *Store) ResolveBacklinksVisibilityQ(q Queryer, userID, workspaceID string, includeDeletedItems, authIsBearer bool) (fullCollIDs, grantedItemIDs []string, err error) {
	user, err := s.GetUserQ(q, userID)
	if err != nil {
		return nil, nil, err
	}
	if user == nil {
		// User vanished mid-query (or stale workspace membership row).
		// Empty visibility — caller treats nil/nil/no-error as "see
		// nothing" via BacklinksVisibility{Unrestricted:false} with
		// both lists empty.
		return nil, nil, nil
	}
	// Admin bypass — cookie session auth only (BUG-1617). Bearer-borne
	// admin (CLI / PAT / MCP) falls through to the non-admin path so
	// they're treated identically to a regular user — membership /
	// grants determine visibility, not the platform admin role.
	if user.Role == "admin" && !authIsBearer {
		return nil, nil, nil
	}

	// Member lookup determines whether we treat this as a guest (no
	// member row but has grants) or a member (with full or specific
	// access).
	member, err := s.GetWorkspaceMemberQ(q, workspaceID, userID)
	if err != nil {
		return nil, nil, err
	}

	fullCollIDs, grantedItemIDs, _, err = s.ResolveBacklinksVisibilityWithLoadedQ(q, user, member, workspaceID, includeDeletedItems, authIsBearer)
	return fullCollIDs, grantedItemIDs, err
}

// ResolveBacklinksVisibilityWithLoadedQ is the same visibility decision as
// ResolveBacklinksVisibilityQ for callers that have already loaded `user` and
// their optional workspace `member` row (nil when the caller is not a member
// of `workspaceID`). It also returns `memberCollIDs` — the explicit
// member_collection_access rows loaded for a restricted member — so callers
// such as GET /me/agents can compute collection-edit capabilities without
// re-reading member_collection_access.
func (s *Store) ResolveBacklinksVisibilityWithLoadedQ(q Queryer, user *models.User, member *models.WorkspaceMember, workspaceID string, includeDeletedItems, authIsBearer bool) (fullCollIDs, grantedItemIDs, memberCollIDs []string, err error) {
	if user == nil {
		return nil, nil, nil, nil
	}
	if user.Role == "admin" && !authIsBearer {
		return nil, nil, nil, nil
	}

	// Full-access member — unrestricted.
	if member != nil && (member.CollectionAccess == "all" || member.CollectionAccess == "") {
		return nil, nil, nil, nil
	}

	// Resolve grant-based resources. `member == nil` here means
	// guest; member != nil with specific access means restricted
	// member — both fall through to grant lookups, then merge
	// member-specific collections below if applicable.
	var grantCollIDs []string
	if includeDeletedItems {
		grantCollIDs, grantedItemIDs, err = s.GuestVisibleResourcesIncludeDeletedQ(q, workspaceID, user.ID)
	} else {
		grantCollIDs, grantedItemIDs, err = s.GuestVisibleResourcesQ(q, workspaceID, user.ID)
	}
	if err != nil {
		return nil, nil, nil, err
	}

	// Guest branch: grant-only. (No member_collection_access merge
	// because there's no member row to merge from.)
	if member == nil {
		if grantCollIDs == nil {
			grantCollIDs = []string{}
		}
		if grantedItemIDs == nil {
			grantedItemIDs = []string{}
		}
		return grantCollIDs, grantedItemIDs, nil, nil
	}

	// Restricted member branch — merge grant collections with
	// member_collection_access. System collections are not added: a
	// restricted member reaches one only when it is listed or granted
	// (TASK-3376).
	fullCollSet := make(map[string]bool)
	for _, id := range grantCollIDs {
		fullCollSet[id] = true
	}
	memberCollIDs, err = s.GetMemberCollectionAccessQ(q, workspaceID, user.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, id := range memberCollIDs {
		fullCollSet[id] = true
	}
	fullCollIDs = make([]string, 0, len(fullCollSet))
	for id := range fullCollSet {
		fullCollIDs = append(fullCollIDs, id)
	}
	return fullCollIDs, grantedItemIDs, memberCollIDs, nil
}
