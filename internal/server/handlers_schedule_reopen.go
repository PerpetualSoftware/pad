package server

import (
	"errors"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/google/uuid"
)

func (s *Server) handleReopenItemCommitment(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	item := s.resolveVisibleItem(w, r, workspaceID)
	if item == nil {
		return
	}
	if !s.requireEditPermission(w, r, workspaceID, item.ID, item.CollectionID) {
		return
	}
	raw, err := readBodyForDecode(r, defaultJSONBodyLimit)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	body, err := scheduleBodyBytes[struct {
		ResolutionID *string `json:"resolution_id"`
	}](raw)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	if body.ResolutionID == nil || *body.ResolutionID == "" {
		writeResolveField(w, "resolution_id", "required", "resolution_id is required")
		return
	}
	parsedID, err := uuid.Parse(*body.ResolutionID)
	if err != nil || len(*body.ResolutionID) != 36 {
		writeResolveField(w, "resolution_id", "format", "resolution_id must be a UUID")
		return
	}
	activity := s.activityForRequest(workspaceID, item.ID, "schedule_reopened", r, "")
	result, err := s.store.ReopenOccurrence(workspaceID, item.ID, store.ReopenInput{ResolutionID: parsedID.String(), Actor: activity.Actor, ActorUserID: currentUserID(r), Activity: activity})
	var refusal *store.ReopenError
	if errors.As(err, &refusal) {
		writeError(w, http.StatusConflict, refusal.Code, refusal.Error())
		return
	}
	if s.writeScheduleItemGone(w, r, workspaceID, err) || s.writeResolveError(w, r, err) {
		return
	}
	s.publishItemUpdatedFromRequest(workspaceID, item, r, true)
	sc, rev, err := s.store.GetScheduleRevision(workspaceID, item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	view, err := s.buildCommitmentView(r, workspaceID, item, sc, "")
	if err != nil {
		writeInternalError(w, err)
		return
	}
	view.CommitmentRevision = rev
	writeJSON(w, http.StatusOK, map[string]any{"commitment": view, "reopen": result})
}
