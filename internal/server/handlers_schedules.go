package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/usertz"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
	"github.com/go-chi/chi/v5"
)

// Item schedule handlers (TASK-4102 PR2). HTTP only: the store owns every
// check (store.compileScheduleSpec is the single door), so this file maps
// requests onto it and its errors back onto the API's vocabulary. It adds no
// second parser for rules or walls.

// scheduleOccurrenceView is next_occurrence.
type scheduleOccurrenceView struct {
	At   string `json:"at"`
	Wall string `json:"wall"`
}

// scheduleView is the API shape of a schedule. It is NOT models.ItemSchedule,
// which carries workspace_id, item_id and the internal floating bound (next_at).
type scheduleView struct {
	ID             string                  `json:"id"`
	ItemRef        string                  `json:"item_ref"`
	Dtstart        string                  `json:"dtstart"`
	Tz             *string                 `json:"tz,omitempty"`
	Floating       bool                    `json:"floating"`
	Rrule          *string                 `json:"rrule,omitempty"`
	RDates         []string                `json:"rdates"`
	ExDates        []string                `json:"exdates"`
	Anchor         string                  `json:"anchor"`
	Audience       string                  `json:"audience"`
	LeadMinutes    int                     `json:"lead_minutes"`
	NextOccurrence *scheduleOccurrenceView `json:"next_occurrence,omitempty"`
	NextOverrideAt *string                 `json:"next_override_at,omitempty"`
	// MissedCount is the occurrences that passed unresolved AFTER the
	// outstanding one (it is not counted), computed on read. Stale is
	// MissedCount >= 2. Truncated says the count is a floor (TASK-4103).
	MissedCount     int     `json:"missed_count,omitempty"`
	MissedTruncated bool    `json:"missed_truncated,omitempty"`
	Stale           bool    `json:"stale,omitempty"`
	EndedAt         *string `json:"ended_at,omitempty"`
	ErrorCode       *string `json:"error_code,omitempty"`
	ReminderID      *string `json:"reminder_id,omitempty"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	TimezoneNote    string  `json:"timezone_note,omitempty"`
}

// buildScheduleView renders a stored schedule. A pinned schedule's next_at is
// the occurrence itself. An ARMED floating one stores only the earliest instant
// any zone could give next_wall, which is never shown: next_occurrence.at is
// the wall read in the CALLER's zone (the tick uses the recipient's). Once
// fired, next_at is the exact instant and is what is shown.
func (s *Server) buildScheduleView(r *http.Request, item *models.Item, sc *models.ItemSchedule) scheduleView {
	ref := item.Ref
	if ref == "" {
		ref = item.Slug // a legacy item with no ref (see handleCreateItemReminder's resolver)
	}
	v := scheduleView{
		ID: sc.ID, ItemRef: ref, Dtstart: sc.Dtstart,
		Tz: sc.Tz, Floating: sc.Floating, Rrule: sc.Rrule,
		RDates: nonNilStrings(sc.RDates), ExDates: nonNilStrings(sc.ExDates),
		Anchor: sc.Anchor, Audience: sc.Audience, LeadMinutes: sc.LeadMinutes,
		NextOverrideAt: sc.NextOverrideAt, EndedAt: sc.EndedAt, ErrorCode: sc.ErrorCode,
		ReminderID: sc.ReminderID, CreatedAt: sc.CreatedAt, UpdatedAt: sc.UpdatedAt,
	}
	if sc.EndedAt == nil && sc.NextWall != nil {
		// One reading with the reminder reads (store.OutstandingInstant): a
		// FIRED floating schedule shows the instant it fired for, not today's
		// reading of the wall.
		view := viewerZone(r).Loc
		if at, ok, err := store.OutstandingInstant(sc, view); err == nil && ok {
			v.NextOccurrence = &scheduleOccurrenceView{At: at.Format(time.RFC3339), Wall: *sc.NextWall}
		}
		if n, truncated, err := store.ScheduleMissed(sc, view, time.Now()); err != nil {
			// The view still answers without the count. A rule that compiles but
			// hits a cap fails the same way on every read, so the warning is
			// logged once per schedule edit, not once per GET (TASK-4103 C2b-4).
			if s.firstMissedWarning(sc.ID, sc.UpdatedAt) {
				slog.Warn("schedule view: missed count failed", "schedule_id", sc.ID, "error", err)
			}
		} else {
			s.missedWarned.LoadAndDelete(sc.ID) // recovered: a later failure warns afresh
			v.MissedCount, v.MissedTruncated, v.Stale = n, truncated, n >= 2
		}
	}
	return v
}

// firstMissedWarning reports whether this read is the first to see the missed
// count fail at this edit of the schedule, recording it if so. Atomic, so
// concurrent reads log once; and it only moves forward, so a read of an older
// snapshot (updated_at is RFC3339 to the second, which orders as a string)
// neither logs nor undoes a newer edit's record (TASK-4103 C2b-i r1-1).
func (s *Server) firstMissedWarning(scheduleID, updatedAt string) bool {
	for {
		prev, loaded := s.missedWarned.LoadOrStore(scheduleID, updatedAt)
		if !loaded {
			return true
		}
		if p, _ := prev.(string); p >= updatedAt {
			return false
		}
		if s.missedWarned.CompareAndSwap(scheduleID, prev, updatedAt) {
			return true
		}
	}
}

// writeScheduleNotFound is the 404 for an item with no schedule.
func writeScheduleNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "Schedule not found")
}

func writeScheduleBodyError(w http.ResponseWriter) {
	writeError(w, http.StatusBadRequest, "invalid_body", "Request body must be JSON")
}

func writeScheduleRequired(w http.ResponseWriter, field string) {
	writeError2(w, http.StatusBadRequest, "validation_error",
		"schedule "+field+" cannot be null",
		map[string]interface{}{"field": field, "code": "required"})
}

// writeScheduleError maps a store error from a schedule write onto the API and
// reports whether it handled err. Nothing is persisted on any of these.
func writeScheduleError(w http.ResponseWriter, err error) bool {
	var mal *schedule.MalformedError
	var capErr *schedule.CapError
	var fld *store.ScheduleFieldError
	switch {
	case err == nil:
		return false
	case errors.As(err, &mal):
		writeError2(w, http.StatusBadRequest, "validation_error", mal.Error(),
			map[string]interface{}{"code": string(mal.Code), "part": mal.Part, "value": mal.Value})
	case errors.As(err, &capErr):
		switch capErr.Kind {
		case schedule.CapScan, schedule.CapStall:
			// Input-caused: the same rule fails the same way every time.
			writeError2(w, http.StatusBadRequest, "validation_error", capErr.Error(),
				map[string]interface{}{"code": "cap_" + string(capErr.Kind)})
		case schedule.CapTime:
			// Load-dependent, so not a property of the input.
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusServiceUnavailable, "schedule_busy",
				"The schedule could not be computed in time; retry")
		default:
			// window and instances cannot happen on a write (NextAfter only);
			// anything softer would hide a bug.
			writeInternalError(w, err)
		}
	case errors.As(err, &fld):
		writeError2(w, http.StatusBadRequest, "validation_error", fld.Error(),
			map[string]interface{}{"field": fld.Field, "code": fld.Code})
	case errors.Is(err, store.ErrScheduleExhausted):
		writeError2(w, http.StatusBadRequest, "validation_error",
			"The rule has no occurrence at or after now",
			map[string]interface{}{"code": "exhausted"})
	case errors.Is(err, store.ErrScheduleItemGone):
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
	case errors.Is(err, store.ErrScheduleExists):
		writeError(w, http.StatusConflict, "schedule_exists",
			"This item already has a schedule. PATCH changes it and DELETE removes it.")
	case errors.Is(err, store.ErrReminderManaged):
		writeReminderManaged(w, "")
	case errors.Is(err, store.ErrHardDeadlineRenegotiate):
		writeError2(w, http.StatusConflict, "hard_deadline",
			"A hard deadline changes only by renegotiation: POST .../commitment/renegotiate, with a reason",
			map[string]interface{}{"field": "deadline", "code": "combination"})
	default:
		writeInternalError(w, err)
	}
	return true
}

// writeScheduleItemGone answers a write whose item went away under it (a move
// archived it while the write waited): the answer the door's own pre-check gives,
// 409 archived; 404 only if it is truly gone. It reports whether it answered.
func (s *Server) writeScheduleItemGone(w http.ResponseWriter, r *http.Request, workspaceID string, err error) bool {
	if !errors.Is(err, store.ErrScheduleItemGone) {
		return false
	}
	s.writeItemResolveError(w, r, workspaceID, chi.URLParam(r, "itemSlug"))
	return true
}

// defaultScheduleZone is the creator's zone for a schedule that names neither
// a zone nor floating. It returns the zone name and the note that explains an
// unset account zone ("" when one is set).
func defaultScheduleZone(r *http.Request) (name, note string) {
	z := viewerZone(r)
	return z.Name, usertz.Note(z)
}

// strictScheduleBody decodes a request body with the transport rules every
// decodeJSON caller gets, then refuses a member T does not declare, so a typo
// such as `rrulee` is a 400 rather than a one-shot schedule.
func strictScheduleBody[T any](r *http.Request) (T, error) {
	raw, err := readBodyForDecode(r, defaultJSONBodyLimit)
	if err != nil {
		var out T
		return out, err
	}
	return scheduleBodyBytes[T](raw)
}

var errScheduleBodyNotObject = errors.New("schedule body is not a JSON object")

// scheduleBodyBytes also refuses a body that is not an object: a top-level
// null decodes into a struct as a no-op, so it would read as {} (TASK-4102
// PR2 r2, H-Q1). strictBodyBytes is shared with push devices and left as is.
func scheduleBodyBytes[T any](raw []byte) (T, error) {
	if t := bytes.TrimLeft(raw, " \t\r\n"); len(t) == 0 || t[0] != '{' {
		var out T
		return out, errScheduleBodyNotObject
	}
	return strictBodyBytes[T](raw)
}

// scheduleActivityMeta is the names-only metadata of a schedule write.
func scheduleActivityMeta(kind string, fields []string) string {
	m := map[string]any{"kind": kind}
	if fields != nil {
		m["fields"] = fields
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// recordScheduleWrite logs the activity and publishes the item event. Both are
// best-effort and non-transactional, like a verification. An item_updated SSE
// event rides the existing type; a schedule write does not bump the item seq.
func (s *Server) recordScheduleWrite(r *http.Request, workspaceID string, item *models.Item, action, meta string) {
	s.logActivityWithMeta(workspaceID, item.ID, action, r, meta)
	s.publishItemVerified(workspaceID, item, r)
}

func isJSONNull(raw json.RawMessage) bool { return string(raw) == "null" }

func scheduleUpdateFromMembers(w http.ResponseWriter, raws map[string]json.RawMessage) (upd models.ScheduleUpdate, fields []string, ok bool) {
	names := make([]string, 0, len(raws))
	for name := range raws {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		raw := raws[name]
		if raw == nil {
			continue
		}
		fields = append(fields, name)
		null := isJSONNull(raw)
		clearable := name == "rrule" || name == "rdates" || name == "exdates"
		if null && !clearable {
			writeScheduleRequired(w, name)
			return upd, nil, false
		}
		var err error
		switch name {
		case "dtstart", "tz", "anchor", "audience", "rrule":
			var v string
			if !null {
				err = json.Unmarshal(raw, &v)
			}
			p := &v
			switch name {
			case "dtstart":
				upd.Dtstart = p
			case "tz":
				upd.Tz = p
			case "anchor":
				upd.Anchor = p
			case "audience":
				upd.Audience = p
			case "rrule":
				upd.Rrule = p
			}
		case "floating":
			var v bool
			err = json.Unmarshal(raw, &v)
			upd.Floating = &v
		case "lead_minutes":
			var v int
			err = json.Unmarshal(raw, &v)
			upd.LeadMinutes = &v
		case "rdates", "exdates":
			v := []string{}
			if !null {
				err = json.Unmarshal(raw, &v)
			}
			if v == nil {
				v = []string{}
			}
			if name == "rdates" {
				upd.RDates = &v
			} else {
				upd.ExDates = &v
			}
		}
		if err != nil {
			writeScheduleBodyError(w)
			return upd, nil, false
		}
	}
	return upd, fields, true
}

// handleDeleteItemCommitment removes an item's schedule and its managed reminder.
// DELETE /api/v1/workspaces/{slug}/items/{itemSlug}/commitment
func (s *Server) handleDeleteItemCommitment(w http.ResponseWriter, r *http.Request) {
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
	cur, err := s.store.GetSchedule(workspaceID, item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if cur == nil {
		writeScheduleNotFound(w)
		return
	}
	// Deleting a native schedule that has a deadline, then posting a new one,
	// would be a way around the renegotiation rule: the same people who may
	// renegotiate may remove it. The store reads the deadline under the
	// schedule's lock; the item's own delete is a different door.
	allowed, err := s.canRenegotiateCommitment(r, workspaceID, item)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	removed, err := s.store.DeleteScheduleChecked(workspaceID, item.ID, allowed, commitmentAssigneeGuard(r, item))
	if s.writeScheduleItemGone(w, r, workspaceID, err) {
		return
	}
	if errors.Is(err, store.ErrDeadlineDeleteChanged) {
		writeCommitmentChanged(w)
		return
	}
	if errors.Is(err, store.ErrDeadlineDeleteAuthority) {
		writeDeadlineOwnerRequired(w)
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if !removed {
		writeScheduleNotFound(w)
		return
	}
	s.missedWarned.Delete(cur.ID)
	s.recordScheduleWrite(r, workspaceID, item, "schedule_deleted", scheduleActivityMeta(cur.Kind, nil))
	writeJSON(w, http.StatusOK, map[string]any{"id": cur.ID, "deleted": true})
}

// scheduleResolveBody is POST .../commitment/resolve. Every member is a pointer so
// a missing one is told apart from an empty one.
type scheduleResolveBody struct {
	OccurrenceAt *string `json:"occurrence_at"`
	State        *string `json:"state"`
	Comment      *string `json:"comment"`
	Until        *string `json:"until"`
}

func writeResolveField(w http.ResponseWriter, field, code, msg string) {
	writeError2(w, http.StatusBadRequest, "validation_error", msg,
		map[string]interface{}{"field": field, "code": code})
}

// writeResolveError maps the errors only a resolve raises, then the shared
// schedule vocabulary. A stale answer carries the CURRENT outstanding
// occurrence so the caller can retry with it; a floating one is read in the
// caller's zone, like the view.
func writeResolveError(w http.ResponseWriter, r *http.Request, err error) bool {
	var stale *store.ErrStaleOccurrence
	var broken *store.ErrScheduleBroken
	switch {
	case errors.As(err, &stale):
		details := map[string]interface{}{"ended": stale.Ended()}
		if !stale.Ended() && stale.Schedule.NextWall != nil {
			if at, ok, err := store.OutstandingInstant(stale.Schedule, viewerZone(r).Loc); err == nil && ok {
				details["next_occurrence"] = scheduleOccurrenceView{At: at.Format(time.RFC3339), Wall: *stale.Schedule.NextWall}
			}
		}
		writeError2(w, http.StatusConflict, "stale",
			"That is not the schedule's outstanding occurrence; it was resolved, moved or ended since you read it", details)
		return true
	case errors.As(err, &broken):
		writeError2(w, http.StatusConflict, "schedule_error",
			"The schedule's rule no longer works; change its timing to repair it",
			map[string]interface{}{"error_code": broken.Code})
		return true
	case errors.Is(err, store.ErrScheduleNotFound):
		writeScheduleNotFound(w)
		return true
	}
	return writeScheduleError(w, err)
}

// parseResolveInstant reads an RFC3339 instant member.
func parseResolveInstant(w http.ResponseWriter, field string, raw *string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, *raw)
	if err != nil {
		writeResolveField(w, field, "format", field+" must be an RFC3339 instant")
		return time.Time{}, false
	}
	return t, true
}

// resolveItemSchedule resolves through the commitment door and publishes the
// existing schedule and comment side effects.
func (s *Server) resolveItemSchedule(w http.ResponseWriter, r *http.Request) {
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
	b, err := strictScheduleBody[commitmentResolveBody](r)
	body, actualDoneAt := b.scheduleResolveBody, b.ActualDoneAt
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	if body.OccurrenceAt == nil {
		writeResolveField(w, "occurrence_at", "required", "occurrence_at is required")
		return
	}
	if body.State == nil {
		writeResolveField(w, "state", "required", "state is required")
		return
	}
	in := store.ResolveInput{State: *body.State, View: viewerZone(r).Loc}
	if in.OccurrenceAt, ok = parseResolveInstant(w, "occurrence_at", body.OccurrenceAt); !ok {
		return
	}
	if body.Until != nil {
		u, ok := parseResolveInstant(w, "until", body.Until)
		if !ok {
			return
		}
		in.Until = &u
	}
	if actualDoneAt != nil {
		actual, ok := parseResolveInstant(w, "actual_done_at", actualDoneAt)
		if !ok {
			return
		}
		if in.State != "done" {
			writeResolveField(w, "actual_done_at", "state", "actual_done_at cannot be set unless state is done")
			return
		}
		in.ActualDoneAt = &actual
	}
	in.Actor, _ = actorFromRequest(r)
	var commentInput models.CommentCreate
	if body.Comment != nil && strings.TrimSpace(*body.Comment) != "" {
		commentInput.Body = *body.Comment
		stampCommentAttribution(r, &commentInput)
		in.Actor = commentInput.CreatedBy
		in.Comment = &store.ResolveComment{
			UserID:   currentUserID(r),
			Input:    commentInput,
			Activity: s.activityForRequest(workspaceID, item.ID, "commented", r, ""),
		}
	}

	res, err := s.store.ResolveOccurrence(workspaceID, item.ID, in)
	if s.writeScheduleItemGone(w, r, workspaceID, err) || writeResolveError(w, r, err) {
		return
	}

	s.recordScheduleWrite(r, workspaceID, item, "schedule_resolved", resolveActivityMeta(res.Schedule.Kind, in.State))
	resolution := map[string]any{
		"occurrence_at":   in.OccurrenceAt.UTC().Format(time.RFC3339),
		"state":           in.State,
		"missed_recorded": res.MissedRecorded,
	}
	if res.Comment != nil {
		resolution["comment_id"] = res.Comment.ID
		s.touchUserWrite(r, writeKindComment)
		s.publishCommentEvent(sseCommentCreated, workspaceID, item.ID, res.Comment.ID, item.Title, item.CollectionSlug, commentInput.CreatedBy, commentInput.Source)
		s.publishWatchNotification(watchevents.Notification{
			WorkspaceID:  workspaceID,
			ItemID:       item.ID,
			CollectionID: item.CollectionID,
			ItemRef:      item.Ref,
			Kind:         watchevents.KindComment,
			Actor:        commentInput.CreatedBy,
			ActorName:    actorNameFromRequest(r),
			Summary:      truncateForSummary(res.Comment.Body, 120),
		})
	}
	v, err := s.buildCommitmentView(r, workspaceID, item, res.Schedule, "")
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"commitment": v, "resolution": resolution})
}

// resolveActivityMeta is the names-only metadata of a resolution: the schedule's
// kind and the state chosen. No comment text, wall, instant or rule.
func resolveActivityMeta(kind, state string) string {
	b, _ := json.Marshal(map[string]string{"kind": kind, "state": state})
	return string(b)
}
