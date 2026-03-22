package server

import (
	"net/http"

	"github.com/xarmian/pad/internal/collections"
)

func (s *Server) handlePlaybookLibrary(w http.ResponseWriter, r *http.Request) {
	type response struct {
		Categories []collections.PlaybookCategory `json:"categories"`
	}
	writeJSON(w, http.StatusOK, response{
		Categories: collections.PlaybookLibrary(),
	})
}
