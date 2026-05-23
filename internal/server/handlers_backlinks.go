package server

import (
	"net/http"
	"strconv"

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

	backlinks, err := s.store.GetBacklinks(item.ID, workspaceID, limit, offset, vis)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if backlinks == nil {
		// Always emit a JSON array, never null — easier for
		// clients to consume without special-casing.
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, backlinks)
}
