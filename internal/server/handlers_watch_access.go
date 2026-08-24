package server

import (
	"log/slog"
	"net/http"

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
//
// r supplies the bearer-vs-cookie admin check (TASK-2533 codex round 2
// finding 2) — see computeWatchAccessVisibility's doc comment for why
// this call site can no longer treat "userID is always the caller" as a
// reason to skip it. BUG-2725 replaced the *http.Request parameter on
// that function with a plain bool, so the transport is read here, once.
func (s *Server) filterWatchesByCurrentAccess(r *http.Request, userID string, watches []models.Watch) []models.Watch {
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

	// One transport read for the whole call: r is a single request, so
	// every watch it filters shares its auth transport.
	bearerAuth := isBearerAuth(r)

	out := make([]models.Watch, 0, len(watches))
	for workspaceID, wsWatches := range byWorkspace {
		// Error deliberately discarded: this caller's failure mode is the
		// one computeWatchAccessVisibility's default serves. A store blip
		// yields the deny-everything value, which drops the watches for
		// that workspace for this call — the correct trade here, since
		// delivering a fact to someone whose access could not be confirmed
		// is worse than a briefly quiet nudge stream. The error exists for
		// deliveredSessionCount, whose 0 skips a publish.
		vis, _ := s.computeWatchAccessVisibility(bearerAuth, user, workspaceID)
		for _, w := range wsWatches {
			if vis.allows(w.ItemCollectionID, w.ItemID) {
				out = append(out, w)
			}
		}
	}
	return out
}

// watchAccessVisibility is filterWatchesByCurrentAccess's (and, as of
// codex round 2 finding 2, the addressed-delivery check's) per-workspace
// visibility snapshot. Deliberately keyed by ID (collection ID / item
// ID), unlike handlers_events.go's sseVisibility, which is keyed by
// collection SLUG because it routes live workspace-scoped events that
// only carry a slug — models.Watch and watchevents.Notification both
// carry collection/item IDs directly, so ID-keying here avoids an extra
// slug round trip per collection.
type watchAccessVisibility struct {
	// fullAccess is true for admin (bearer-vs-cookie gated, see
	// computeWatchAccessVisibility) or a member with unrestricted
	// collection access (CollectionAccess != "specific"). true means "no
	// filtering" — every notification in this workspace passes.
	fullAccess bool
	// visibleCollIDs is the set of collection IDs the caller GENUINELY has
	// full access to — a member's own assigned collections, system
	// collections, or a guest's direct collection_grants (nil/empty when
	// fullAccess is true or the caller has none). Deliberately NOT built
	// from VisibleCollectionIDs/GuestVisibleCollectionIDs (TASK-2533
	// codex round 2 finding 1) — those over-widen for navigation,
	// including a collection ID merely because the caller has an item
	// grant somewhere inside it, which is exactly the leak this field
	// must not reproduce. Built from GuestVisibleResources'
	// fullCollectionIDs (+ GetMemberCollectionAccess/
	// ListSystemCollectionIDs for a member) instead — see
	// computeWatchAccessVisibility.
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
// one workspace, built from the SAME primitives computeSSEVisibility
// uses (handlers_events.go) and the SAME two-tier construction, not an
// approximation of it.
//
// TASK-2533 codex round 2 finding 1 (confirmed real, fixed): the
// previous version used VisibleCollectionIDs/GuestVisibleCollectionIDs
// directly as the "fully visible" gate. Those functions deliberately
// OVER-WIDEN for navigation purposes — GuestVisibleCollectionIDs' own
// query adds a collection's ID to the result if the caller has an item
// grant on ANY item inside it (so a guest's sidebar can show the
// collection at all), explicitly leaving item-level narrowing to the
// caller. allows() never did that narrowing: a guest granted item A got
// treated as having full access to A's whole collection, including
// sibling item B they were never granted. Fixed by using
// GuestVisibleResources' fullCollectionIDs return (populated ONLY from
// direct collection_grants — never widened by an item grant) as the
// "genuinely full access" set, exactly like computeSSEVisibility's own
// fullCollSet construction, with GetMemberCollectionAccess +
// ListSystemCollectionIDs layered on for an actual workspace member.
//
// Admin bypass (TASK-2533 codex round 2 finding 2 — the argument below
// REPLACES a now-falsified one, not just a code fix): this used to treat
// ANY admin as full-access regardless of auth transport, reasoned as
// "every call site filters a caller's OWN watches, so this can't leak
// another user's data." That reasoning held for watch-matched
// notifications, but finding 2 is exactly the case where it doesn't:
// the addressed-delivery path is now ALSO gated by this
// function (handlers_watch_events.go), and an addressed
// notification is, by definition, about something aimed AT the
// connected user in a workspace — a bearer-borne admin token (a leaked
// PAT, a compromised CI credential) being unconditionally trusted to see
// EVERY workspace's addressed traffic for "themselves" is exactly the
// blast-radius BUG-1616 exists to bound. There is no watch-specific
// exemption from that reasoning; this now mirrors computeSSEVisibility's
// bearer-vs-cookie distinction exactly, so admin access to watch/nudge
// delivery has no bespoke ACL argument of its own left to defend.
// BEARER-VS-COOKIE IS THE ONLY PER-CONNECTION INPUT, and BUG-2725 is why
// it arrives as a bool rather than as the *http.Request it used to be.
//
// Every other input below is per-USER: platform role, workspace
// membership, CollectionAccess, guest grants, system collections. They
// resolve identically for every stream a given user holds. `bearerAuth`
// is the single exception — it is a property of the individual
// connection, fixed when that connection opened.
//
// That distinction is load-bearing for deliveredSessionCount
// (handlers_push.go), which must reproduce this predicate for sessions
// it is not serving. With an *http.Request parameter it could only ever
// pass the PUSHER's request, silently answering for the wrong
// connection. With a bool it can pass each target session's own
// recorded transport — and, because this is the only varying input, it
// needs AT MOST TWO resolutions per workspace no matter how many
// sessions it counts. That bound is what retires the "N access checks
// per push" cost objection permanently; it is enforced structurally in
// deliveredSessionCount rather than argued.
// THE ERROR RETURN IS NOT DECORATION (codex round 2, P1 — the same
// class round 1 caught one layer up). Every store failure below used to
// be collapsed into an empty, deny-everything visibility. For the SSE
// stream that is correct and stays the default: delivering a fact to
// someone whose access could not be confirmed is the worse outcome
// there, and that caller discards this error deliberately.
//
// deliveredSessionCount is a second caller with the OPPOSITE
// consequence. Its 0 makes a targeted push SKIP the publish, so a DB
// outage silently collapsing to "not visible" drops the instruction and
// answers 200 — BUG-2698's defect, which round 1 removed from the
// GetUser lookup and which lived on in all four store calls here.
//
// So the resolution and the POLICY are separated: this function reports
// what happened, and each caller applies the disposition its own failure
// mode calls for. The visibility returned alongside a non-nil error is
// always the deny-everything value, so a caller that ignores the error
// keeps exactly the old behaviour.
//
// Population (CONVE-18): four store calls in this function could fail —
// GetWorkspaceMember, GuestVisibleResources, GetMemberCollectionAccess,
// ListSystemCollectionIDs. All four now report. The last two were not
// merely swallowed but discarded into `_`, which is how they escaped
// round 1's sweep. Search boundary: this function only; the sibling
// resolver computeSSEVisibility (handlers_events.go) has the same shape
// and was NOT changed, because no counting caller reads it.
func (s *Server) computeWatchAccessVisibility(bearerAuth bool, user *models.User, workspaceID string) (watchAccessVisibility, error) {
	if user.Role == "admin" && !bearerAuth {
		return watchAccessVisibility{fullAccess: true}, nil
	}

	member, err := s.store.GetWorkspaceMember(workspaceID, user.ID)
	if err != nil {
		slog.Warn("watch-access: failed to resolve workspace membership, denying all in workspace",
			"workspace_id", workspaceID, "user_id", user.ID, "error", err)
		return watchAccessVisibility{}, err
	}
	if member != nil && member.CollectionAccess != "specific" {
		// "all" access (or the legacy "" default) — unrestricted. Note
		// this fires for a bearer-borne admin too, exactly like
		// computeSSEVisibility: a bearer admin who is ALSO a genuine full
		// member of this specific workspace legitimately sees it in
		// full; the gate above only denies the platform-wide, not-a-
		// member-here bypass.
		return watchAccessVisibility{fullAccess: true}, nil
	}

	// From here: either a "specific"-access member, a non-member guest,
	// or a bearer-borne admin with no membership/grants in this
	// workspace at all — all three need the fullCollSet/grantedItemSet
	// distinction GuestVisibleResources provides, unlike
	// VisibleCollectionIDs.
	fullCollIDs, grantedItemIDs, gerr := s.store.GuestVisibleResources(workspaceID, user.ID)
	if gerr != nil {
		slog.Warn("watch-access: failed to resolve grants, denying all in workspace",
			"workspace_id", workspaceID, "user_id", user.ID, "error", gerr)
		return watchAccessVisibility{}, gerr
	}

	fullCollSet := make(map[string]bool, len(fullCollIDs))
	for _, id := range fullCollIDs {
		fullCollSet[id] = true
	}
	if member != nil {
		// A "specific"-access member's OWN assigned collections + system
		// collections are genuinely fully visible — mirrors
		// computeSSEVisibility exactly. A pure guest (member == nil) has
		// no such assignment; fullCollSet stays limited to their direct
		// collection_grants from GuestVisibleResources above.
		memberColls, mcErr := s.store.GetMemberCollectionAccess(workspaceID, user.ID)
		if mcErr != nil {
			slog.Warn("watch-access: failed to resolve member collection access, denying all in workspace",
				"workspace_id", workspaceID, "user_id", user.ID, "error", mcErr)
			return watchAccessVisibility{}, mcErr
		}
		sysColls, scErr := s.store.ListSystemCollectionIDs(workspaceID)
		if scErr != nil {
			slog.Warn("watch-access: failed to list system collections, denying all in workspace",
				"workspace_id", workspaceID, "error", scErr)
			return watchAccessVisibility{}, scErr
		}
		for _, id := range memberColls {
			fullCollSet[id] = true
		}
		for _, id := range sysColls {
			fullCollSet[id] = true
		}
	}

	grantedSet := make(map[string]bool, len(grantedItemIDs))
	for _, id := range grantedItemIDs {
		grantedSet[id] = true
	}

	return watchAccessVisibility{
		visibleCollIDs: fullCollSet,
		grantedItemIDs: grantedSet,
	}, nil
}
