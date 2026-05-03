package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
)

func normalizeWorkspaceInput(input *models.WorkspaceCreate) error {
	if input == nil {
		return nil
	}

	settings, err := models.NormalizeWorkspaceSettings(input.Settings)
	if err != nil {
		return fmt.Errorf("invalid settings JSON: %w", err)
	}
	if input.Context != nil {
		settings, err = models.ApplyWorkspaceContext(settings, input.Context)
		if err != nil {
			return fmt.Errorf("invalid workspace context: %w", err)
		}
	}
	input.Settings = settings
	return nil
}

func normalizeWorkspaceUpdateInput(input *models.WorkspaceUpdate) error {
	if input == nil {
		return nil
	}

	if input.Settings != nil {
		settings, err := models.NormalizeWorkspaceSettings(*input.Settings)
		if err != nil {
			return fmt.Errorf("invalid settings JSON: %w", err)
		}
		input.Settings = &settings
	}

	if input.Context != nil {
		base := "{}"
		if input.Settings != nil {
			base = *input.Settings
		}
		settings, err := models.ApplyWorkspaceContext(base, input.Context)
		if err != nil {
			return fmt.Errorf("invalid workspace context: %w", err)
		}
		input.Settings = &settings
	}

	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{"status": "ok"}
	if s.version != "" {
		resp["version"] = s.version
	}
	if s.commit != "" {
		resp["commit"] = s.commit
	}
	if s.buildTime != "" {
		resp["build_time"] = s.buildTime
	}
	resp["cloud_mode"] = s.cloudMode
	writeJSON(w, http.StatusOK, resp)
}

// handleHealthLive is a lightweight liveness probe — always returns 200 if the
// process is running. Kubernetes uses this to decide whether to restart the pod.
func (s *Server) handleHealthLive(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleHealthReady is a readiness probe — returns 200 only when the service
// can accept traffic (DB connection healthy). Kubernetes uses this to decide
// whether to route traffic to the pod.
func (s *Server) handleHealthReady(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not ready",
			"error":  "database unavailable",
		})
		return
	}

	resp := map[string]interface{}{
		"status": "ready",
	}

	// Include connection pool stats (useful for debugging, not required for pass/fail).
	dbStats := s.store.DB().Stats()
	resp["db"] = map[string]interface{}{
		"open_connections": dbStats.OpenConnections,
		"in_use":           dbStats.InUse,
		"idle":             dbStats.Idle,
		"driver":           string(s.store.D().Driver()),
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	type templateInfo struct {
		Name        string   `json:"name"`
		Category    string   `json:"category"`
		Description string   `json:"description"`
		Icon        string   `json:"icon"`
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
			Category:    t.Category,
			Description: t.Description,
			Icon:        t.Icon,
			Collections: colls,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)

	// Authenticated users — including admins — see only workspaces they're
	// a member of (which includes ones they own, since owners get a
	// workspace_members row at creation time). Server admins previously got
	// the unfiltered list here, which leaked workspace metadata into their
	// "shared with me" switcher even though they weren't members
	// (BUG-982). Cross-tenant visibility for admins is available through
	// the admin panel routes (/api/v1/admin/...), which call
	// ListWorkspaces() directly with the appropriate auth gate.
	if user != nil {
		workspaces, err := s.store.GetUserWorkspaces(user.ID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if workspaces == nil {
			workspaces = []models.Workspace{}
		}
		writeJSON(w, http.StatusOK, workspaces)
		return
	}

	// Pre-auth / fresh-install bootstrap: list everything so the setup
	// flow can find any seeded workspace.
	workspaces, err := s.store.ListWorkspaces()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if workspaces == nil {
		workspaces = []models.Workspace{}
	}
	writeJSON(w, http.StatusOK, workspaces)
}

func (s *Server) handleReorderWorkspaces(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}

	var input []struct {
		Slug      string `json:"slug"`
		SortOrder int    `json:"sort_order"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	for _, item := range input {
		ws, err := s.store.GetWorkspaceBySlug(item.Slug)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if ws == nil {
			continue
		}
		// Skip silently if user is not a member of this workspace
		// (e.g. admin sees all workspaces but may not be joined to all)
		if err := s.store.UpdateWorkspaceSortOrder(userID, ws.ID, item.SortOrder); err != nil {
			if err == sql.ErrNoRows {
				continue
			}
			writeInternalError(w, err)
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
	if err := normalizeWorkspaceInput(&input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// Set owner to the authenticated user
	if userID := currentUserID(r); userID != "" {
		input.OwnerID = userID
	}

	// Enforce workspace count limit (user-scoped)
	if userID := currentUserID(r); userID != "" {
		if !s.enforceUserPlanLimit(w, userID, "workspaces") {
			return
		}
	}

	ws, err := s.store.CreateWorkspace(input)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Seed collections for the new workspace using the requested template
	if err := s.store.SeedCollectionsFromTemplate(ws.ID, input.Template); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Workspace created but failed to seed collections: "+err.Error())
		return
	}

	// Add the creator as workspace owner
	if userID := currentUserID(r); userID != "" {
		_ = s.store.AddWorkspaceMember(ws.ID, userID, "owner")
	}

	writeJSON(w, http.StatusCreated, ws)
}

func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.getWorkspace(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) handleUpdateWorkspace(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	existing, ok := s.getWorkspace(w, r)
	if !ok {
		return
	}

	var input models.WorkspaceUpdate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := normalizeWorkspaceUpdateInput(&input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	ws, err := s.store.UpdateWorkspace(existing.Slug, input)
	if err != nil {
		writeInternalError(w, err)
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
	if !requireMinRole(w, r, "owner") {
		return
	}
	ws, ok := s.getWorkspace(w, r)
	if !ok {
		return
	}
	err := s.store.DeleteWorkspace(ws.Slug)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Workspace not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleExportWorkspace(w http.ResponseWriter, r *http.Request) {
	// `?format=tar` switches to the tar.gz bundle that includes
	// attachment blobs (TASK-884). Default stays JSON for backward
	// compat — existing automation hitting this endpoint without a
	// query param keeps working unchanged. The CLI's
	// `pad workspace export` opts into the bundle by default.
	if strings.EqualFold(r.URL.Query().Get("format"), "tar") {
		s.handleExportWorkspaceBundle(w, r)
		return
	}

	if !requireMinRole(w, r, "owner") {
		return
	}
	ws, ok := s.getWorkspace(w, r)
	if !ok {
		return
	}
	export, err := s.store.ExportWorkspace(ws.Slug)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-export.json"`, ws.Slug))
	writeJSON(w, http.StatusOK, export)
}

