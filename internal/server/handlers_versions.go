package server

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	dmp "github.com/sergi/go-diff/diffmatchpatch"

	"github.com/xarmian/pad/internal/models"
)

func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request) {
	_, doc, ok := s.getWorkspaceDocument(w, r)
	if !ok {
		return
	}

	// Resolve diffs so API consumers always get full content
	versions, err := s.store.ListVersionsResolved(doc.ID, doc.Content)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if versions == nil {
		versions = []models.Version{}
	}

	writeJSON(w, http.StatusOK, versions)
}

func (s *Server) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	_, doc, ok := s.getWorkspaceDocument(w, r)
	if !ok {
		return
	}

	versionID := chi.URLParam(r, "versionID")
	// Resolve diffs to return full content
	version, err := s.store.GetVersionResolved(versionID, doc.ID, doc.Content)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if version == nil {
		writeError(w, http.StatusNotFound, "not_found", "Version not found")
		return
	}

	writeJSON(w, http.StatusOK, version)
}

func (s *Server) handleGetDiff(w http.ResponseWriter, r *http.Request) {
	_, doc, ok := s.getWorkspaceDocument(w, r)
	if !ok {
		return
	}

	// Use resolved versions so diffs work correctly
	versions, err := s.store.ListVersionsResolved(doc.ID, doc.Content)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	var oldContent, newContent string

	fromID := r.URL.Query().Get("from")
	toID := r.URL.Query().Get("to")
	lastN := r.URL.Query().Get("last")

	if fromID != "" && toID != "" {
		// Diff between two specific versions (already resolved)
		fromVersion, err := s.store.GetVersionResolved(fromID, doc.ID, doc.Content)
		if err != nil || fromVersion == nil {
			writeError(w, http.StatusNotFound, "not_found", "From version not found")
			return
		}
		toVersion, err := s.store.GetVersionResolved(toID, doc.ID, doc.Content)
		if err != nil || toVersion == nil {
			writeError(w, http.StatusNotFound, "not_found", "To version not found")
			return
		}
		oldContent = fromVersion.Content
		newContent = toVersion.Content
	} else if lastN != "" {
		n, err := strconv.Atoi(lastN)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "bad_request", "Invalid 'last' parameter")
			return
		}
		if n > len(versions) {
			n = len(versions)
		}
		if len(versions) == 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "No versions available")
			return
		}
		// versions are sorted DESC, so versions[0] is most recent
		// Compare version at index n-1 to current document content
		oldContent = versions[n-1].Content
		newContent = doc.Content
	} else {
		// Default: compare most recent version to current
		if len(versions) == 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "No versions available")
			return
		}
		oldContent = versions[0].Content
		newContent = doc.Content
	}

	// Compute diff
	differ := dmp.New()
	diffs := differ.DiffMain(oldContent, newContent, true)
	patches := differ.PatchMake(oldContent, diffs)
	patchText := differ.PatchToText(patches)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"diff":    patchText,
		"changes": diffsToChanges(diffs),
	})
}

type diffChange struct {
	Type  string `json:"type"` // "equal", "insert", "delete"
	Value string `json:"value"`
}

func diffsToChanges(diffs []dmp.Diff) []diffChange {
	changes := make([]diffChange, len(diffs))
	for i, d := range diffs {
		var t string
		switch d.Type {
		case dmp.DiffEqual:
			t = "equal"
		case dmp.DiffInsert:
			t = "insert"
		case dmp.DiffDelete:
			t = "delete"
		}
		changes[i] = diffChange{Type: t, Value: d.Text}
	}
	return changes
}
