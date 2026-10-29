package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/usertz"
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
	EndedAt        *string                 `json:"ended_at,omitempty"`
	ErrorCode      *string                 `json:"error_code,omitempty"`
	ReminderID     *string                 `json:"reminder_id,omitempty"`
	CreatedAt      string                  `json:"created_at"`
	UpdatedAt      string                  `json:"updated_at"`
	TimezoneNote   string                  `json:"timezone_note,omitempty"`
}

// buildScheduleView renders a stored schedule. A pinned schedule's next_at is
// the occurrence itself. A floating one stores only the earliest instant any
// zone could give next_wall, which is never shown: next_occurrence.at is the
// wall read in the CALLER's zone (the tick uses the recipient's).
func buildScheduleView(r *http.Request, item *models.Item, sc *models.ItemSchedule) scheduleView {
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
		switch {
		case !sc.Floating && sc.NextAt != nil:
			v.NextOccurrence = &scheduleOccurrenceView{At: *sc.NextAt, Wall: *sc.NextWall}
		case sc.Floating:
			if w, err := schedule.ParseWall(*sc.NextWall); err == nil {
				at := schedule.Resolve(viewerZone(r).Loc, w).UTC().Format(time.RFC3339)
				v.NextOccurrence = &scheduleOccurrenceView{At: at, Wall: *sc.NextWall}
			}
		}
	}
	return v
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
	writeJSON(w, http.StatusOK, buildScheduleView(r, item, sc))
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
	v := buildScheduleView(r, item, sc)
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
		writeJSON(w, http.StatusOK, buildScheduleView(r, item, cur))
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
	v := buildScheduleView(r, item, sc)
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
	s.recordScheduleWrite(r, workspaceID, item, "schedule_deleted", scheduleActivityMeta(cur.Kind, nil))
	writeJSON(w, http.StatusOK, map[string]any{"id": cur.ID, "deleted": true})
}
