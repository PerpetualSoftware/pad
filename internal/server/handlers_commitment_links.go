package server

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Transport bodies contain only client-settable fields. Presence is retained
// for the offset union, including zero values from the other mode.
type commitmentOffsetBody struct {
	Mode           *string `json:"mode"`
	ElapsedSecs    *int64  `json:"elapsed_secs"`
	Days           *int    `json:"days"`
	ClockSecs      *int    `json:"clock_secs"`
	AllDay         *bool   `json:"all_day"`
	Zone           *string `json:"zone"`
	elapsedPresent bool
	civilPresent   bool
}

func (b *commitmentOffsetBody) UnmarshalJSON(raw []byte) error {
	type fields commitmentOffsetBody
	decoded, err := scheduleBodyBytes[fields](raw)
	if err != nil {
		return err
	}
	*b = commitmentOffsetBody(decoded)
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return err
	}
	_, b.elapsedPresent = members["elapsed_secs"]
	for _, name := range []string{"days", "clock_secs", "all_day", "zone"} {
		if _, ok := members[name]; ok {
			b.civilPresent = true
		}
	}
	return nil
}

type commitmentLinkCreateBody struct {
	Child                 *string               `json:"child"`
	ParentRole            *string               `json:"parent_role"`
	ChildRole             *string               `json:"child_role"`
	Offset                *commitmentOffsetBody `json:"offset"`
	CancelPolicy          *string               `json:"cancel_policy"`
	Preview               bool                  `json:"preview"`
	Reason                string                `json:"reason"`
	Revision              string                `json:"revision"`
	AcknowledgeViolations bool                  `json:"acknowledge_violations"`
}
type commitmentLinkUpdateBody struct {
	Offset                *commitmentOffsetBody `json:"offset"`
	CancelPolicy          *string               `json:"cancel_policy"`
	Preview               bool                  `json:"preview"`
	Reason                string                `json:"reason"`
	Revision              string                `json:"revision"`
	AcknowledgeViolations bool                  `json:"acknowledge_violations"`
}
type commitmentMovePreviewBody struct {
	Role   *string `json:"role"`
	To     *string `json:"to"`
	Reason string  `json:"reason"`
}
type commitmentMoveBody struct {
	Role                  *string `json:"role"`
	To                    *string `json:"to"`
	Reason                string  `json:"reason"`
	Revision              string  `json:"revision"`
	AcknowledgeViolations bool    `json:"acknowledge_violations"`
}
type commitmentLinkWriteView struct {
	Link  *models.ScheduleLink    `json:"link"`
	Moved models.LinkedMoveResult `json:"moved"`
}

