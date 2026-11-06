package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type workspaceGroupView struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	WorkspaceIDs []string `json:"workspace_ids"`
	CreatedAt    string   `json:"created_at,omitempty"`
	UpdatedAt    string   `json:"updated_at,omitempty"`
}

type workspaceGroupBody struct {
	Name         json.RawMessage `json:"name"`
	WorkspaceIDs json.RawMessage `json:"workspace_ids"`
}

func workspaceGroupCaller(w http.ResponseWriter, r *http.Request) *models.User {
	if appContextFrom(r) != nil {
		writeError(w, 404, "not_found", "Not found")
		return nil
	}
	user := currentUser(r)
	if user == nil {
		writeError(w, 401, "unauthorized", "Authentication required")
	}
	return user
}
func workspaceGroupBuiltin(id string) bool { return id == "all" || id == "mine" }
func writeWorkspaceGroupValidation(w http.ResponseWriter, field, code, message string) {
	writeError2(w, 400, "validation_error", message, map[string]interface{}{"field": field, "code": code})
}
func writeWorkspaceGroupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, 404, "not_found", "Workspace group not found")
	case errors.Is(err, store.ErrWorkspaceGroupNameTaken):
		writeError2(w, 409, "conflict", "A workspace group already has this name", map[string]interface{}{"field": "name", "code": "name_taken"})
	case errors.Is(err, store.ErrWorkspaceGroupLimit):
		writeWorkspaceGroupValidation(w, "groups", "range", "At most 20 workspace groups are allowed")
	case errors.Is(err, store.ErrWorkspaceGroupMemberLimit):
		writeWorkspaceGroupValidation(w, "workspace_ids", "range", "At most 100 workspaces are allowed per group")
	case errors.Is(err, store.ErrWorkspaceGroupUnknownWorkspace):
		writeWorkspaceGroupValidation(w, "workspace_ids", "unknown_workspace", "A workspace is not available")
	default:
		writeInternalError(w, err)
	}
}

// readWorkspaceGroupBody validates transport types before field semantics.
// Raw messages distinguish omission from null, including null array entries.
func readWorkspaceGroupBody(w http.ResponseWriter, r *http.Request, create bool) (name *string, ids *[]string, ok bool) {
	raw, err := strictScheduleBody[workspaceGroupBody](r)
	if err != nil {
		writeScheduleBodyError(w)
		return nil, nil, false
	}
	if len(raw.Name) > 0 && string(raw.Name) != "null" {
		var value string
		if err := json.Unmarshal(raw.Name, &value); err != nil {
			writeScheduleBodyError(w)
			return nil, nil, false
		}
		name = &value
	}
	if len(raw.WorkspaceIDs) > 0 && string(raw.WorkspaceIDs) != "null" {
		var values []*string
		if err := json.Unmarshal(raw.WorkspaceIDs, &values); err != nil {
			writeScheduleBodyError(w)
			return nil, nil, false
		}
		list := make([]string, 0, len(values))
		seen := map[string]bool{}
		for _, value := range values {
			if value == nil {
				writeScheduleBodyError(w)
				return nil, nil, false
			}
			if !seen[*value] {
				seen[*value] = true
				list = append(list, *value)
			}
		}
		ids = &list
	}
	if string(raw.Name) == "null" || (create && name == nil) {
		writeWorkspaceGroupValidation(w, "name", "required", "name is required")
		return nil, nil, false
	}
	if string(raw.WorkspaceIDs) == "null" || (create && ids == nil) {
		writeWorkspaceGroupValidation(w, "workspace_ids", "required", "workspace_ids is required")
		return nil, nil, false
	}
	if name != nil {
		*name = strings.TrimSpace(*name)
		invalid := utf8.RuneCountInString(*name) < 1 || utf8.RuneCountInString(*name) > 60
		for _, r := range *name {
			invalid = invalid || unicode.IsControl(r)
		}
		if invalid {
			writeWorkspaceGroupValidation(w, "name", "range", "name must contain 1 to 60 characters without control characters")
			return nil, nil, false
		}
		if strings.EqualFold(*name, "all") || strings.EqualFold(*name, "mine") {
			writeWorkspaceGroupValidation(w, "name", "reserved", "This workspace group name is reserved")
			return nil, nil, false
		}
	}
	if ids != nil && len(*ids) > store.MaxWorkspaceGroupMembers {
		writeWorkspaceGroupError(w, store.ErrWorkspaceGroupMemberLimit)
		return nil, nil, false
	}
	return name, ids, true
}

