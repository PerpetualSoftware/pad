package server

import (
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/collections"
)

func (s *Server) handleConventionLibrary(w http.ResponseWriter, r *http.Request) {
	type response struct {
		Categories []collections.LibraryCategory `json:"categories"`
	}
	writeJSON(w, http.StatusOK, response{
		Categories: collections.ConventionLibrary(),
	})
}
