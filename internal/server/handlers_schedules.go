package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
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

// scheduleMembers is the fixed allowlist of request member names, in the order
// an update's activity entry lists them. The activity metadata is built from
// the intersection of the body's keys with this list, so it holds names only
// and can never carry rule text or a wall.
var scheduleMembers = []string{
	"kind", "dtstart", "dtend", "tz", "floating", "rrule", "rdates", "exdates",
	"anchor", "audience", "lead_minutes",
}

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
	Kind           string                  `json:"kind"`
	Dtstart        string                  `json:"dtstart"`
	Dtend          *string                 `json:"dtend,omitempty"`
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
		ID: sc.ID, ItemRef: ref, Kind: sc.Kind, Dtstart: sc.Dtstart, Dtend: sc.Dtend,
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
	default:
		writeInternalError(w, err)
	}
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

// strictScheduleCreateBody is strictScheduleBody for POST, which also reports
// a required member sent as null: decoding null into a string leaves it
// unset, so without this `kind: null` read as an enum error and `dtstart:
// null` as a wall error rather than PATCH's `required`. Null on an optional
// member stays the member left out (TASK-4102 PR2 r1, C-M1).
func strictScheduleCreateBody(r *http.Request) (req models.ScheduleCreate, nullField string, err error) {
	raw, err := readBodyForDecode(r, defaultJSONBodyLimit)
	if err != nil {
		return req, "", err
	}
	if req, err = scheduleBodyBytes[models.ScheduleCreate](raw); err != nil {
		return req, "", err
	}
	var required struct {
		Kind    json.RawMessage `json:"kind"`
		Dtstart json.RawMessage `json:"dtstart"`
	}
	if err := json.Unmarshal(raw, &required); err != nil {
		return req, "", err
	}
	switch {
	case string(required.Kind) == "null":
		return req, "kind", nil
	case string(required.Dtstart) == "null":
		return req, "dtstart", nil
	}
	return req, "", nil
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

// handleGetItemSchedule returns an item's schedule.
// GET /api/v1/workspaces/{slug}/items/{itemSlug}/schedule
//
// Like the reminders list, an ARCHIVED item resolves read-only.
func (s *Server) handleGetItemSchedule(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
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
	sc, err := s.store.GetSchedule(workspaceID, item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if sc == nil {
		writeScheduleNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, s.buildScheduleView(r, item, sc))
}

// handleCreateItemSchedule creates an item's schedule.
// POST /api/v1/workspaces/{slug}/items/{itemSlug}/schedule
func (s *Server) handleCreateItemSchedule(w http.ResponseWriter, r *http.Request) {
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
	req, nullField, err := strictScheduleCreateBody(r)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	if nullField != "" {
		writeScheduleRequired(w, nullField)
		return
	}
	note := ""
	if !req.Floating && req.Tz == "" {
		req.Tz, note = defaultScheduleZone(r)
	}
	sc, err := s.store.CreateSchedule(workspaceID, item.ID, req)
	if writeScheduleError(w, err) {
		return
	}
	s.recordScheduleWrite(r, workspaceID, item, "schedule_created", scheduleActivityMeta(sc.Kind, nil))
	v := s.buildScheduleView(r, item, sc)
	v.TimezoneNote = note
	writeJSON(w, http.StatusCreated, v)
}

// schedulePatchBody holds each member raw so key presence, and an explicit
// null, are known. An absent member is a nil RawMessage.
type schedulePatchBody struct {
	Kind        json.RawMessage `json:"kind"`
	Dtstart     json.RawMessage `json:"dtstart"`
	Dtend       json.RawMessage `json:"dtend"`
	Tz          json.RawMessage `json:"tz"`
	Floating    json.RawMessage `json:"floating"`
	Rrule       json.RawMessage `json:"rrule"`
	RDates      json.RawMessage `json:"rdates"`
	ExDates     json.RawMessage `json:"exdates"`
	Anchor      json.RawMessage `json:"anchor"`
	Audience    json.RawMessage `json:"audience"`
	LeadMinutes json.RawMessage `json:"lead_minutes"`
}

func (b schedulePatchBody) present() map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"kind": b.Kind, "dtstart": b.Dtstart, "dtend": b.Dtend, "tz": b.Tz,
		"floating": b.Floating, "rrule": b.Rrule, "rdates": b.RDates, "exdates": b.ExDates,
		"anchor": b.Anchor, "audience": b.Audience, "lead_minutes": b.LeadMinutes,
	}
}

