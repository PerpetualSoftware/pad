package server

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// handleGetItemRelationBacklinks answers "which items point at this one
// through a relation field?" (PLAN-2857 U5 / TASK-2997).
//
// Separate from handleGetItemBacklinks rather than folded into it. The two
// answer different questions — a `[[wiki-link]]` in prose is not a typed
// field-valued edge — and they have different shapes: a relation backlink
// carries the FIELD KEY it points through and has no snippet, because a field
// value has no surrounding text to quote. Merging them would mean a response
// where half the entries have a snippet and half have a field key, and every
// consumer branching on which.
//
// THE COUNT IS THE VIEWER'S (ratified, day 55). Visibility comes from the same
// guestResourceFilter the wiki backlinks use, so "referenced by 3" means "by 3
// you can see". A true count would leak the existence of items the viewer has
// no access to.
//
// No cross-workspace tier, unlike wiki backlinks: a relation value names an
// item id, and PLAN-2857 v1 excludes cross-workspace relation targets — a
// carried value is dropped at the boundary rather than resolved. There is
// nothing foreign to page through.
func (s *Server) handleGetItemRelationBacklinks(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	// IncludeDeleted, matching the wiki handler: a soft-deleted item still has
	// a page, and "who pointed at this?" is a question that outlives it.
	item, err := s.store.ResolveItemIncludeDeleted(workspaceID, itemSlug)
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
	if limit > 300 {
		limit = 300
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

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

	total, err := s.store.CountRelationBacklinks(item.ID, workspaceID, vis)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	links, err := s.store.GetRelationBacklinks(item.ID, workspaceID, limit, offset, vis)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if links == nil {
		links = []store.RelationBacklink{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"relation_backlinks": links,
		// `total` is what the header renders as "Referenced by N". It is
		// computed under the SAME visibility and the SAME filters as the page,
		// so the header cannot promise a number the list will not produce.
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}
