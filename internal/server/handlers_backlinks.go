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
//   - SOURCE items are filtered by per-row collection visibility +
//     guest grants — a backlink from a hidden collection or a
//     guest-walled item is dropped so this endpoint can't leak the
//     existence of unviewable items via the snippet field.
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

	backlinks, err := s.store.GetBacklinks(item.ID, workspaceID, limit, offset)
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

	// Per-source visibility filter. The store-level query already
	// gates source.deleted_at IS NULL and excludes self-links, but
	// it has no view into the requester's permissions — that's
	// strictly an HTTP-layer concern.
	visibleIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
	if visErr != nil {
		writeInternalError(w, visErr)
		return
	}
	fullCollIDs, grantedItemIDs, grantErr := s.guestResourceFilter(r, workspaceID)
	if grantErr != nil {
		writeInternalError(w, grantErr)
		return
	}
	if visibleIDs != nil {
		filtered := backlinks[:0]
		for _, bl := range backlinks {
			// Re-resolve the source item just enough to apply
			// the existing visibility helpers. A second lookup
			// per backlink isn't free, but for N≤50 it's well
			// within the latency budget; if this becomes hot
			// we can extend GetBacklinks to return collection_id
			// alongside the slug.
			src, err := s.store.GetItem(bl.SourceItemID)
			if err != nil || src == nil {
				continue
			}
			if !isCollectionVisible(src.CollectionID, visibleIDs) {
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
