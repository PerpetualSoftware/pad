package server

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/xarmian/pad/internal/collections"
	"github.com/xarmian/pad/internal/events"
	"github.com/xarmian/pad/internal/models"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	type templateInfo struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Collections []string `json:"collections"`
	}
	templates := collections.ListTemplates()
	result := make([]templateInfo, 0, len(templates))
	for _, t := range templates {
		colls := make([]string, 0, len(t.Collections))
		for _, c := range t.Collections {
			colls = append(colls, c.Icon+" "+c.Name)
		}
		result = append(result, templateInfo{
			Name:        t.Name,
			Description: t.Description,
			Collections: colls,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	workspaces, err := s.store.ListWorkspaces()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if workspaces == nil {
		workspaces = []models.Workspace{}
	}
	writeJSON(w, http.StatusOK, workspaces)
}

func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	var input models.WorkspaceCreate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Name is required")
		return
	}

	ws, err := s.store.CreateWorkspace(input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Seed collections for the new workspace using the requested template
	if err := s.store.SeedCollectionsFromTemplate(ws.ID, input.Template); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Workspace created but failed to seed collections: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, ws)
}

func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	ws, err := s.store.GetWorkspaceBySlug(slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if ws == nil {
		writeError(w, http.StatusNotFound, "not_found", "Workspace not found")
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) handleUpdateWorkspace(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	var input models.WorkspaceUpdate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	ws, err := s.store.UpdateWorkspace(slug, input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if ws == nil {
		writeError(w, http.StatusNotFound, "not_found", "Workspace not found")
		return
	}

	s.publishEvent(events.WorkspaceUpdated, ws.ID, "", ws.Name, "", "", "")

	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	err := s.store.DeleteWorkspace(slug)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Workspace not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleExportWorkspace(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	export, err := s.store.ExportWorkspace(slug)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-export.json"`, slug))
	writeJSON(w, http.StatusOK, export)
}

func (s *Server) handleImportWorkspace(w http.ResponseWriter, r *http.Request) {
	var data models.WorkspaceExport
	if err := decodeJSON(r, &data); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid export data: "+err.Error())
		return
	}

	// Optional: override workspace name via query param
	newName := r.URL.Query().Get("name")

	ws, err := s.store.ImportWorkspace(&data, newName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "import_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, ws)
}
