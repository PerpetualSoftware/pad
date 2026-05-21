package server

import (
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/collections"
)

// handleConventionLibrary returns the global convention library.
//
// Query params (all optional):
//   - category=<name> — return only the matching category. Case-sensitive
//     exact match against LibraryCategory.Name. Unknown categories return
//     an empty Categories slice (NOT 404 — the library itself exists; the
//     filter just produced no rows). TASK-1561 / PLAN-1560.
//
// No workspace context — the library is global content.
func (s *Server) handleConventionLibrary(w http.ResponseWriter, r *http.Request) {
	type response struct {
		Categories []collections.LibraryCategory `json:"categories"`
	}

	cats := collections.ConventionLibrary()
	if category := r.URL.Query().Get("category"); category != "" {
		filtered := make([]collections.LibraryCategory, 0, 1)
		for _, cat := range cats {
			if cat.Name == category {
				filtered = append(filtered, cat)
			}
		}
		cats = filtered
	}

	writeJSON(w, http.StatusOK, response{Categories: cats})
}