func isJSONNull(raw json.RawMessage) bool { return string(raw) == "null" }

// toScheduleUpdate maps a patch body onto the store's update. null clears
// dtend, rrule, rdates and exdates; on any other member it is `required`. It
// writes the response itself and returns ok=false on a refusal. fields is the
// allowlisted member names the body carried.
func toScheduleUpdate(w http.ResponseWriter, b schedulePatchBody) (upd models.ScheduleUpdate, fields []string, ok bool) {
	raws := b.present()
	for _, name := range scheduleMembers {
		raw := raws[name]
		if raw == nil {
			continue
		}
		fields = append(fields, name)
		null := isJSONNull(raw)
		clearable := name == "dtend" || name == "rrule" || name == "rdates" || name == "exdates"
		if null && !clearable {
			writeScheduleRequired(w, name)
			return upd, nil, false
		}
		var err error
		switch name {
		case "kind", "dtstart", "tz", "anchor", "audience", "dtend", "rrule":
			var v string
			if !null {
				err = json.Unmarshal(raw, &v)
			}
			p := &v
			switch name {
			case "kind":
				upd.Kind = p
			case "dtstart":
				upd.Dtstart = p
			case "tz":
				upd.Tz = p
			case "anchor":
				upd.Anchor = p
			case "audience":
				upd.Audience = p
			case "dtend":
				upd.Dtend = p
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

// handleUpdateItemSchedule changes an item's schedule.
// PATCH /api/v1/workspaces/{slug}/items/{itemSlug}/schedule
func (s *Server) handleUpdateItemSchedule(w http.ResponseWriter, r *http.Request) {
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
	body, err := strictScheduleBody[schedulePatchBody](r)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	upd, fields, ok := toScheduleUpdate(w, body)
	if !ok {
		return
	}
	if len(fields) == 0 {
		// {} changes nothing: answer with the schedule, without a write that
		// would only move updated_at.
		cur, err := s.store.GetSchedule(workspaceID, item.ID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if cur == nil {
			writeScheduleNotFound(w)
			return
		}
		writeJSON(w, http.StatusOK, s.buildScheduleView(r, item, cur))
		return
	}
	note := ""
	if upd.Floating != nil && !*upd.Floating && upd.Tz == nil {
		// Leaving floating without naming a zone takes the caller's, like create.
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
	sc, err := s.store.UpdateSchedule(workspaceID, item.ID, upd)
	if writeScheduleError(w, err) {
		return
	}
	if sc == nil {
		writeScheduleNotFound(w)
		return
	}
	s.recordScheduleWrite(r, workspaceID, item, "schedule_updated", scheduleActivityMeta(sc.Kind, fields))
	v := s.buildScheduleView(r, item, sc)
	v.TimezoneNote = note
	writeJSON(w, http.StatusOK, v)
}

// handleDeleteItemSchedule removes an item's schedule and its managed reminder.
// DELETE /api/v1/workspaces/{slug}/items/{itemSlug}/schedule
func (s *Server) handleDeleteItemSchedule(w http.ResponseWriter, r *http.Request) {
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
	removed, err := s.store.DeleteSchedule(workspaceID, item.ID)
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

// scheduleResolveBody is POST .../schedule/resolve. Every member is a pointer so
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
		writeResolveField(w, field, "range", field+" must be an RFC3339 instant")
		return time.Time{}, false
	}
	return t, true
}

// handleResolveItemSchedule resolves the outstanding occurrence of an item's
// schedule: done and skip advance it, snooze and move keep it outstanding.
// POST /api/v1/workspaces/{slug}/items/{itemSlug}/schedule/resolve
func (s *Server) handleResolveItemSchedule(w http.ResponseWriter, r *http.Request) {
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
	body, err := strictScheduleBody[scheduleResolveBody](r)
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
	if writeResolveError(w, r, err) {
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
	writeJSON(w, http.StatusOK, map[string]any{
		"schedule":   s.buildScheduleView(r, item, res.Schedule),
		"resolution": resolution,
	})
}

// resolveActivityMeta is the names-only metadata of a resolution: the schedule's
// kind and the state chosen. No comment text, wall, instant or rule.
func resolveActivityMeta(kind, state string) string {
	b, _ := json.Marshal(map[string]string{"kind": kind, "state": state})
	return string(b)
}
