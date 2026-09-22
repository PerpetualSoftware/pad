package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// handleListItemDecisions returns an item's latest typed-decision answer per
// question (TASK-3117). An answer computed from an earlier state of the item
// is still returned — it is the newest one there is — with current=false.
//
// With no provider configured the list is empty: no rows are ever written,
// and rows written under an earlier configuration are not served, since
// nothing would keep them current.
func (s *Server) handleListItemDecisions(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	item, err := s.store.ResolveItem(workspaceID, chi.URLParam(r, "itemSlug"))
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return
	}
	decisions, err := s.decisionRunner().Decisions(item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ref":       item.Ref,
		"decisions": decisions,
	})
}
