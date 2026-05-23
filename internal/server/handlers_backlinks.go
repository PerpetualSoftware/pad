package server

import (
	"net/http"
	"strconv"

	"github.com/PerpetualSoftware/pad/internal/models"
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
//   - SOURCE items are filtered by per-row collection visibility in
//     SQL (via visibleCollectionIDs → GetBacklinks). Filtering in
//     SQL — not post-fetch — is essential for correct pagination:
//     the LIMIT/OFFSET counts visible rows, not raw rows. Otherwise
//     a restricted user asking for limit=50 could receive an empty
//     page even when later visible backlinks exist (Codex round-1
//     P1). Item-level guest grants apply on top of the collection
//     gate; they're rare enough that the residual handler-side trim
//     doesn't materially shrink pages.
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

	// Visibility gate. nil → no restriction (owner/editor/root tokens);
	// non-empty → SQL-side filter for SOURCE collection visibility.
	// Passing this into the store query (not filtering after the
	// fetch) is what makes the LIMIT/OFFSET pagination correct for
	// restricted users — see the function-level comment above and
	// Codex round-1 P1.
	visibleIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
	if visErr != nil {
		writeInternalError(w, visErr)
		return
	}

	backlinks, err := s.store.GetBacklinks(item.ID, workspaceID, limit, offset, visibleIDs)
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

	// Item-level guest grants apply on top of the SQL collection
	// gate. These are item-by-item ACL overrides; pushing them into
	// SQL would mean expanding the WHERE clause per request, which
	// isn't worth it for an N≤50 page. For the common authenticated
	// case (no guest grants active) this loop runs N times and drops
	// nothing.
	fullCollIDs, grantedItemIDs, grantErr := s.guestResourceFilter(r, workspaceID)
	if grantErr != nil {
		writeInternalError(w, grantErr)
		return
	}
	if visibleIDs != nil {
		filtered := backlinks[:0]
		for _, bl := range backlinks {
			src, err := s.store.GetItem(bl.SourceItemID)
			if err != nil || src == nil {
				continue
			}
			if !s.isItemVisibleToGuest(r, workspaceID, src, fullCollIDs, grantedItemIDs) {
				continue
			}
			filtered = append(filtered, bl)
		}
		backlinks = filtered
	}

	// Defensive: never return null even after the filter loop
	// emptied the slice.
	if backlinks == nil {
		backlinks = []models.Backlink{}
	}
	writeJSON(w, http.StatusOK, backlinks)
}
