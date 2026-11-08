package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"

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

// commitmentAssigneeGuard preserves the snapshot the authority decision used.
// Workspace owners can act regardless of assignment.
func commitmentAssigneeGuard(r *http.Request, item *models.Item) *string {
	if workspaceRole(r) == "owner" {
		return nil
	}
	assigned := ""
	if item.AssignedUserID != nil {
		assigned = *item.AssignedUserID
	}
	return &assigned
}

func writeCommitmentChanged(w http.ResponseWriter) {
	writeError2(w, http.StatusConflict, "commitment_changed", "The commitment changed; re-preview before applying",
		map[string]interface{}{"field": "revision", "code": "stale"})
}

// commitmentView adds the schedule view to the commitment without exposing the
// stored kind and dtend. Every successful read/write uses this builder.
type commitmentView struct {
	*models.Commitment
	scheduleView
	Kind  *string `json:"kind,omitempty"`
	Dtend *string `json:"dtend,omitempty"`
}

func (s *Server) writeItemCommitment(w http.ResponseWriter, r *http.Request, workspaceID string, item *models.Item, status int, note string) {
	c, err := s.store.GetCommitment(workspaceID, item.ID)
	if errors.Is(err, store.ErrScheduleNotFound) {
		writeScheduleNotFound(w)
		return
	}
	if err != nil {
		writeInternalError(w, err)
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
	v := s.buildScheduleView(r, item, sc)
	v.TimezoneNote = note
	writeJSON(w, status, commitmentView{Commitment: c, scheduleView: v})
}

// readCommitmentBody gives retired members the same field error on POST and
// PATCH, including null. The strict decoder still judges every other member.
func readCommitmentBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := readBodyForDecode(r, defaultJSONBodyLimit)
	if err != nil {
		writeScheduleBodyError(w)
		return nil, false
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		writeScheduleBodyError(w)
		return nil, false
	}
	for _, name := range []string{"kind", "dtend"} {
		if _, found := members[name]; found {
			writeError2(w, http.StatusBadRequest, "validation_error", name+" is not accepted", map[string]interface{}{"field": name, "code": "not_accepted"})
			return nil, false
		}
	}
	return raw, true
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
	s.writeItemCommitment(w, r, workspaceID, item, http.StatusOK, "")
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
	raw, ok := readCommitmentBody(w, r)
	if !ok {
		return
	}
	body, err := scheduleBodyBytes[commitmentCreateBody](raw)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	// Preserve the schedule create door's required-null field errors.
	var required struct {
		Dtstart json.RawMessage `json:"dtstart"`
	}
	if err := json.Unmarshal(raw, &required); err != nil {
		writeScheduleBodyError(w)
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
	note := ""
	if !req.Floating && req.Tz == "" {
		req.Tz, note = defaultScheduleZone(r)
	}
	sc, err := s.store.CreateCommitmentSchedule(workspaceID, item.ID, req, *body.Roles)
	if s.writeScheduleItemGone(w, r, workspaceID, err) || writeScheduleError(w, err) {
		return
	}
	s.recordScheduleWrite(r, workspaceID, item, "schedule_created", scheduleActivityMeta(sc.Kind, nil))
	s.writeItemCommitment(w, r, workspaceID, item, http.StatusCreated, note)
}

// This type is the single accepted-member list for PATCH. Raw fields preserve
// absence and null; strict decoding refuses every undeclared member.
type commitmentPatchBody struct {
	Dtstart     json.RawMessage `json:"dtstart,omitempty"`
	Tz          json.RawMessage `json:"tz,omitempty"`
	Floating    json.RawMessage `json:"floating,omitempty"`
	Rrule       json.RawMessage `json:"rrule,omitempty"`
	RDates      json.RawMessage `json:"rdates,omitempty"`
	ExDates     json.RawMessage `json:"exdates,omitempty"`
	Anchor      json.RawMessage `json:"anchor,omitempty"`
	Audience    json.RawMessage `json:"audience,omitempty"`
	LeadMinutes json.RawMessage `json:"lead_minutes,omitempty"`
	Roles       json.RawMessage `json:"roles,omitempty"`
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
	raw, ok := readCommitmentBody(w, r)
	if !ok {
		return
	}
	body, err := scheduleBodyBytes[commitmentPatchBody](raw)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	// Marshal the declared fields to collect only present members. This keeps
	// validation and activity member names tied to the strict body's definition.
	declared, _ := json.Marshal(body)
	var members map[string]json.RawMessage
	_ = json.Unmarshal(declared, &members)
	if len(members) == 0 {
		writeScheduleRequired(w, "body")
		return
	}
	fields := make([]string, 0, len(members))
	for name := range members {
		fields = append(fields, name)
	}
	sort.Strings(fields)
	var roles *models.CommitmentRolesInput
	if body.Roles != nil {
		if isJSONNull(body.Roles) {
			writeScheduleRequired(w, "roles")
			return
		}
		roles = &models.CommitmentRolesInput{}
		parsed, err := scheduleBodyBytes[models.CommitmentRolesInput](body.Roles)
		if err != nil {
			writeScheduleBodyError(w)
			return
		}
		*roles = parsed
	}
	delete(members, "roles")
	upd, _, ok := scheduleUpdateFromMembers(w, members)
	if !ok {
		return
	}
	note := ""
	if upd.Floating != nil && !*upd.Floating && upd.Tz == nil {
		cur, err := s.store.GetSchedule(workspaceID, item.ID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if cur != nil && cur.Floating {
			name, n := defaultScheduleZone(r)
			upd.Tz, note = &name, n
		}
	}
	sc, err := s.store.UpdateCommitment(workspaceID, item.ID, upd, roles)
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
	s.recordScheduleWrite(r, workspaceID, item, "schedule_updated", scheduleActivityMeta(sc.Kind, fields))
	s.writeItemCommitment(w, r, workspaceID, item, http.StatusOK, note)
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
		actor, _ := actorFromRequest(r)
		if err := s.store.EnqueueRenegotiationRefusedNotification(workspaceID, item.ID, currentUserID(r), actor == "agent"); err != nil {
			slog.Warn("enqueue renegotiation refusal notification", "workspace_id", workspaceID, "item_id", item.ID, "error", err)
		}
		writeDeadlineOwnerRequired(w)
		return
	}
	body, err := strictScheduleBody[models.CommitmentRenegotiateInput](r)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	result, err := s.store.RenegotiateCommitment(workspaceID, item.ID, currentUserID(r), body, commitmentAssigneeGuard(r, item))
	if errors.Is(err, store.ErrCommitmentChanged) {
		writeCommitmentChanged(w)
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
	s.writeItemCommitment(w, r, workspaceID, item, http.StatusOK, "")
}
