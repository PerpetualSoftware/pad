package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// The per-user open set of workspace tabs (PLAN-3002 U1 / TASK-3256):
// /api/v1/me/workspace-tabs. User-scoped, so it sits outside the /{slug}
// access subrouter, like PUT /workspaces/reorder, and every handler resolves
// the workspace against the caller's OWN visible set instead. Web client
// only: no CLI or MCP surface (CONVE-191).
//
// Every response that lists tabs lists only tabs whose workspace is in that
// visible set, so a tab never names a workspace the caller cannot open, even
// for a loss path that never deleted its row.

// maxWorkspaceTabLastRouteLen bounds a stored last_route. A route is a path
// plus a short query; 2048 is the conventional URL ceiling and far above any
// route the web app builds.
const maxWorkspaceTabLastRouteLen = 2048

type workspaceTabsResponse struct {
	Tabs []models.WorkspaceTab `json:"tabs"`
}

// tabsVisibleWorkspaces is the set every tab read and write is checked
// against: the caller's workspaces (members and guests, live only), narrowed
// by an OAuth token's allow-list exactly as GET /workspaces narrows it.
func (s *Server) tabsVisibleWorkspaces(r *http.Request, userID string) ([]models.Workspace, error) {
	visible, err := s.store.GetUserWorkspaces(userID)
	if err != nil {
		return nil, err
	}
	return filterWorkspacesByTokenAllowlist(r.Context(), visible), nil
}

func findVisibleWorkspace(visible []models.Workspace, slug string) *models.Workspace {
	for i := range visible {
		if visible[i].Slug == slug {
			return &visible[i]
		}
	}
	return nil
}

// writeWorkspaceTabs answers with the caller's current, visibility-filtered
// open set. Every write answers with it too, so the client never needs a
// second round trip to learn what the write did.
//
// The visible set is read AGAIN here rather than reused from the request's
// start: access revoked while the write ran must not be served from the
// earlier snapshot (codex round 1 on TASK-3256). A row the write left for a
// workspace revoked in that window stays stored and is hidden by this filter
// on every read, which is the design's read-side invariant.
func (s *Server) writeWorkspaceTabs(w http.ResponseWriter, r *http.Request, userID string) {
	visible, err := s.tabsVisibleWorkspaces(r, userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	rows, err := s.store.ListWorkspaceTabRows(userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workspaceTabsResponse{Tabs: store.VisibleWorkspaceTabs(rows, visible)})
}

// tabsCaller returns the user and their visible set, or writes the refusal.
func (s *Server) tabsCaller(w http.ResponseWriter, r *http.Request) (string, []models.Workspace, bool) {
	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return "", nil, false
	}
	visible, err := s.tabsVisibleWorkspaces(r, userID)
	if err != nil {
		writeInternalError(w, err)
		return "", nil, false
	}
	return userID, visible, true
}

func (s *Server) handleListWorkspaceTabs(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}
	s.writeWorkspaceTabs(w, r, userID)
}

