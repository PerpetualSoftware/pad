package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "viewer") {
		return
	}
	_, doc, ok := s.getWorkspaceDocument(w, r)
	if !ok {
		return
	}

	// Resolve diffs so API consumers always get full content, from the body
	// read in the same snapshot as the versions (BUG-3833), not doc.Content.
	versions, err := s.store.ListVersionsResolvedSnapshot(r.Context(), doc.ID)
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
	// Resolve diffs to return full content, in one snapshot (BUG-3833).
	version, err := s.store.GetVersionResolvedSnapshot(r.Context(), versionID, doc.ID)
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
