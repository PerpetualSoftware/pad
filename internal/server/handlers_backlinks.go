package server

import (
	"net/http"
	"strconv"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/go-chi/chi/v5"
)

// handleGetItemBacklinks serves
// `GET /api/v1/workspaces/{ws}/items/{itemSlug}/backlinks` — the
// REST surface for PLAN-1593's reverse `[[...]]` index. Returns a
// JSON array of Backlink rows, ordered most-recently-updated source
// first.
//
// Auth + scoping:
//   - Workspace lookup follows the standard middleware-resolved path
//     (getWorkspaceID).
//   - The TARGET item must be visible to the requester (visibility
//     check via requireItemVisible). If the requester can't see the
//     target, they don't get to know who links to it.
//   - SOURCE items are filtered IN SQL using the precise
//     `(fullCollIDs, grantedItemIDs)` shape returned by
//     guestResourceFilter. Pagination correctness depends on this:
//     filtering AFTER the fetch would let hidden rows consume LIMIT
//     slots and shrink pages silently — Codex round-1 P1 and
//     round-2 P1 both flagged variants of that bug.
//
// Visibility model passed to the store:
//   - admin or full-access member          → Unrestricted=true
//   - guest or restricted member with grants → the (fullColl, grantedItem)
//     lists from guestResourceFilter; the store builds
//     `AND (s.collection_id IN <full> OR s.id IN <granted>)`
//
// Pagination:
//   - `?limit=N` (1–300, default 50). Out-of-range values are
//     normalized in the store layer.
//   - `?offset=N` (>=0). Skipped page lets the UI / CLI build a
//     paged "see more" affordance.
//
// PLAN-1593 / TASK-1594.
func (s *Server) handleGetItemBacklinks(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItem(workspaceID, itemSlug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return
	}

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	// Normalize at the handler boundary so the same-ws/cross-ws
	// split math agrees with the store layer's internal clamping.
	// Without this, `?limit=301` would let the handler compute
	// `remaining = 301 - len(sameWs)` while the store fetched only
	// 50 same-ws rows — same-ws tier exhausted at row 50 but the
	// handler thinks it has 251 same-ws slots left, so the cross-
	// ws slice starts incorrectly. Codex round 3 P2.
	if limit > 300 {
		limit = 300
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	// Resolve the precise visibility primitives. nil/nil ↦
	// Unrestricted (admin / full-access member / root token);
	// non-nil ↦ restricted user, build the SQL filter from the
	// exact (full-coll, granted-item) lists.
	fullCollIDs, grantedItemIDs, gErr := s.guestResourceFilter(r, workspaceID)
	if gErr != nil {
		writeInternalError(w, gErr)
		return
	}
	vis := store.BacklinksVisibility{Unrestricted: fullCollIDs == nil && grantedItemIDs == nil}
	if !vis.Unrestricted {
		vis.FullCollectionIDs = fullCollIDs
		vis.GrantedItemIDs = grantedItemIDs
	}
	// Parent↔child suppression is read entirely from item_links
	// inside the store layer (TASK-1607 follow-up — the initial
	// fix routed through items.parent_id which is empty in
	// production because SetParentLink only writes to item_links).
	// The handler no longer needs to plumb parent context through.

	// Union pagination across same-ws + cross-ws tiers.
	//
	// Tier order: same-workspace first, then cross-workspace —
	// matches the renderer's UI mental model (your own workspace's
	// backlinks at the top of the panel, foreign workspaces below).
	// Within each tier, ordering is updated_at DESC.
	//
	// Pagination strategy: count same-ws results so the handler
	// knows where the cross-ws tier begins. Splits the requested
	// (limit, offset) window into a same-ws slice and a cross-ws
	// slice. Computing the count up-front lets pages 2+ correctly
	// land in the cross-ws tier when same-ws is exhausted.
	sameCount, err := s.store.CountBacklinks(item.ID, workspaceID, vis)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	var sameWs []models.Backlink
	var crossWs []models.Backlink

	// Same-ws slice: offset/limit relative to the same-ws tier.
	if offset < sameCount {
		sameLimit := limit
		if offset+sameLimit > sameCount {
			sameLimit = sameCount - offset
		}
		if sameLimit > 0 {
			sameWs, err = s.store.GetBacklinks(item.ID, workspaceID, sameLimit, offset, vis)
			if err != nil {
				writeInternalError(w, err)
				return
			}
		}
	}

	// Cross-ws slice: only fetched when there's room left in the
	// page after same-ws fills (or when offset already exceeded
	// same-ws). Cross-ws target ref needs the canonical
	// PREFIX-NUMBER shape derived from the target item.
	remaining := limit - len(sameWs)
	if remaining > 0 && item.CollectionPrefix != "" && item.ItemNumber != nil {
		// crossOffset is the offset INTO the cross-ws tier — zero
		// when same-ws filled some of the current page, or
		// (offset - sameCount) when the offset already passed the
		// same-ws tier entirely.
		crossOffset := 0
		if offset > sameCount {
			crossOffset = offset - sameCount
		}
		targetRef := item.CollectionPrefix + "-" + strconv.Itoa(*item.ItemNumber)
		// Use currentUser from the request — guaranteed non-nil
		// here because the middleware required workspace access.
		user := currentUser(r)
		if user != nil {
			// Propagate the OAuth/MCP token's workspace allow-list
			// (TASK-952) into the cross-ws enumeration. nil → no
			// token gate (PAT or pre-consent token, allow all
			// enumerated workspaces); a slice gates each source
			// workspace by slug. Without this, an MCP token
			// consented for workspace A could leak backlinks from
			// workspace B that the user has independent access to.
			// Codex round 2 P1.
			allowedSlugs := TokenAllowedWorkspacesFromContext(r.Context())
			crossWs, err = s.store.GetCrossWorkspaceBacklinks(workspaceID, targetRef, user.ID, allowedSlugs, remaining, crossOffset)
			if err != nil {
				writeInternalError(w, err)
				return
			}
		}
	}

	combined := append(sameWs, crossWs...)
	if combined == nil {
		// Always emit a JSON array, never null — easier for
		// clients to consume without special-casing.
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, combined)
}