// validateWorkspaceGroupMembers refuses any id the caller is not admitted to and
// returns the admitted set (nil when ids is nil and nothing was read).
func (s *Server) validateWorkspaceGroupMembers(w http.ResponseWriter, r *http.Request, user *models.User, ids *[]string) (map[string]bool, bool) {
	if ids == nil {
		return nil, true
	}
	admitted, err := s.needsAdmittedWorkspaces(r, user)
	if err != nil {
		writeInternalError(w, err)
		return nil, false
	}
	allowed := map[string]bool{}
	for _, ws := range admitted {
		allowed[ws.ID] = true
	}
	for _, id := range *ids {
		if _, err := uuid.Parse(id); err != nil || !allowed[id] {
			writeWorkspaceGroupError(w, store.ErrWorkspaceGroupUnknownWorkspace)
			return nil, false
		}
	}
	return allowed, true
}

// visibleWorkspaceGroup filters every saved member and orders by the admitted
// workspace slug and id. Hidden members never contribute response metadata.
func visibleWorkspaceGroup(g models.WorkspaceGroup, admitted []models.Workspace) models.WorkspaceGroup {
	members := map[string]bool{}
	for _, id := range g.WorkspaceIDs {
		members[id] = true
	}
	visible := make([]models.Workspace, 0)
	for _, ws := range admitted {
		if members[ws.ID] {
			visible = append(visible, ws)
		}
	}
	sort.Slice(visible, func(i, j int) bool {
		if visible[i].Slug != visible[j].Slug {
			return visible[i].Slug < visible[j].Slug
		}
		return visible[i].ID < visible[j].ID
	})
	g.WorkspaceIDs = make([]string, 0, len(visible))
	for _, ws := range visible {
		g.WorkspaceIDs = append(g.WorkspaceIDs, ws.ID)
	}
	return g
}
func namedWorkspaceGroupView(g models.WorkspaceGroup, admitted []models.Workspace) workspaceGroupView {
	g = visibleWorkspaceGroup(g, admitted)
	return workspaceGroupView{ID: g.ID, Name: g.Name, Kind: "named", WorkspaceIDs: g.WorkspaceIDs, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt}
}
func builtinWorkspaceGroupView(id, userID string, admitted []models.Workspace) workspaceGroupView {
	g := models.WorkspaceGroup{ID: id, Name: "All workspaces"}
	if id == "mine" {
		g.Name = "My workspaces"
	}
	for _, ws := range admitted {
		if id == "all" || ws.OwnerID == userID {
			g.WorkspaceIDs = append(g.WorkspaceIDs, ws.ID)
		}
	}
	g = visibleWorkspaceGroup(g, admitted)
	return workspaceGroupView{ID: id, Name: g.Name, Kind: id, WorkspaceIDs: g.WorkspaceIDs}
}

