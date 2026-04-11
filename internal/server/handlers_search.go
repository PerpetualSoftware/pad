package server

import (
	"net/http"

	"github.com/xarmian/pad/internal/store"
)

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Query parameter 'q' is required")
		return
	}

	params := store.SearchParams{
		Query:     query,
		Workspace: r.URL.Query().Get("workspace"),
	}

	// When no specific workspace is given, scope search to the user's
	// workspaces so results never leak across workspace boundaries.
	if params.Workspace == "" {
		user := currentUser(r)
		if user != nil {
			workspaces, err := s.store.GetUserWorkspaces(user.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal_error", "Failed to resolve user workspaces")
				return
			}
			for _, ws := range workspaces {
				params.WorkspaceIDs = append(params.WorkspaceIDs, ws.ID)
			}
			// If user has no workspaces, return empty results
			if len(params.WorkspaceIDs) == 0 {
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"results": []store.SearchResult{},
					"total":   0,
				})
				return
			}
			// Apply per-workspace collection visibility filtering.
			// Collect all visible collection IDs across user's workspaces.
			var allVisibleCollIDs []string
			needsCollFilter := false
			for _, ws := range workspaces {
				visIDs, err := s.store.VisibleCollectionIDs(ws.ID, user.ID)
				if err != nil {
					// Fail closed: skip this workspace entirely on error
					// rather than searching it without a collection filter.
					params.WorkspaceIDs = removeString(params.WorkspaceIDs, ws.ID)
					continue
				}
				if visIDs != nil {
					needsCollFilter = true
					allVisibleCollIDs = append(allVisibleCollIDs, visIDs...)
				} else {
					// "all" access — include all collections from this workspace
					colls, _ := s.store.ListCollections(ws.ID)
					for _, c := range colls {
						allVisibleCollIDs = append(allVisibleCollIDs, c.ID)
					}
				}
			}
			if needsCollFilter {
				params.CollectionIDs = allVisibleCollIDs
			}
		}
		// If no user (fresh install, no auth), allow unscoped search
	}

	// Apply collection visibility filter when searching a specific workspace
	if params.Workspace != "" {
		ws, _ := s.store.GetWorkspaceBySlug(params.Workspace)
		if ws != nil {
			visibleIDs, visErr := s.visibleCollectionIDs(r, ws.ID)
			if visErr != nil {
				writeInternalError(w, visErr)
				return
			}
			params.CollectionIDs = visibleIDs
		}
	}

	results, err := s.store.Search(params)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if results == nil {
		results = []store.SearchResult{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results": results,
		"total":   len(results),
	})
}

func removeString(ss []string, s string) []string {
	result := ss[:0]
	for _, v := range ss {
		if v != s {
			result = append(result, v)
		}
	}
	return result
}
