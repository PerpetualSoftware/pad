package server

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

var workspaceRenameSlug = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func (s *Server) handleRenameWorkspace(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	ws, ok := s.getWorkspace(w, r)
	if !ok {
		return
	}
	var input models.WorkspaceRename
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if strings.TrimSpace(input.ExpectedSlug) == "" || len(input.Slug) > 100 ||
		!workspaceRenameSlug.MatchString(input.Slug) || isUUID(input.Slug) {
		writeError(w, http.StatusBadRequest, "bad_request", "Supply expected_slug and a slug of up to 100 lowercase letters, digits and single hyphens")
		return
	}
	switch input.Slug {
	case "deleted", "import", "import-status", "reorder":
		writeError(w, http.StatusBadRequest, "bad_request", "That workspace slug is reserved")
		return
	}
	if input.Name != nil {
		name := strings.TrimSpace(*input.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "Workspace name cannot be empty")
			return
		}
		input.Name = &name
	}
	renamed, changed, err := s.store.RenameWorkspace(ws.ID, input.ExpectedSlug, input.Slug, input.Name)
	switch {
	case errors.Is(err, store.ErrWorkspaceSlugConflict):
		writeError(w, http.StatusConflict, "workspace_slug_taken", "That workspace slug is already claimed")
		return
	case errors.Is(err, store.ErrWorkspaceRenameStale):
		writeError(w, http.StatusConflict, "workspace_rename_stale", "The workspace slug changed. Use its current slug to rename it.")
		return
	case err != nil:
		writeStoreErrorR(w, r, err, http.StatusInternalServerError, "internal_error", "Failed to rename workspace")
		return
	case renamed == nil:
		writeWorkspaceNotFound(w, "Workspace not found")
		return
	}
	if changed {
		s.logWorkspaceAuditEvent(renamed.ID, "workspace.renamed", r, auditMeta(map[string]string{
			"previous_slug": input.ExpectedSlug, "slug": renamed.Slug,
		}))
		s.publishEvent(events.WorkspaceUpdated, renamed.ID, "", renamed.Name, "", "", "")
	}
	writeJSON(w, http.StatusOK, renamed)
}
