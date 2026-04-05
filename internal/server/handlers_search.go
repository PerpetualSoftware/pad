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
		}
		// If no user (fresh install, no auth), allow unscoped search
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
