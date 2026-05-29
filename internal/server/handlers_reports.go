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

	// Scope the report to the caller's FULL-collection visibility, mirroring
	// the dashboard's dashCollIDs logic. Aggregate report counts have no
	// item-level filtering, so a collection is only in scope if the caller can
	// see all of it — otherwise a member/guest could infer hidden collections'
	// slugs, throughput, and status distribution.
	//
	//   - visibleCollectionIDs == nil → admin / all-access member: no
	//     restriction (full workspace report).
	//   - item-level grants present → use the full-access collection set
	//     (fullCollIDs); item-grant-only collections are NOT counted, since
	//     that would leak the whole collection's aggregates.
	//   - otherwise → the caller's full-collection set (specific members,
	//     guests with only full-collection grants).
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	opts := store.ReportOptions{
		Window: r.URL.Query().Get("window"),
	}
	if visibleIDs != nil {
		opts.ScopeToVisible = true
		opts.VisibleCollectionIDs = visibleIDs
		fullCollIDs, grantedItemIDs, gErr := s.guestResourceFilter(r, workspaceID)
		if gErr != nil {
			writeInternalError(w, gErr)
			return
		}
		if len(grantedItemIDs) > 0 {
			opts.VisibleCollectionIDs = fullCollIDs
		}
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
