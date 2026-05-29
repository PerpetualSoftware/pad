package server

import (
	"net/http"
	"strings"

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