func (s *Server) handleImportWorkspace(w http.ResponseWriter, r *http.Request) {
	// Content-Type dispatch:
	//   application/gzip / application/x-gzip / application/x-tar
	//     → tar.gz bundle path (TASK-885) — handles attachments.
	//   anything else → JSON path (legacy items-only).
	//
	// We prefer Content-Type over file-magic sniffing so a misnamed
	// upload fails fast with a clear error rather than silently going
	// through the wrong code path. The CLI's pad import command sets
	// the right header based on the file extension; web UI does the
	// same when uploading a .tar.gz.
	ct := strings.TrimSpace(r.Header.Get("Content-Type"))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct == "application/gzip" || ct == "application/x-gzip" || ct == "application/x-tar" {
		s.handleImportWorkspaceBundle(w, r)
		return
	}

	var data models.WorkspaceExport
	// WorkspaceExport contains all collections, items, comments, and item
	// versions for the workspace — even a modest project export blows past
	// the default 2 MiB decodeJSON cap. 64 MiB is well above any realistic
	// single-workspace backup while still far from the heap-exhaustion
	// range the default cap protects against.
	if err := decodeJSONWithLimit(r, &data, 64<<20); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid export data: "+err.Error())
		return
	}

	// Optional: override workspace name via query param
	newName := r.URL.Query().Get("name")

	// Set the authenticated user as owner so the imported workspace is
	// accessible and has correct owner_username for URL routing.
	userID := currentUserID(r)
	ws, err := s.store.ImportWorkspace(&data, newName, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "import_failed", err.Error())
		return
	}

	// Add the importer as workspace owner (mirrors handleCreateWorkspace)
	if userID != "" {
		_ = s.store.AddWorkspaceMember(ws.ID, userID, "owner")
	}

	writeJSON(w, http.StatusCreated, ws)
}
