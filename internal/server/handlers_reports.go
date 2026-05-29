package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// handleGetReport serves the windowed project report (PLAN-1628 / TASK-1630):
//
//	GET /workspaces/{slug}/report?window=week&collections=tasks,bugs
//
// window ∈ {day, week, 2wk, month} (default week). collections is an optional
// comma-separated list of collection slugs to include; omitted = all.
func (s *Server) handleGetReport(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	scopeToVisible, visibleIDs, err := s.reportVisibleCollections(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	opts := store.ReportOptions{
		Window:               r.URL.Query().Get("window"),
		ScopeToVisible:       scopeToVisible,
		VisibleCollectionIDs: visibleIDs,
	}
	// offset = periods back (0 = current). Non-numeric/negative → 0 (the store
	// also clamps).
	if n, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("offset"))); err == nil && n > 0 {
		opts.Offset = n
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("collections")); raw != "" {
		for _, slug := range strings.Split(raw, ",") {
			if s := strings.TrimSpace(slug); s != "" {
				opts.Collections = append(opts.Collections, s)
			}
		}
	}

	report, err := s.store.GetReport(workspaceID, opts)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// reportVisibleCollections computes the collection-visibility scope for the
// aggregate report. Aggregate counts have no item-level filtering, so a
// collection is only in scope if the caller can see ALL of it — otherwise a
// member/guest could infer hidden collections' slugs, throughput, and status
// distribution.
//
//   - scopeToVisible=false → unrestricted (no-auth instance, cookie platform
//     admin, or an all-access member): the full workspace report.
//   - scopeToVisible=true → restrict to ids (the caller's full-collection set).
//
// Bearer-aware: s.visibleCollectionIDs() would grant ANY platform admin an
// unrestricted view, but RequireWorkspaceAccess suppresses the platform-admin
// bypass for bearer auth (PAT/CLI/OAuth) and falls through to membership — so a
// bearer admin who is only a restricted member must be scoped to their actual
// membership (BUG-1616/1617 pattern). When the caller has item-level grants,
// scope to the full-access collection set only (item-grant-only collections'
// aggregates must not leak), mirroring the dashboard's dashCollIDs logic.
func (s *Server) reportVisibleCollections(r *http.Request, workspaceID string) (scopeToVisible bool, ids []string, err error) {
	user := currentUser(r)
	if user == nil || (user.Role == "admin" && !isBearerAuth(r)) {
		return false, nil, nil // unrestricted
	}
	visibleIDs, err := s.store.VisibleCollectionIDs(workspaceID, user.ID)
	if err != nil {
		return false, nil, err
	}
	if visibleIDs == nil {
		// all-access member → no filtering.
		return false, nil, nil
	}
	ids = visibleIDs
	fullCollIDs, grantedItemIDs, gErr := s.guestResourceFilter(r, workspaceID)
	if gErr != nil {
		return false, nil, gErr
	}
	if len(grantedItemIDs) > 0 {
		ids = fullCollIDs
	}
	return true, ids, nil
}

// handleGetReportLayout returns the current user's saved Insights layout for
// the workspace (PLAN-1628 / TASK-1634), or surface defaults when none/no user.
//
//	GET /workspaces/{slug}/report/layout
func (s *Server) handleGetReportLayout(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	layout := models.ReportLayout{HiddenCards: []string{}, DefaultCollections: []string{}}
	if user := currentUser(r); user != nil {
		saved, err := s.store.GetReportLayout(user.ID, workspaceID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if saved != nil {
			layout = *saved
			if layout.HiddenCards == nil {
				layout.HiddenCards = []string{}
			}
			if layout.DefaultCollections == nil {
				layout.DefaultCollections = []string{}
			}
		}
	}
	writeJSON(w, http.StatusOK, layout)
}

// handleSaveReportLayout upserts the current user's Insights layout for the
// workspace. Per-user; requires an authenticated user.
//
//	PUT /workspaces/{slug}/report/layout   (body: models.ReportLayout)
func (s *Server) handleSaveReportLayout(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in to save your Insights layout")
		return
	}

	var layout models.ReportLayout
	if err := json.NewDecoder(r.Body).Decode(&layout); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid layout JSON")
		return
	}

	// Sanitize before persisting: drop an invalid window, and filter
	// hidden_cards to the known toggleable set (dedup) so stored config can't
	// drift from the UI's card vocabulary.
	if !models.ValidReportWindow(layout.DefaultWindow) {
		layout.DefaultWindow = ""
	}
	clean := []string{}
	seen := map[string]bool{}
	for _, c := range layout.HiddenCards {
		if models.ReportCardIDs[c] && !seen[c] {
			clean = append(clean, c)
			seen[c] = true
		}
	}
	layout.HiddenCards = clean
	if layout.DefaultCollections == nil {
		layout.DefaultCollections = []string{}
	}

	if err := s.store.SaveReportLayout(user.ID, workspaceID, layout); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, layout)
}