// handleOpenWorkspaceTab: POST {slug, ephemeral}. A workspace outside the
// caller's visible set answers the workspace 404 (BUG-3069), the same answer
// an unknown slug gets.
func (s *Server) handleOpenWorkspaceTab(w http.ResponseWriter, r *http.Request) {
	userID, visible, ok := s.tabsCaller(w, r)
	if !ok {
		return
	}
	var input struct {
		Slug      string `json:"slug"`
		Ephemeral bool   `json:"ephemeral"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if input.Slug == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "slug is required")
		return
	}
	ws := findVisibleWorkspace(visible, input.Slug)
	if ws == nil {
		writeWorkspaceNotFound(w, "Workspace not found")
		return
	}
	if err := s.store.OpenWorkspaceTab(userID, ws.ID, input.Ephemeral); err != nil {
		writeInternalError(w, err)
		return
	}
	s.writeWorkspaceTabs(w, r, userID)
}

// handleReorderWorkspaceTabs: PUT [slug, …], the bar's full order. Slugs the
// caller cannot see, or that are not open, are ignored silently (the answer an
// unknown slug gets from PUT /workspaces/reorder), so the response says
// nothing about which slugs exist.
func (s *Server) handleReorderWorkspaceTabs(w http.ResponseWriter, r *http.Request) {
	userID, visible, ok := s.tabsCaller(w, r)
	if !ok {
		return
	}
	var slugs []string
	if err := decodeJSON(r, &slugs); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	order := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		if ws := findVisibleWorkspace(visible, slug); ws != nil {
			order = append(order, ws.ID)
		}
	}
	if err := s.store.ReorderWorkspaceTabs(userID, order); err != nil {
		writeInternalError(w, err)
		return
	}
	s.writeWorkspaceTabs(w, r, userID)
}

// handleCloseWorkspaceTab: DELETE /{slug}. Closing is idempotent, and a slug
// outside the visible set is a no-op with the same 200, so this door is not
// an existence oracle either.
func (s *Server) handleCloseWorkspaceTab(w http.ResponseWriter, r *http.Request) {
	userID, visible, ok := s.tabsCaller(w, r)
	if !ok {
		return
	}
	if ws := findVisibleWorkspace(visible, chi.URLParam(r, "slug")); ws != nil {
		if err := s.store.CloseWorkspaceTab(userID, ws.ID); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	s.writeWorkspaceTabs(w, r, userID)
}

// handleUpdateWorkspaceTab: PATCH /{slug} {pin?, last_route?}. pin makes an
// ephemeral tab durable (there is no unpin); last_route replaces the stored
// route, and "" or null clears it.
func (s *Server) handleUpdateWorkspaceTab(w http.ResponseWriter, r *http.Request) {
	userID, visible, ok := s.tabsCaller(w, r)
	if !ok {
		return
	}
	var input struct {
		Pin       *bool          `json:"pin"`
		LastRoute optionalString `json:"last_route"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if input.Pin == nil && !input.LastRoute.Set {
		writeError(w, http.StatusBadRequest, "validation_error", "nothing to update: send pin or last_route")
		return
	}
	if input.Pin != nil && !*input.Pin {
		writeError(w, http.StatusBadRequest, "validation_error", "pin can only be true: a kept tab is not made ephemeral again")
		return
	}
	ws := findVisibleWorkspace(visible, chi.URLParam(r, "slug"))
	if ws == nil {
		writeWorkspaceNotFound(w, "Workspace not found")
		return
	}
	u := store.WorkspaceTabUpdate{Pin: input.Pin != nil && *input.Pin}
	if input.LastRoute.Set {
		route := input.LastRoute.Value
		if route != "" {
			if msg := validateWorkspaceTabLastRoute(route, ws.OwnerUsername, ws.Slug); msg != "" {
				writeError(w, http.StatusBadRequest, "validation_error", msg)
				return
			}
		}
		u.LastRoute = &route
	}
	if err := s.store.UpdateWorkspaceTab(userID, ws.ID, u); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "This workspace is not open in a tab")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.writeWorkspaceTabs(w, r, userID)
}

// validateWorkspaceTabLastRoute accepts only a same-workspace path: the
// workspace's own /{owner}/{ws} or anything under /{owner}/{ws}/, with an
// optional query or fragment. Anything else would turn the stored route into
// an open redirect when the client navigates to it. It returns "" when the
// route is acceptable, else the refusal message.
func validateWorkspaceTabLastRoute(route, owner, slug string) string {
	if len(route) > maxWorkspaceTabLastRouteLen {
		return "last_route is too long"
	}
	if owner == "" || slug == "" {
		return "this workspace has no route prefix to store a last_route under"
	}
	for _, c := range route {
		if c == '\\' || unicode.IsControl(c) || unicode.IsSpace(c) {
			return "last_route must be a plain path"
		}
	}
	path := route
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	// Checked on the DECODED path too. A URL parser reads %2e%2e as a ..
	// segment (codex round 1 on TASK-3256), so the literal checks below alone
	// let /{owner}/{ws}/%2e%2e/other through to navigate out of the
	// workspace. Decoding everything is a superset of the dot-segment rule,
	// and an escape that does not decode is refused. So is a '%' that SURVIVES
	// one decode (codex round 2): %252e%252e decodes to %2e%2e, which any layer
	// decoding once more reads as .. again. The web app's routes are slugs and
	// refs, so a path never needs one; the query is not checked.
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return "last_route must be a valid path"
	}
	if strings.Contains(decoded, "%") {
		return "last_route must not be encoded twice"
	}
	for _, c := range decoded {
		if c == '\\' || unicode.IsControl(c) || unicode.IsSpace(c) {
			return "last_route must be a plain path"
		}
	}
	prefix := "/" + owner + "/" + slug
	if path != prefix && !strings.HasPrefix(path, prefix+"/") {
		return "last_route must be a path inside this workspace"
	}
	for _, p := range []string{path, decoded} {
		for _, seg := range strings.Split(p[1:], "/") {
			if seg == "." || seg == ".." {
				return "last_route must not contain . or .. segments"
			}
		}
		if strings.Contains(p, "//") {
			return "last_route must not contain an empty segment"
		}
	}
	return ""
}

// optionalString tells an absent JSON member (Set false) from a present one.
// null and "" both decode to a present empty string, which clears.
type optionalString struct {
	Set   bool
	Value string
}

func (o *optionalString) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = ""
		return nil
	}
	return json.Unmarshal(b, &o.Value)
}
