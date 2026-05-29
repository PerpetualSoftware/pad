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

	// Scope the report to the caller's visible collections, mirroring the
	// dashboard. Without this, a member/guest with access to one collection
	// could infer hidden collections' slugs, throughput, and status
	// distribution from the aggregate counts. Aggregate reports are a
	// full-collection-visibility feature; item-level grants are intentionally
	// not surfaced in workspace-wide counts.
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	opts := store.ReportOptions{
		Window:               r.URL.Query().Get("window"),
		ScopeToVisible:       true,
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
