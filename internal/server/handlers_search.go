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

	results, err := s.store.Search(params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
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
