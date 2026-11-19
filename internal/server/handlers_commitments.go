package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"

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
	Links []models.ScheduleLink    `json:"links"`
	Gates *models.ScheduleGateView `json:"gates"`
	// Editable is set on GET only: a display hint saying whether this caller
	// may POST/PATCH the commitment. It authorises nothing; writes re-check.
	Editable *bool `json:"editable,omitempty"`
	// CommitmentRevision is the optimistic-concurrency token a PATCH or renegotiate
	// may send back as expected_revision. Set only where the view's schedule was
	// read fresh with GetScheduleRevision; absent on the resolve response.
	CommitmentRevision string `json:"commitment_revision,omitempty"`
}

// commitmentWriteAllowed is the single decision for writing an item's
// commitment. Its consumers: the POST and PATCH commitment doors
// (requireCommitmentWrite), the GET commitment and gates `editable` flags,
// and the last_resolution redaction in buildCommitmentView. The scope check
// uses a fixed write method, never r.Method, so a read-scoped token reading
// with GET is judged as the writer it is not.
func (s *Server) commitmentWriteAllowed(r *http.Request, workspaceID string, item *models.Item) (bool, error) {
	if !tokenScopeAllows(TokenScopesFromContext(r.Context()), http.MethodPost, "") {
		return false, nil
	}
	if !appWriteAllows(r, item.CollectionID) {
		return false, nil
	}
	return s.canEditInCollection(r, workspaceID, item.ID, item.CollectionID)
}

// requireCommitmentWrite answers a refused commitment write with the same
// bytes as the middleware token-scope gate and requireEditPermission.
func (s *Server) requireCommitmentWrite(w http.ResponseWriter, r *http.Request, workspaceID string, item *models.Item) bool {
	ok, err := s.commitmentWriteAllowed(r, workspaceID, item)
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	if ok {
		return true
	}
	if !tokenScopeAllows(TokenScopesFromContext(r.Context()), http.MethodPost, "") {
		// Unreachable behind the middleware's scope gate; kept as defence.
		writeForbidden(w, "forbidden", "Token scope does not permit this action", forbiddenReasonTokenScope)
		return false
	}
	s.writeEditRefusal(w, r, item.CollectionID)
	return false
}

