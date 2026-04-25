package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/xarmian/pad/internal/models"
)

func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "viewer") {
		return
	}
	_, doc, ok := s.getWorkspaceDocument(w, r)
	if !ok {
		return
	}

	// Resolve diffs so API consumers always get full content
	versions, err := s.store.ListVersionsResolved(doc.ID, doc.Content)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if versions == nil {
		versions = []models.Version{}
	}

	writeJSON(w, http.StatusOK, versions)
}

func (s *Server) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "viewer") {
		return
	}
	_, doc, ok := s.getWorkspaceDocument(w, r)
	if !ok {
		return
	}

	versionID := chi.URLParam(r, "versionID")
	// Resolve diffs to return full content
	version, err := s.store.GetVersionResolved(versionID, doc.ID, doc.Content)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if version == nil {
		writeError(w, http.StatusNotFound, "not_found", "Version not found")
		return
	}

	writeJSON(w, http.StatusOK, version)
}
