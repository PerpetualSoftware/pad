package server

import (
	"log/slog"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// filterWatchesByCurrentAccess re-checks each watch's CURRENT visibility
// (workspace membership / role / collection access / item grants)
// before it is used to gate SSE delivery or returned from
// GET /api/v1/watches (TASK-2533, codex round 1 finding 1).
//
// Store.ListWatchesForUser's doc comment explains why this exists: the
// watches table durably outlives a workspace membership or grant
// revocation — nothing deletes a watch row when access is pulled — so
// without this step a caller removed from a workspace (or whose guest
// grant was revoked) would keep receiving live nudges and keep seeing
// the watch in `pad watch list`, leaking item title/ref, workspace slug,
// actor, and summary for a workspace they can no longer see.
//
// Grouped by workspace (one RBAC resolution per DISTINCT workspace among
// the caller's watches, not one per watch): a caller's watches can
// legitimately span many workspaces, unlike a single SSE connection
// (handlers_events.go's computeSSEVisibility), which only ever resolves
// visibility for the ONE workspace it's subscribed to.
func (s *Server) filterWatchesByCurrentAccess(userID string, watches []models.Watch) []models.Watch {
	if len(watches) == 0 {
		return watches
	}

	user, err := s.store.GetUser(userID)
	if err != nil || user == nil {
		// Can't resolve the caller at all — fail closed.
		slog.Warn("watch-access: failed to resolve user, denying all watches", "user_id", userID, "error", err)
		return nil
	}

	byWorkspace := make(map[string][]models.Watch)
	for _, w := range watches {
		byWorkspace[w.WorkspaceID] = append(byWorkspace[w.WorkspaceID], w)
	}

	out := make([]models.Watch, 0, len(watches))
	for workspaceID, wsWatches := range byWorkspace {
		vis := s.computeWatchAccessVisibility(user, workspaceID)
		for _, w := range wsWatches {
			if vis.allows(w.ItemCollectionID, w.ItemID) {
				out = append(out, w)
			}
		}
	}
	return out
}

// watchAccessVisibility is filterWatchesByCurrentAccess's per-workspace
// visibility snapshot. Deliberately keyed by ID (collection ID / item
// ID), unlike handlers_events.go's sseVisibility, which is keyed by
// collection SLUG because it routes live workspace-scoped events that
// only carry a slug — models.Watch already carries ItemCollectionID and
// ItemID directly, so ID-keying here avoids an extra slug round trip per
// collection.
type watchAccessVisibility struct {
	// fullAccess is true for admin or a member with unrestricted
	// collection access (CollectionAccess != "specific"). true means "no
	// filtering" — every watch in this workspace passes.
	fullAccess bool
	// visibleCollIDs is the set of collection IDs the caller can see in
	// full (nil/empty when fullAccess is true or the caller has none).
	visibleCollIDs map[string]bool
	// grantedItemIDs are individually-granted items (guest item grants,
	// or a "specific"-access member's item-level grants) — visible
	// regardless of visibleCollIDs. nil when the caller has no
	// item-level grants (the common case).
	grantedItemIDs map[string]bool
}

func (v watchAccessVisibility) allows(collectionID, itemID string) bool {
	if v.fullAccess {
		return true
	}
	if v.visibleCollIDs[collectionID] {
		return true
	}
	return v.grantedItemIDs[itemID]
}

// computeWatchAccessVisibility resolves a user's current access within
// one workspace. Built from the same two store primitives
// computeSSEVisibility uses (VisibleCollectionIDs + GuestVisibleResources)
// rather than reimplementing membership/grant SQL here — see that
// function (handlers_events.go) for the underlying RBAC shape this
// mirrors, minus the slug translation this call site doesn't need.
//
// Admin bypass note: unlike computeSSEVisibility's BUG-1616 carve-out
// (cookie-session admin only, bearer-borne admin gets the real grant-
// based check), this treats ANY admin as full-access regardless of auth
// transport. That's deliberately simpler and still safe: every call site
// filters a caller's OWN watches (userID is always the authenticated
// requester, never a caller-supplied target), so the only thing this
// bypass affects is whether an admin's OWN watches stay visible to
// themselves — not a cross-user visibility leak the way the SSE
// carve-out has to guard against.
func (s *Server) computeWatchAccessVisibility(user *models.User, workspaceID string) watchAccessVisibility {
	if user.Role == "admin" {
		return watchAccessVisibility{fullAccess: true}
	}

	visibleIDs, err := s.store.VisibleCollectionIDs(workspaceID, user.ID)
	if err != nil {
		slog.Warn("watch-access: failed to resolve visible collections, denying all in workspace",
			"workspace_id", workspaceID, "user_id", user.ID, "error", err)
		return watchAccessVisibility{} // fail closed: no collections, no items
	}
	if visibleIDs == nil {
		// nil is VisibleCollectionIDs' sentinel for "unrestricted" —
		// full workspace member with CollectionAccess == "all"/"".
		return watchAccessVisibility{fullAccess: true}
	}

	v := watchAccessVisibility{
		visibleCollIDs: make(map[string]bool, len(visibleIDs)),
	}
	for _, id := range visibleIDs {
		v.visibleCollIDs[id] = true
	}

	// Item-level grants layer on top of the collection set — a guest or
	// a "specific"-access member may see individual items outside it
	// (mirrors computeSSEVisibility's needsItemFilter / grantedItemSet).
	// A failure here does NOT widen or narrow the collection-level
	// result above; it only means no item-level grants get layered on.
	_, grantedItemIDs, gerr := s.store.GuestVisibleResources(workspaceID, user.ID)
	if gerr != nil {
		slog.Warn("watch-access: failed to resolve item grants, no item-level widening",
			"workspace_id", workspaceID, "user_id", user.ID, "error", gerr)
		return v
	}
	if len(grantedItemIDs) > 0 {
		v.grantedItemIDs = make(map[string]bool, len(grantedItemIDs))
		for _, id := range grantedItemIDs {
			v.grantedItemIDs[id] = true
		}
	}
	return v
}