func graphField(field, code string) error {
	return &store.ScheduleFieldError{Field: field, Code: code, Message: "Invalid commitment " + field}
}
func graphRequired(field string, value *string) error {
	if value == nil || strings.TrimSpace(*value) == "" {
		return graphField(field, "required")
	}
	return nil
}
func graphRole(field string, value *string) error {
	if err := graphRequired(field, value); err != nil {
		return err
	}
	switch *value {
	case "target", "earliest", "deadline", "occupied_until":
		return nil
	}
	return graphField(field, "enum")
}
func (b *commitmentOffsetBody) normalized() (models.ScheduleLinkOffset, error) {
	out := models.ScheduleLinkOffset{}
	if b == nil {
		return out, graphField("offset", "required")
	}
	if err := graphRequired("offset", b.Mode); err != nil {
		return out, err
	}
	out.Mode = *b.Mode
	switch out.Mode {
	case "elapsed":
		if b.civilPresent {
			return out, graphField("offset", "combination")
		}
		if b.ElapsedSecs == nil {
			return out, graphField("offset", "required")
		}
		out.ElapsedSecs = *b.ElapsedSecs
	case "civil":
		if b.elapsedPresent {
			return out, graphField("offset", "combination")
		}
		if b.Days == nil || b.ClockSecs == nil || b.AllDay == nil || b.Zone == nil {
			return out, graphField("offset", "required")
		}
		out.Days, out.ClockSecs, out.AllDay, out.Zone = *b.Days, *b.ClockSecs, *b.AllDay, *b.Zone
	default:
		return out, graphField("offset", "enum")
	}
	return out, store.ValidateScheduleLinkOffset(out)
}
func graphRevision(revision string) error {
	if revision == "" {
		return nil
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(revision)
	if err != nil || len(revision) != 43 || len(decoded) != 32 {
		return graphField("revision", "format")
	}
	return nil
}

func graphPolicy(policy *string) error {
	if policy != nil && *policy != models.LinkWaitingOn && *policy != models.LinkCancelChildOccurrence {
		return graphField("cancel_policy", "enum")
	}
	return nil
}

// resolveCommitmentGraphItem has no archived fallback. Visibility failures and
// disappearing ends use the graph error adapter at every door.
func (s *Server) resolveCommitmentGraphItem(w http.ResponseWriter, r *http.Request, workspaceID, ref string) *models.Item {
	item, err := s.store.ResolveItem(workspaceID, ref)
	if err != nil {
		writeInternalError(w, err)
		return nil
	}
	if item == nil {
		s.writeGraphError(w, r, store.ErrScheduleLinkNotFound)
		return nil
	}
	access, err := s.buildCommitmentAccess(r, s.store.Q(), workspaceID, item.ID, commitmentAccessLive)
	if err != nil {
		writeInternalError(w, err)
		return nil
	}
	if !access.Readable[item.ID] {
		s.writeGraphError(w, r, store.ErrScheduleLinkNotFound)
		return nil
	}
	return item
}
func (s *Server) graphAccessLoader(r *http.Request, workspaceID string, mode commitmentAccessMode, seeds ...string) store.CommitmentAccessLoader {
	return func(q store.Queryer) (models.CommitmentAccess, error) {
		merged := models.CommitmentAccess{Readable: map[string]bool{}, Editable: map[string]bool{}}
		for _, id := range seeds {
			access, err := s.buildCommitmentAccess(r, q, workspaceID, id, mode)
			if err != nil {
				return models.CommitmentAccess{}, err
			}
			for id, yes := range access.Readable {
				merged.Readable[id] = yes
			}
			for id, yes := range access.Editable {
				merged.Editable[id] = yes
			}
		}
		return merged, nil
	}
}
func (s *Server) writeGraphError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, store.ErrScheduleItemGone) || errors.Is(err, store.ErrScheduleNotFound) {
		err = store.ErrScheduleLinkNotFound
	}
	var field *store.ScheduleFieldError
	if errors.As(err, &field) && field.Code == "ended" {
		copy := *field
		copy.Code = "combination"
		err = &copy
	}
	if !s.writeCommitmentGraphError(w, r, err) {
		writeScheduleError(w, err)
	}
	return true
}

