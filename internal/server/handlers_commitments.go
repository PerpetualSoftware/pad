package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/go-chi/chi/v5"
)

// canRenegotiateCommitment is the authority decision for renegotiations and
// their previews. The caller owns the item's visibility and write doors.
func (s *Server) canRenegotiateCommitment(r *http.Request, workspaceID string, item *models.Item) (bool, error) {
	if workspaceRole(r) == "owner" {
		return true, nil
	}
	if item.AssignedUserID != nil && *item.AssignedUserID != "" {
		return currentUserID(r) != "" && currentUserID(r) == *item.AssignedUserID, nil
	}
	return s.canEditInCollection(r, workspaceID, item.ID, item.CollectionID)
}

// writeItemCommitment re-reads the roles and journal after a successful write.
func (s *Server) writeItemCommitment(w http.ResponseWriter, workspaceID, itemID string, status int) {
	c, err := s.store.GetCommitment(workspaceID, itemID)
	if errors.Is(err, store.ErrScheduleNotFound) {
		writeScheduleNotFound(w)
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, status, c)
}

func (s *Server) handleGetItemCommitment(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	// Like GET /schedule, archived items are readable.
	item, err := s.store.ResolveItemIncludeDeleted(workspaceID, chi.URLParam(r, "itemSlug"))
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
	s.writeItemCommitment(w, workspaceID, item.ID, http.StatusOK)
}

type commitmentCreateBody struct {
	models.ScheduleCreate
	Roles *models.CommitmentRolesInput `json:"roles"`
}

func (s *Server) handleCreateItemCommitment(w http.ResponseWriter, r *http.Request) {
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
	body, err := scheduleBodyBytes[commitmentCreateBody](raw)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	// Preserve the schedule create door's required-null field errors.
	var required struct {
		Kind    json.RawMessage `json:"kind"`
		Dtstart json.RawMessage `json:"dtstart"`
	}
	if err := json.Unmarshal(raw, &required); err != nil {
		writeScheduleBodyError(w)
		return
	}
	if isJSONNull(required.Kind) {
		writeScheduleRequired(w, "kind")
		return
	}
	if isJSONNull(required.Dtstart) {
		writeScheduleRequired(w, "dtstart")
		return
	}
	if body.Roles == nil {
		writeScheduleRequired(w, "roles")
		return
	}
	req := body.ScheduleCreate
	if !req.Floating && req.Tz == "" {
		req.Tz, _ = defaultScheduleZone(r)
	}
	sc, err := s.store.CreateCommitmentSchedule(workspaceID, item.ID, req, *body.Roles)
	if s.writeScheduleItemGone(w, r, workspaceID, err) || writeScheduleError(w, err) {
		return
	}
	s.recordScheduleWrite(r, workspaceID, item, "schedule_created", scheduleActivityMeta(sc.Kind, nil))
	s.writeItemCommitment(w, workspaceID, item.ID, http.StatusCreated)
}

type commitmentPatchBody struct {
	Roles *models.CommitmentRolesInput `json:"roles"`
}

func (s *Server) handleUpdateItemCommitment(w http.ResponseWriter, r *http.Request) {
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
	body, err := strictScheduleBody[commitmentPatchBody](r)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	if body.Roles == nil {
		writeScheduleRequired(w, "roles")
		return
	}
	sc, err := s.store.SetCommitmentRoles(workspaceID, item.ID, *body.Roles)
	if s.writeScheduleItemGone(w, r, workspaceID, err) {
		return
	}
	if errors.Is(err, store.ErrScheduleNotFound) {
		writeScheduleNotFound(w)
		return
	}
	if writeScheduleError(w, err) {
		return
	}
	s.recordScheduleWrite(r, workspaceID, item, "schedule_updated", scheduleActivityMeta(sc.Kind, []string{"roles"}))
	s.writeItemCommitment(w, workspaceID, item.ID, http.StatusOK)
}

type commitmentResolveBody struct {
	scheduleResolveBody
	ActualDoneAt *string `json:"actual_done_at"`
}

func (s *Server) handleResolveItemCommitment(w http.ResponseWriter, r *http.Request) {
	s.resolveItemSchedule(w, r, true)
}

func writeDeadlineOwnerRequired(w http.ResponseWriter) {
	writeError2(w, http.StatusForbidden, "deadline_owner_required",
		"A schedule with a hard deadline is removed or changed only by the item's assignee or a workspace owner; to remove or change a hard deadline use POST .../commitment/renegotiate",
		map[string]interface{}{"field": "deadline", "code": "authority"})
}

func (s *Server) handleRenegotiateItemCommitment(w http.ResponseWriter, r *http.Request) {
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
	allowed, err := s.canRenegotiateCommitment(r, workspaceID, item)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if !allowed {
		writeDeadlineOwnerRequired(w)
		return
	}
	body, err := strictScheduleBody[models.CommitmentRenegotiateInput](r)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	result, err := s.store.RenegotiateCommitment(workspaceID, item.ID, currentUserID(r), body)
	if errors.Is(err, store.ErrCommitmentChanged) {
		writeError2(w, http.StatusConflict, "commitment_changed", "The commitment changed; re-preview before applying",
			map[string]interface{}{"field": "revision", "code": "stale"})
		return
	}
	if s.writeScheduleItemGone(w, r, workspaceID, err) {
		return
	}
	if errors.Is(err, store.ErrScheduleNotFound) {
		writeScheduleNotFound(w)
		return
	}
	if writeScheduleError(w, err) {
		return
	}
	if body.Preview {
		writeJSON(w, http.StatusOK, result)
		return
	}
	sc, err := s.store.GetSchedule(workspaceID, item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if sc == nil {
		writeScheduleNotFound(w)
		return
	}
	fields := []string{}
	if body.Deadline != nil {
		fields = append(fields, "deadline")
	}
	if body.DeadlineOffset != nil {
		fields = append(fields, "deadline_offset")
	}
	if body.DeadlineBehavior != nil {
		fields = append(fields, "deadline_behavior")
	}
	if body.RemoveDeadline {
		fields = append(fields, "remove_deadline")
	}
	s.recordScheduleWrite(r, workspaceID, item, "schedule_updated", scheduleActivityMeta(sc.Kind, fields))
	s.writeItemCommitment(w, workspaceID, item.ID, http.StatusOK)
}
