package server

import (
	"errors"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/go-chi/chi/v5"
)

const hiddenCommitmentPartnerMessage = "This date is linked to something you can't view. Ask a workspace owner to change or remove the link."
const commitmentPreviewMessage = "This linked change needs a commitment move preview before applying"

// writeCommitmentGraphError handles only the closed graph error vocabulary.
// Unexpected failures go through the existing sanitized schedule fallback.
func (s *Server) writeCommitmentGraphError(w http.ResponseWriter, r *http.Request, err error) bool {
	status, code, message := http.StatusConflict, "", ""
	var refused *store.LinkRefusedError
	switch {
	case errors.Is(err, store.ErrScheduleLinked):
		code, message = "schedule_linked", hiddenCommitmentPartnerMessage
		if s.store != nil && r != nil {
			workspaceID, _ := r.Context().Value(ctxResolvedWorkspaceID).(string)
			if workspaceID != "" {
				item, e := s.store.ResolveItem(workspaceID, chi.URLParam(r, "itemSlug"))
				if e != nil {
					writeInternalError(w, errors.New("commitment_partner_lookup_failed"))
					return true
				}
				if item != nil {
					access, e := s.buildCommitmentAccess(r, s.store.Q(), workspaceID, item.ID, commitmentAccessLive)
					if e != nil {
						writeInternalError(w, errors.New("commitment_access_failed"))
						return true
					}
					partners, e := s.store.CommitmentLinkPartnersQ(s.store.Q(), workspaceID, item.ID)
					if e != nil {
						writeInternalError(w, errors.New("commitment_partner_lookup_failed"))
						return true
					}
					all := len(partners) > 0
					for _, id := range partners {
						if !access.Readable[id] {
							all = false
						}
					}
					if all {
						message = store.ErrScheduleLinked.Error()
					}
				}
			}
		}
	case errors.Is(err, store.ErrDeadlineApprovalRequired):
		code, message = "deadline_approval_required", store.ErrDeadlineApprovalRequired.Error()
	case errors.Is(err, store.ErrLinkViolationAck):
		code, message = "link_violation_ack", store.ErrLinkViolationAck.Error()
	case errors.Is(err, store.ErrCommitmentChanged):
		writeCommitmentChanged(w)
		return true
	case errors.Is(err, store.ErrLinkBudget):
		status, code, message = http.StatusServiceUnavailable, "link_budget", store.ErrLinkBudget.Error()
		w.Header().Set("Retry-After", "1")
	case errors.Is(err, store.ErrLinkEditRequired):
		status, code, message = http.StatusForbidden, "link_edit_required", store.ErrLinkEditRequired.Error()
	case errors.Is(err, store.ErrScheduleLinkNotFound):
		status, code, message = http.StatusNotFound, "schedule_link_not_found", "Commitment link or gate not found"
	case errors.Is(err, store.ErrCommitmentPreviewRequired):
		base := "/api/v1/workspaces/" + chi.URLParam(r, "slug") + "/items/" + chi.URLParam(r, "itemSlug") + "/commitment"
		writeError2(w, http.StatusConflict, "commitment_preview_required", commitmentPreviewMessage, map[string]interface{}{"preview": "POST " + base + "/move-preview", "commit": "POST " + base + "/move"})
		return true
	case errors.As(err, &refused):
		code = refused.Code
		switch code {
		case "link_cycle", "link_self", "link_duplicate_incoming", "link_limit", "gate_duplicate":
			message = code
		case "link_invalid":
			status, message = http.StatusBadRequest, "Invalid commitment link configuration"
		default:
			return false
		}
	default:
		return false
	}
	writeError(w, status, code, message)
	return true
}

func (s *Server) writeScheduleError(w http.ResponseWriter, r *http.Request, err error) bool {
	return s.writeCommitmentGraphError(w, r, err) || writeScheduleError(w, err)
}
func (s *Server) writeResolveError(w http.ResponseWriter, r *http.Request, err error) bool {
	return s.writeCommitmentGraphError(w, r, err) || writeResolveError(w, r, err)
}
