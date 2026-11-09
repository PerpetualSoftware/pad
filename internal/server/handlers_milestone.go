package server

import (
	"errors"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/go-chi/chi/v5"
)

// Milestone forecasts and outcomes (TASK-4358): the write doors beside the
// commitment. The gate order is the commitment's: the item (404 or 409), edit
// permission, the schedule (404), then the named child.

type forecastBody struct {
	Value  *string `json:"value"`
	Source *string `json:"source"`
}

type milestoneChildBody struct {
	Kind   *string `json:"kind"`
	Reason *string `json:"reason"`
}

// milestoneGate runs the gates every milestone write shares and returns the
// item and its schedule, or nil after writing the refusal.
func (s *Server) milestoneGate(w http.ResponseWriter, r *http.Request) (string, *models.Item, *models.ItemSchedule) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return "", nil, nil
	}
	item := s.resolveVisibleItem(w, r, workspaceID)
	if item == nil {
		return "", nil, nil
	}
	if !s.requireEditPermission(w, r, workspaceID, item.ID, item.CollectionID) {
		return "", nil, nil
	}
	sc, err := s.store.GetSchedule(workspaceID, item.ID)
	if err != nil {
		writeInternalError(w, err)
		return "", nil, nil
	}
	if sc == nil {
		writeScheduleNotFound(w)
		return "", nil, nil
	}
	return workspaceID, item, sc
}

// milestoneWriteDone maps a store error from a milestone write, or records the
// activity (names only) and answers with the commitment view.
func (s *Server) milestoneWriteDone(w http.ResponseWriter, r *http.Request, workspaceID string, item *models.Item, sc *models.ItemSchedule, member string, err error) {
	switch {
	case s.writeScheduleItemGone(w, r, workspaceID, err):
		return
	case errors.Is(err, store.ErrScheduleNotFound):
		writeScheduleNotFound(w)
		return
	case errors.Is(err, store.ErrMilestoneChildNotFound):
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	case writeScheduleError(w, err):
		return
	case err != nil:
		writeInternalError(w, err)
		return
	}
	s.recordScheduleWrite(r, workspaceID, item, "schedule_updated", scheduleActivityMeta(sc.Kind, []string{member}))
	s.writeItemCommitment(w, r, workspaceID, item, http.StatusOK, "")
}

func (s *Server) handlePutCommitmentForecast(w http.ResponseWriter, r *http.Request) {
	workspaceID, item, sc := s.milestoneGate(w, r)
	if sc == nil {
		return
	}
	body, err := strictScheduleBody[forecastBody](r)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	if body.Value == nil {
		writeScheduleRequired(w, "value")
		return
	}
	source := ""
	if body.Source != nil {
		source = *body.Source
	}
	actor, _ := actorFromRequest(r)
	err = s.store.SetForecast(workspaceID, item.ID, currentUserID(r), actor, *body.Value, source)
	s.milestoneWriteDone(w, r, workspaceID, item, sc, "forecast", err)
}

func (s *Server) handleDeleteCommitmentForecast(w http.ResponseWriter, r *http.Request) {
	workspaceID, item, sc := s.milestoneGate(w, r)
	if sc == nil {
		return
	}
	actor, _ := actorFromRequest(r)
	err := s.store.ClearForecast(workspaceID, item.ID, currentUserID(r), actor)
	s.milestoneWriteDone(w, r, workspaceID, item, sc, "forecast", err)
}

// resolveMilestoneChild resolves {childRef} for a caller that must be able to
// read it. An unknown item, one in another workspace, an archived one and one
// the caller cannot read all answer the same bytes, and this runs before any
// check that would reveal the child's existence (duplicate, cap).
func (s *Server) resolveMilestoneChild(w http.ResponseWriter, r *http.Request, workspaceID string) *models.Item {
	child, err := s.store.ResolveItem(workspaceID, chi.URLParam(r, "childRef"))
	if err != nil {
		writeInternalError(w, err)
		return nil
	}
	if child == nil || !s.itemVisibleFor(r, workspaceID)(child.ID) {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return nil
	}
	return child
}

func (s *Server) handlePutMilestoneChild(w http.ResponseWriter, r *http.Request) {
	workspaceID, item, sc := s.milestoneGate(w, r)
	if sc == nil {
		return
	}
	body, err := strictScheduleBody[milestoneChildBody](r)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	child := s.resolveMilestoneChild(w, r, workspaceID)
	if child == nil {
		return
	}
	kind, reason := "", ""
	if body.Kind != nil {
		kind = *body.Kind
	}
	if body.Reason != nil {
		reason = *body.Reason
	}
	err = s.store.SetMilestoneChild(workspaceID, item.ID, child.ID, kind, reason, currentUserID(r))
	s.milestoneWriteDone(w, r, workspaceID, item, sc, "children", err)
}

func (s *Server) handleDeleteMilestoneChild(w http.ResponseWriter, r *http.Request) {
	workspaceID, item, sc := s.milestoneGate(w, r)
	if sc == nil {
		return
	}
	child := s.resolveMilestoneChild(w, r, workspaceID)
	if child == nil {
		return
	}
	err := s.store.RemoveMilestoneChild(workspaceID, item.ID, child.ID, currentUserID(r))
	s.milestoneWriteDone(w, r, workspaceID, item, sc, "children", err)
}

// readableChildren decides, before any transaction opens, which of the
// milestone's selected children the caller may read. The copy and its preview
// count only those.
func (s *Server) readableChildren(r *http.Request, workspaceID, itemID string) (func(string) bool, error) {
	ids, err := s.store.MilestoneChildIDs(workspaceID, itemID)
	if err != nil {
		return nil, err
	}
	visible := s.itemVisibleFor(r, workspaceID)
	ok := make(map[string]bool, len(ids))
	for _, id := range ids {
		ok[id] = visible(id)
	}
	return func(id string) bool { return ok[id] }, nil
}