// graphResourceEnds is private: it deliberately returns neither end to the
// caller until the component builder has admitted both of them.
func (s *Server) graphResourceEnds(w http.ResponseWriter, r *http.Request, workspaceID, id string, gate bool) (string, string, bool) {
	if _, err := uuid.Parse(id); err != nil {
		s.writeGraphError(w, r, store.ErrScheduleLinkNotFound)
		return "", "", false
	}
	query := `SELECT p.item_id,c.item_id FROM schedule_links l JOIN item_schedules p ON p.id=l.parent_schedule_id JOIN item_schedules c ON c.id=l.child_schedule_id WHERE l.workspace_id=? AND l.id=?`
	if gate {
		query = `SELECT blocked_item_id,blocker_item_id FROM schedule_gates WHERE workspace_id=? AND id=?`
	}
	var a, b string
	err := s.store.Q().QueryRow(s.store.D().Rebind(query), workspaceID, id).Scan(&a, &b)
	if errors.Is(err, sql.ErrNoRows) {
		err = store.ErrScheduleLinkNotFound
	}
	if s.writeGraphError(w, r, err) {
		return "", "", false
	}
	return a, b, true
}
func graphEndsEditable(access models.CommitmentAccess, a, b string) error {
	if !access.Readable[a] || !access.Readable[b] {
		return store.ErrScheduleLinkNotFound
	}
	if !access.Editable[a] || !access.Editable[b] {
		return store.ErrLinkEditRequired
	}
	return nil
}
func (s *Server) handleListCommitmentLinks(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	item := s.resolveCommitmentGraphItem(w, r, ws, chi.URLParam(r, "itemSlug"))
	if item == nil {
		return
	}
	access, err := s.buildCommitmentAccess(r, s.store.Q(), ws, item.ID, commitmentAccessLive)
	if s.writeGraphError(w, r, err) {
		return
	}
	links, err := s.store.ListScheduleLinks(ws, item.ID, access)
	if s.writeGraphError(w, r, err) {
		return
	}
	writeJSON(w, 200, struct {
		Links []models.ScheduleLink `json:"links"`
	}{links})
}
func (s *Server) handleCreateCommitmentLink(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	item := s.resolveCommitmentGraphItem(w, r, ws, chi.URLParam(r, "itemSlug"))
	if item == nil {
		return
	}
	b, err := strictScheduleBody[commitmentLinkCreateBody](r)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	if err = graphRequired("child", b.Child); s.writeGraphError(w, r, err) {
		return
	}
	child := s.resolveCommitmentGraphItem(w, r, ws, *b.Child)
	if child == nil {
		return
	}
	loader := s.graphAccessLoader(r, ws, commitmentAccessLive, item.ID, child.ID)
	access, err := loader(s.store.Q())
	if s.writeGraphError(w, r, err) {
		return
	}
	if s.writeGraphError(w, r, graphEndsEditable(access, item.ID, child.ID)) {
		return
	}
	for _, v := range []struct {
		name  string
		value *string
	}{{"parent_role", b.ParentRole}, {"child_role", b.ChildRole}} {
		if s.writeGraphError(w, r, graphRole(v.name, v.value)) {
			return
		}
	}
	offset, err := b.Offset.normalized()
	if s.writeGraphError(w, r, err) || s.writeGraphError(w, r, graphPolicy(b.CancelPolicy)) {
		return
	}
	if s.writeGraphError(w, r, graphRevision(b.Revision)) {
		return
	}
	actor, _ := actorFromRequest(r)
	in := models.ScheduleLinkCreate{ParentItemID: item.ID, ChildItemID: child.ID, ParentRole: *b.ParentRole, ChildRole: *b.ChildRole, Offset: offset, Move: models.LinkWriteOptions{Actor: actor, ActorUserID: currentUserID(r), Preview: b.Preview, Reason: b.Reason, Revision: b.Revision, AcknowledgeViolations: b.AcknowledgeViolations, CommitmentAccessLoader: loader}}
	if b.CancelPolicy != nil {
		in.CancelPolicy = *b.CancelPolicy
	}
	result, err := s.store.CreateScheduleLink(ws, in, access)
	if errors.Is(err, store.ErrDeadlineApprovalRequired) && !b.Preview {
		s.afterCommitmentRefusal(r, ws, commitmentRefusalAttempt{create: &in})
	}
	if s.writeGraphError(w, r, err) {
		return
	}
	status := http.StatusCreated
	if b.Preview {
		status = http.StatusOK
	}
	writeJSON(w, status, commitmentLinkWriteView{result.Link, result.Moved})
}
func (s *Server) handleUpdateCommitmentLink(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "linkID")
	a, b, ok := s.graphResourceEnds(w, r, ws, id, false)
	if !ok {
		return
	}
	loader := s.graphAccessLoader(r, ws, commitmentAccessLive, a, b)
	access, err := loader(s.store.Q())
	if s.writeGraphError(w, r, err) || s.writeGraphError(w, r, graphEndsEditable(access, a, b)) {
		return
	}
	body, err := strictScheduleBody[commitmentLinkUpdateBody](r)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	if body.Offset == nil && body.CancelPolicy == nil {
		s.writeGraphError(w, r, graphField("offset", "required"))
		return
	}
	if s.writeGraphError(w, r, graphPolicy(body.CancelPolicy)) {
		return
	}
	if s.writeGraphError(w, r, graphRevision(body.Revision)) {
		return
	}
	actor, _ := actorFromRequest(r)
	in := models.ScheduleLinkUpdate{CancelPolicy: body.CancelPolicy, Move: models.LinkWriteOptions{Actor: actor, ActorUserID: currentUserID(r), Preview: body.Preview, Reason: body.Reason, Revision: body.Revision, AcknowledgeViolations: body.AcknowledgeViolations, CommitmentAccessLoader: loader}}
	if body.Offset != nil {
		offset, err := body.Offset.normalized()
		if s.writeGraphError(w, r, err) {
			return
		}
		in.Offset = &offset
	}
	result, err := s.store.UpdateScheduleLink(ws, id, in, access)
	if errors.Is(err, store.ErrDeadlineApprovalRequired) && !body.Preview {
		s.afterCommitmentRefusal(r, ws, commitmentRefusalAttempt{linkID: id, update: &in})
	}
	if s.writeGraphError(w, r, err) {
		return
	}
	writeJSON(w, 200, commitmentLinkWriteView{result.Link, result.Moved})
}
func (s *Server) handleDeleteCommitmentLink(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "linkID")
	a, b, ok := s.graphResourceEnds(w, r, ws, id, false)
	if !ok {
		return
	}
	loader := s.graphAccessLoader(r, ws, commitmentAccessRemove, a, b)
	access, err := loader(s.store.Q())
	if s.writeGraphError(w, r, err) {
		return
	}
	if s.writeGraphError(w, r, s.store.DeleteScheduleLink(ws, id, access, loader)) {
		return
	}
	w.WriteHeader(204)
}
func (s *Server) handlePreviewCommitmentMove(w http.ResponseWriter, r *http.Request) {
	s.handleCommitmentMove(w, r, true)
}
func (s *Server) handleMoveCommitment(w http.ResponseWriter, r *http.Request) {
	s.handleCommitmentMove(w, r, false)
}
func (s *Server) handleCommitmentMove(w http.ResponseWriter, r *http.Request, preview bool) {
	ws, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	item := s.resolveCommitmentGraphItem(w, r, ws, chi.URLParam(r, "itemSlug"))
	if item == nil {
		return
	}
	var b commitmentMoveBody
	var err error
	if preview {
		var p commitmentMovePreviewBody
		p, err = strictScheduleBody[commitmentMovePreviewBody](r)
		b.Role, b.To, b.Reason = p.Role, p.To, p.Reason
	} else {
		b, err = strictScheduleBody[commitmentMoveBody](r)
	}
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	if s.writeGraphError(w, r, graphRole("role", b.Role)) || s.writeGraphError(w, r, graphRequired("to", b.To)) {
		return
	}
	if _, err := time.Parse(time.RFC3339, *b.To); err != nil {
		s.writeGraphError(w, r, graphField("to", "format"))
		return
	}
	loader := s.graphAccessLoader(r, ws, commitmentAccessLive, item.ID)
	access, err := loader(s.store.Q())
	if s.writeGraphError(w, r, err) || s.writeGraphError(w, r, graphEndsEditable(access, item.ID, item.ID)) {
		return
	}
	if s.writeGraphError(w, r, graphRevision(b.Revision)) {
		return
	}
	actor, _ := actorFromRequest(r)
	in := models.LinkedMoveInput{ItemID: item.ID, Role: *b.Role, To: *b.To, Actor: actor, ActorUserID: currentUserID(r), Reason: b.Reason, Preview: preview, Revision: b.Revision, AcknowledgeViolations: b.AcknowledgeViolations, Access: access, CommitmentAccessLoader: loader}
	if in.Role == "deadline" {
		allowed, err := s.canRenegotiateCommitment(r, ws, item)
		if s.writeGraphError(w, r, err) {
			return
		}
		if !allowed {
			writeDeadlineOwnerRequired(w)
			return
		}
		in.ExpectedAssignee = commitmentAssigneeGuard(r, item)
	}
	result, err := s.store.MoveLinkedDate(ws, in)
	if errors.Is(err, store.ErrDeadlineApprovalRequired) && !preview {
		s.afterCommitmentRefusal(r, ws, commitmentRefusalAttempt{move: &in})
	}
	if s.writeGraphError(w, r, err) {
		return
	}
	writeJSON(w, 200, result)
}