func (s *Server) handleListWorkspaceGroups(w http.ResponseWriter, r *http.Request) {
	user := workspaceGroupCaller(w, r)
	if user == nil {
		return
	}
	groups, err := s.store.ListWorkspaceGroups(r.Context(), user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	admitted, err := s.needsAdmittedWorkspaces(r, user)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	out := []workspaceGroupView{builtinWorkspaceGroupView("all", user.ID, admitted), builtinWorkspaceGroupView("mine", user.ID, admitted)}
	for _, g := range groups {
		out = append(out, namedWorkspaceGroupView(g, admitted))
	}
	writeJSON(w, 200, struct {
		Groups []workspaceGroupView `json:"groups"`
	}{out})
}
func (s *Server) handleGetWorkspaceGroup(w http.ResponseWriter, r *http.Request) {
	user := workspaceGroupCaller(w, r)
	if user == nil {
		return
	}
	id := chi.URLParam(r, "id")
	if workspaceGroupBuiltin(id) {
		admitted, err := s.needsAdmittedWorkspaces(r, user)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		writeJSON(w, 200, builtinWorkspaceGroupView(id, user.ID, admitted))
		return
	}
	g, err := s.store.GetWorkspaceGroup(r.Context(), user.ID, id)
	if err != nil {
		writeWorkspaceGroupError(w, err)
		return
	}
	s.writeNamedWorkspaceGroup(w, r, user, *g, 200)
}
func (s *Server) writeNamedWorkspaceGroup(w http.ResponseWriter, r *http.Request, user *models.User, g models.WorkspaceGroup, status int) {
	// Re-read admission after a write so a concurrent revocation is not
	// answered using the set that admitted the request body.
	admitted, err := s.needsAdmittedWorkspaces(r, user)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, status, namedWorkspaceGroupView(g, admitted))
}
func (s *Server) handleCreateWorkspaceGroup(w http.ResponseWriter, r *http.Request) {
	user := workspaceGroupCaller(w, r)
	if user == nil {
		return
	}
	name, ids, ok := readWorkspaceGroupBody(w, r, true)
	if !ok {
		return
	}
	if _, ok := s.validateWorkspaceGroupMembers(w, r, user, ids); !ok {
		return
	}
	g, err := s.store.CreateWorkspaceGroup(r.Context(), user.ID, *name, *ids)
	if err != nil {
		writeWorkspaceGroupError(w, err)
		return
	}
	s.writeNamedWorkspaceGroup(w, r, user, *g, 201)
}
func (s *Server) handleUpdateWorkspaceGroup(w http.ResponseWriter, r *http.Request) {
	user := workspaceGroupCaller(w, r)
	if user == nil {
		return
	}
	id := chi.URLParam(r, "id")
	if workspaceGroupBuiltin(id) {
		writeWorkspaceGroupValidation(w, "id", "builtin_group", "Computed workspace groups cannot be changed")
		return
	}
	// Resolve ownership before parsing a body so all missing group ids have
	// the same refusal, even when a submitted body is invalid.
	current, err := s.store.GetWorkspaceGroup(r.Context(), user.ID, id)
	if err != nil {
		writeWorkspaceGroupError(w, err)
		return
	}
	name, ids, ok := readWorkspaceGroupBody(w, r, false)
	if !ok {
		return
	}
	admitted, ok := s.validateWorkspaceGroupMembers(w, r, user, ids)
	if !ok {
		return
	}
	// Saved members the caller cannot see right now stay: the body can only
	// name what the caller can see, so replacing the list must not erase them.
	var keep []string
	if ids != nil {
		for _, wsID := range current.WorkspaceIDs {
			if !admitted[wsID] {
				keep = append(keep, wsID)
			}
		}
	}
	g, err := s.store.UpdateWorkspaceGroup(r.Context(), user.ID, id, name, ids, keep)
	if err != nil {
		writeWorkspaceGroupError(w, err)
		return
	}
	s.writeNamedWorkspaceGroup(w, r, user, *g, 200)
}
func (s *Server) handleDeleteWorkspaceGroup(w http.ResponseWriter, r *http.Request) {
	user := workspaceGroupCaller(w, r)
	if user == nil {
		return
	}
	id := chi.URLParam(r, "id")
	if workspaceGroupBuiltin(id) {
		writeWorkspaceGroupValidation(w, "id", "builtin_group", "Computed workspace groups cannot be changed")
		return
	}
	if err := s.store.DeleteWorkspaceGroup(r.Context(), user.ID, id); err != nil {
		writeWorkspaceGroupError(w, err)
		return
	}
	writeJSON(w, 200, map[string]interface{}{"id": id, "deleted": true})
}