func (s *Server) buildCommitmentView(r *http.Request, workspaceID string, item *models.Item, sc *models.ItemSchedule, note string) (*commitmentView, error) {
	access, err := s.buildCommitmentAccess(r, s.store.Q(), workspaceID, item.ID, commitmentAccessLive)
	if err != nil {
		return nil, err
	}
	c, err := s.store.GetCommitmentForReaders(workspaceID, item.ID, func(id string) bool { return access.Readable[id] }, s.itemVisibleForIncludingTrashed(r, workspaceID))
	if err != nil {
		return nil, err
	}
	// Eligibility is exposed only by the commitment read and reopen POST, and
	// by create, so a GET reproduces the create body apart from the GET-only
	// `editable` hint. A new schedule has no checkpoint, so create always
	// answers reopen_unavailable.
	var editable *bool
	createCall := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commitment")
	if r.Method == http.MethodGet || createCall || strings.HasSuffix(r.URL.Path, "/commitment/reopen") {
		writable, err := s.commitmentWriteAllowed(r, workspaceID, item)
		if err != nil {
			return nil, err
		}
		if !writable {
			c.LastResolution = nil
		}
		if r.Method == http.MethodGet {
			flag := item.DeletedAt == nil && writable
			editable = &flag
		}
	} else {
		c.LastResolution = nil
	}
	v, err := s.buildScheduleView(r, item, sc)
	if err != nil {
		return nil, err
	}
	v.TimezoneNote = note
	out := &commitmentView{Commitment: c, scheduleView: v, Links: []models.ScheduleLink{}, Gates: &models.ScheduleGateView{State: models.GateStateReleased, Gates: []models.ScheduleGate{}}, Editable: editable}
	if access.Readable[item.ID] {
		out.Links, err = s.store.ListScheduleLinks(workspaceID, item.ID, access)
		if err != nil {
			return nil, err
		}
		out.Gates, err = s.store.ScheduleGates(workspaceID, item.ID, access)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Server) writeItemCommitment(w http.ResponseWriter, r *http.Request, workspaceID string, item *models.Item, status int, note string) {
	// The token and sc share one snapshot. The rest of the view (roles, links,
	// gates) is read afterwards from the pool, so it is never older than the
	// token; a write landing in between only makes the token stale, which costs
	// a spurious 409 at worst and never a lost update.
	sc, rev, err := s.store.GetScheduleRevision(workspaceID, item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if sc == nil {
		writeScheduleNotFound(w)
		return
	}
	v, err := s.buildCommitmentView(r, workspaceID, item, sc, note)
	if errors.Is(err, store.ErrScheduleNotFound) {
		writeScheduleNotFound(w)
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	v.CommitmentRevision = rev
	writeJSON(w, status, v)
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
	for name := range members {
		if strings.EqualFold(name, "kind") || strings.EqualFold(name, "dtend") {
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
	// Archived items are readable.
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
	if !s.requireCommitmentWrite(w, r, workspaceID, item) {
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
	if s.writeScheduleItemGone(w, r, workspaceID, err) || s.writeScheduleError(w, r, err) {
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
	// ExpectedRevision is a precondition, not a change member (TASK-4630).
	ExpectedRevision json.RawMessage `json:"expected_revision,omitempty"`
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
	if !s.requireCommitmentWrite(w, r, workspaceID, item) {
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
	// Null and "" are absent, like renegotiate's revision. Deleting the key keeps
	// it out of the required check, the activity fields and the update itself.
	expected := ""
	if raw, ok := members["expected_revision"]; ok {
		delete(members, "expected_revision")
		if !isJSONNull(raw) {
			if err := json.Unmarshal(raw, &expected); err != nil {
				writeScheduleBodyError(w)
				return
			}
		}
	}
	if len(members) == 0 {
		writeScheduleRequired(w, "body")
		return
	}
	fields := make([]string, 0, len(members))
	for name := range members {
		fields = append(fields, name)
	}
	sort.Strings(fields)
	var roles *models.CommitmentRolesPatch
	if body.Roles != nil {
		if isJSONNull(body.Roles) {
			writeScheduleRequired(w, "roles")
			return
		}
		roles = &models.CommitmentRolesPatch{}
		parsed, err := scheduleBodyBytes[models.CommitmentRolesPatch](body.Roles)
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
	sc, err := s.store.UpdateCommitmentExpecting(workspaceID, item.ID, upd, roles, expected)
	if s.writeScheduleItemGone(w, r, workspaceID, err) {
		return
	}
	if errors.Is(err, store.ErrScheduleNotFound) {
		writeScheduleNotFound(w)
		return
	}
	if s.writeScheduleError(w, r, err) {
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
	s.resolveItemSchedule(w, r)
}

func writeDeadlineOwnerRequired(w http.ResponseWriter) {
	writeError2(w, http.StatusForbidden, "deadline_owner_required",
		"A schedule with a hard deadline is removed or changed only by the item's assignee or a workspace owner; to remove or change a hard deadline use POST .../commitment/renegotiate",
		map[string]interface{}{"field": "deadline", "code": "authority"})
}

// skipDeadlineOwnerMessage is the 403 text of a skip before a hard deadline.
const skipDeadlineOwnerMessage = "Only the assignee or a workspace owner can skip this occurrence before its hard deadline."

// skipCommitmentChangedMessage is the 409 text of a skip whose assignee snapshot
// went stale: there is no preview to redo, the caller re-reads and tries again.
const skipCommitmentChangedMessage = "The item's assignee changed while the skip was being checked; reload the occurrence and skip again"

func writeSkipCommitmentChanged(w http.ResponseWriter) {
	writeError2(w, http.StatusConflict, "commitment_changed", skipCommitmentChangedMessage,
		map[string]interface{}{"field": "revision", "code": "stale"})
}

func writeSkipDeadlineOwnerRequired(w http.ResponseWriter) {
	writeError2(w, http.StatusForbidden, "deadline_owner_required", skipDeadlineOwnerMessage,
		map[string]interface{}{"field": "deadline", "code": "authority"})
}

// commitmentRenegotiateBody declares only client-settable members.
type commitmentRenegotiateBody struct {
	Reason                string                           `json:"reason"`
	Scope                 string                           `json:"scope"`
	Deadline              *string                          `json:"deadline"`
	DeadlineOffset        *models.CommitmentDeadlineOffset `json:"deadline_offset"`
	DeadlineBehavior      *string                          `json:"deadline_behavior"`
	RemoveDeadline        bool                             `json:"remove_deadline"`
	Revision              string                           `json:"revision"`
	Preview               bool                             `json:"preview"`
	AcknowledgeViolations bool                             `json:"acknowledge_violations"`
	ExpectedRevision      string                           `json:"expected_revision"`
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
	request, err := strictScheduleBody[commitmentRenegotiateBody](r)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	body := models.CommitmentRenegotiateInput{Reason: request.Reason, Scope: request.Scope, Deadline: request.Deadline, DeadlineOffset: request.DeadlineOffset, DeadlineBehavior: request.DeadlineBehavior, RemoveDeadline: request.RemoveDeadline, Revision: request.Revision, ExpectedRevision: request.ExpectedRevision, Preview: request.Preview, AcknowledgeViolations: request.AcknowledgeViolations}
	body.Access, err = s.buildCommitmentAccess(r, s.store.Q(), workspaceID, item.ID, commitmentAccessLive)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	body.ActorUserID = currentUserID(r)
	body.CommitmentAccessLoader = func(q store.Queryer) (models.CommitmentAccess, error) {
		return s.buildCommitmentAccess(r, q, workspaceID, item.ID, commitmentAccessLive)
	}
	result, err := s.store.RenegotiateCommitment(workspaceID, item.ID, currentUserID(r), body, commitmentAssigneeGuard(r, item))
	if errors.Is(err, store.ErrDeadlineApprovalRequired) && !body.Preview && body.Deadline != nil {
		actor, _ := actorFromRequest(r)
		s.afterCommitmentRefusal(r, workspaceID, commitmentRefusalAttempt{move: &models.LinkedMoveInput{ItemID: item.ID, Role: "deadline", To: *body.Deadline, Reason: body.Reason, Actor: actor, ActorUserID: body.ActorUserID, ExpectedAssignee: commitmentAssigneeGuard(r, item)}})
	}
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
	if s.writeScheduleError(w, r, err) {
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
