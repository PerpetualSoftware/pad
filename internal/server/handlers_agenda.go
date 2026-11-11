package server

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/usertz"
)

// The workspace agenda (TASK-4104 D1, DOC-4098 Views): the schedules'
// occurrences and the items dated inside a window, in the caller's zone.
// HTTP only; the CLI and MCP render this shape in a later unit.

const (
	agendaMaxWindow  = 92 * 24 * time.Hour // schedule.CapWindow's own bound
	agendaMaxEntries = 500
	agendaMaxScan    = 1000 // schedules read per request
	// agendaMaxCountRows bounds the item creations and status changes read for
	// the counts series in one request.
	agendaMaxCountRows = 20000
	// agendaBudget bounds the expansion of a guest-callable read: a thousand
	// schedules at the engine's own per-call time cap are otherwise unbounded.
	agendaBudget = 1500 * time.Millisecond
)

type agendaEntry struct {
	Type          string                  `json:"type"` // schedule | date | created | completed
	At            string                  `json:"at,omitempty"`
	Day           string                  `json:"day,omitempty"`
	Wall          string                  `json:"wall,omitempty"`
	AllDay        bool                    `json:"all_day,omitempty"`
	Floating      *bool                   `json:"floating,omitempty"`
	State         string                  `json:"state,omitempty"`
	ItemRef       string                  `json:"item_ref"`
	ItemTitle     string                  `json:"item_title"`
	Collection    string                  `json:"collection"`
	Roles         *models.CommitmentRoles `json:"roles,omitempty"`
	OccurrenceKey string                  `json:"occurrence_key,omitempty"`
	Rank          string                  `json:"rank,omitempty"`
	Reason        string                  `json:"reason,omitempty"`
	RolesOrigin   string                  `json:"roles_origin,omitempty"`
	Audience      string                  `json:"audience,omitempty"`
	ScheduleID    string                  `json:"schedule_id,omitempty"`
	Field         string                  `json:"field,omitempty"`
	Date          string                  `json:"date,omitempty"`
	Fired         bool                    `json:"fired,omitempty"`
	Overdue       bool                    `json:"overdue,omitempty"`
	ResolvedAt    string                  `json:"resolved_at,omitempty"`
	Actor         string                  `json:"actor,omitempty"`
	CommentID     string                  `json:"comment_id,omitempty"`
	MissedCount   int                     `json:"missed_count,omitempty"`
	Stale         bool                    `json:"stale,omitempty"`

	Workspace     string `json:"workspace,omitempty"`
	WorkspaceName string `json:"workspace_name,omitempty"`
	// OwnerUsername tells apart two workspaces that share a slug under
	// different owners.
	OwnerUsername string `json:"owner_username,omitempty"`

	instant time.Time // sort key, with Type, Workspace, ItemRef and subKey
	subKey  string
}

type agendaUnavailable struct {
	ItemRef   string `json:"item_ref"`
	Code      string `json:"code"`
	Workspace string `json:"workspace,omitempty"`
	// OwnerUsername: see agendaEntry.
	OwnerUsername string `json:"owner_username,omitempty"`
}

// agendaUnexpanded names a schedule the window could not be expanded for (a
// scan, time or stall cap with nothing examined). It is never dropped silently.
type agendaUnexpanded struct {
	ScheduleID string `json:"schedule_id"`
	ItemRef    string `json:"item_ref"`
	ItemTitle  string `json:"item_title"`
	Collection string `json:"collection"`
	Audience   string `json:"audience,omitempty"`
	Reason     string `json:"reason"` // scan_limit
	Workspace  string `json:"workspace,omitempty"`
	// OwnerUsername: see agendaEntry.
	OwnerUsername string `json:"owner_username,omitempty"`
}

type agendaResponse struct {
	WindowComplete       bool                `json:"window_complete"`
	PriorityList         []agendaEntry       `json:"priority_list"`
	PriorityListComplete bool                `json:"priority_list_complete"`
	WaitingOn            []agendaEntry       `json:"waiting_on"`
	WaitingOnComplete    bool                `json:"waiting_on_complete"`
	NoDate               []agendaEntry       `json:"no_date"`
	NoDateComplete       bool                `json:"no_date_complete"`
	NoDateScope          string              `json:"no_date_scope,omitempty"`
	From                 string              `json:"from"`
	To                   string              `json:"to"`
	Tz                   string              `json:"tz"`
	TimezoneNote         string              `json:"timezone_note,omitempty"`
	Entries              []agendaEntry       `json:"entries"`
	Truncated            bool                `json:"truncated,omitempty"`
	NextFrom             string              `json:"next_from,omitempty"`
	Unexpanded           []agendaUnexpanded  `json:"unexpanded,omitempty"`
	Unavailable          []agendaUnavailable `json:"unavailable,omitempty"`
	// Counts is the per-day series for /me/agenda; CountsTruncated says a cap
	// stopped it, in which case Counts is absent rather than partial.
	Counts          []agendaCount `json:"counts,omitempty"`
	CountsTruncated bool          `json:"counts_truncated,omitempty"`
}

type agendaCount struct {
	Day       string `json:"day"`
	Completed int    `json:"completed"`
	Open      int    `json:"open"`
	Burndown  int    `json:"burndown"`
}

func writeAgendaParam(w http.ResponseWriter, field, code, msg string) {
	writeError2(w, http.StatusBadRequest, "validation_error", msg, map[string]interface{}{"field": field, "code": code})
}

// parseAgendaWindow reads from and to, writing the 400 itself on a refusal.
func parseAgendaWindow(w http.ResponseWriter, r *http.Request) (from, to time.Time, ok bool) {
	var t [2]time.Time
	for i, name := range []string{"from", "to"} {
		raw := strings.TrimSpace(r.URL.Query().Get(name))
		if raw == "" {
			writeAgendaParam(w, name, "required", "`"+name+"` is required: an RFC 3339 instant")
			return
		}
		var err error
		if t[i], err = time.Parse(time.RFC3339, raw); err != nil {
			writeAgendaParam(w, name, "format", "`"+name+"` must be an RFC 3339 instant, such as 2026-11-02T00:00:00Z")
			return
		}
	}
	from, to = t[0], t[1]
	switch {
	case !to.After(from):
		writeAgendaParam(w, "to", "range", "`to` must be after `from`")
	case to.Sub(from) > agendaMaxWindow:
		writeError2(w, http.StatusBadRequest, "validation_error", "The window is longer than 92 days",
			map[string]interface{}{"code": "cap_window", "limit_days": 92})
	default:
		return from, to, true
	}
	return
}

// agendaZone reads the optional `tz` parameter, writing the 400 itself on a bad
// name. note is set when the zone is the caller's own default.
func agendaZone(w http.ResponseWriter, r *http.Request) (zone usertz.Zone, note string, ok bool) {
	zone = viewerZone(r)
	if tz := strings.TrimSpace(r.URL.Query().Get("tz")); tz != "" || r.URL.Query().Has("tz") {
		loc, err := usertz.Validate(tz)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_tz", "`tz` must be an IANA time zone name, such as America/Los_Angeles")
			return zone, "", false
		}
		return usertz.Zone{Name: tz, Loc: loc, Set: true}, "", true
	}
	return zone, usertz.Note(zone), true
}

// handleGetAgenda serves GET /api/v1/workspaces/{ws}/agenda?from&to&tz.
func (s *Server) handleGetAgenda(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Has("stale") {
		s.handleStaleAgenda(w, r, workspaceID)
		return
	}
	from, to, ok := parseAgendaWindow(w, r)
	if !ok {
		return
	}
	zone, note, ok := agendaZone(w, r)
	if !ok {
		return
	}

	// The dashboard's visibility, applied in SQL before any bound.
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	fullColl, granted, err := s.guestResourceFilter(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	scope := store.PendingReminderScope{CollectionIDs: visibleIDs}
	if len(granted) > 0 {
		scope = store.PendingReminderScope{CollectionIDs: fullColl, ItemIDs: granted}
	}

	run := s.newAgendaRead(from, to, zone, note)
	run.recipient = strings.TrimSpace(r.URL.Query().Get("recipient"))
	run.listNoDate = run.recipient != ""
	if !run.listNoDate {
		run.response.NoDateScope = "personal_only"
	} else {
		run.audience = "human"
		if actor, _ := actorFromRequest(r); actor == "agent" {
			run.audience = "agent"
		}
	}
	if err := s.appendWorkspaceAgenda(run, workspaceID, scope); err != nil {
		writeInternalError(w, err)
		return
	}
	run.finish()
	writeJSON(w, http.StatusOK, run.response)
}

// agendaRead holds request-wide bounds and cuts shared by both agenda doors.
type agendaRead struct {
	response                agendaResponse
	from, to, now, deadline time.Time
	loc                     *time.Location
	remaining               int
	workRemaining           int
	recipient, audience     string
	listNoDate              bool
	scanCut                 bool
	// A set cut cannot promise a resume instant. A time cut can.
	setCut  bool
	timeCut time.Time
}

func (s *Server) newAgendaRead(from, to time.Time, zone usertz.Zone, note string) *agendaRead {
	now := s.agendaNow()
	limit := agendaMaxScan
	if s.agendaScanLimit > 0 {
		limit = min(limit, s.agendaScanLimit)
	}
	return &agendaRead{
		response: agendaResponse{From: from.Format(time.RFC3339), To: to.Format(time.RFC3339), Tz: zone.Name, TimezoneNote: note, Entries: []agendaEntry{}, WindowComplete: true, PriorityListComplete: true, WaitingOnComplete: true, NoDateComplete: true, PriorityList: []agendaEntry{}, WaitingOn: []agendaEntry{}, NoDate: []agendaEntry{}},
		from:     from, to: to, now: now, deadline: now.Add(agendaBudget), loc: zone.Loc, remaining: limit, workRemaining: agendaMaxScan, listNoDate: true,
	}
}

// appendWorkspaceAgenda is the single schedule expansion and dated-item path.
func (s *Server) appendWorkspaceAgenda(run *agendaRead, workspaceID string, scope store.PendingReminderScope) error {
	resp := &run.response
	from, to, now, loc := run.from, run.to, run.now, run.loc

	var schedules []store.AgendaSchedule
	if !run.scanCut {
		var more bool
		var err error
		schedules, more, err = s.store.ListSchedulesForAgenda(workspaceID, scope, from, to, run.remaining)
		if err != nil {
			return err
		}
		run.remaining -= len(schedules)
		run.scanCut = more
		resp.Truncated = resp.Truncated || more
		run.setCut = run.setCut || more
	}

	sch, err := expandAgendaSchedules(s.store, schedules, loc, from, to, now, run.deadline, s.agendaNow)
	if err != nil {
		return err
	}
	resp.Entries = append(resp.Entries, sch.Entries...)
	resp.Unavailable = append(resp.Unavailable, sch.Unavailable...)
	resp.Unexpanded = append(resp.Unexpanded, sch.Unexpanded...)
	resp.Truncated = resp.Truncated || sch.Truncated
	run.setCut = run.setCut || sch.SetCut
	if !sch.TimeCut.IsZero() && (run.timeCut.IsZero() || sch.TimeCut.Before(run.timeCut)) {
		run.timeCut = sch.TimeCut
	}

	if err := s.appendAgendaWork(run, workspaceID, scope); err != nil {
		return err
	}
	// The dated half: ListItems carries the scope, archived and terminal clauses.
	fromDay, toDay := from.In(loc).Format("2006-01-02"), to.Add(-time.Nanosecond).In(loc).Format("2006-01-02")
	items, err := s.store.ListItems(workspaceID, models.ItemListParams{
		CollectionIDs: scope.CollectionIDs, ItemIDs: scope.ItemIDs, NonTerminal: true, NoContent: true,
		FieldDateWindow: &models.FieldDateWindow{Keys: overdueDateFields, From: fromDay, To: toDay},
		Limit:           agendaMaxEntries + 1,
	})
	if err != nil {
		return err
	}
	resp.Truncated = resp.Truncated || len(items) > agendaMaxEntries
	run.setCut = run.setCut || len(items) > agendaMaxEntries
	today := overdueToday(now.In(loc))
	itemIDs := make([]string, len(items))
	for i, it := range items {
		itemIDs[i] = it.ID
	}
	gated, err := s.store.AgendaGatedItemIDs(workspaceID, itemIDs)
	if err != nil {
		return err
	}
	for _, it := range items {
		if gated[it.ID] {
			gates, err := s.store.ScheduleGates(workspaceID, it.ID, models.CommitmentAccess{Readable: map[string]bool{it.ID: true}})
			if err != nil {
				return err
			}
			if gates.State != models.GateStateReleased {
				continue
			}
		}
		ref := it.Ref
		if ref == "" {
			ref = it.Slug
		}
		for _, key := range overdueDateFields {
			v := extractFieldValue(it.Fields, key)
			if len(v) < 10 || v[:10] < fromDay || v[:10] > toDay {
				continue
			}
			at, err := time.Parse(time.RFC3339, v)
			if err != nil {
				if at, err = time.ParseInLocation("2006-01-02", v[:10], loc); err != nil {
					continue
				}
			}
			resp.Entries = append(resp.Entries, agendaEntry{
				Type: "date", At: at.In(loc).Format(time.RFC3339), Day: v[:10], Field: key, Date: v, Overdue: v < today,
				ItemRef: ref, ItemTitle: it.Title, Collection: it.CollectionSlug, instant: at, subKey: key,
			})
		}
	}

	return nil
}

func agendaTypeOrder(kind string) int {
	switch kind {
	case "schedule":
		return 0
	case "date":
		return 1
	case "created":
		return 2
	default:
		return 3
	}
}

func (run *agendaRead) finish() {
	resp := &run.response
	from, to := run.from, run.to

	sort.SliceStable(resp.Entries, func(i, j int) bool {
		a, b := resp.Entries[i], resp.Entries[j]
		switch {
		case !a.instant.Equal(b.instant):
			return a.instant.Before(b.instant)
		case a.Type != b.Type:
			return agendaTypeOrder(a.Type) < agendaTypeOrder(b.Type)
		case a.Workspace != b.Workspace:
			return a.Workspace < b.Workspace
		case a.ItemRef != b.ItemRef:
			return a.ItemRef < b.ItemRef
		}
		return a.subKey < b.subKey
	})
	if len(resp.Entries) > agendaMaxEntries {
		// Entries are in time order, so the first one not shown is where a client
		// resumes, when it is a schedule or item event. A dated entry is selected
		// by calendar day, not instant, so a cut on one promises nothing.
		if cut := resp.Entries[agendaMaxEntries]; cut.Type == "date" {
			run.setCut = true
		} else if run.timeCut.IsZero() || cut.instant.Before(run.timeCut) {
			run.timeCut = cut.instant
		}
		resp.Entries, resp.Truncated = resp.Entries[:agendaMaxEntries], true
	}
	// next_from promises every instant before it was examined, so it is never
	// emitted when a cut that no instant describes also fired. A schedule that
	// could not be expanded at all resumes at the window's end.
	switch {
	case run.setCut:
	case !run.timeCut.IsZero():
		// A resume point must move the client forward and stay inside the window
		// it asked for; one that does not promises nothing.
		if run.timeCut.After(to) {
			run.timeCut = to
		}
		if run.timeCut.After(from) {
			resp.NextFrom = run.timeCut.UTC().Format(time.RFC3339)
		}
	case len(resp.Unexpanded) > 0:
		resp.NextFrom = to.UTC().Format(time.RFC3339)
	}
	if len(resp.Unexpanded) > 0 {
		resp.Truncated = true
	}
	resp.WindowComplete = !resp.Truncated && len(resp.Unavailable) == 0
	finishAgendaWork(resp)
}

// agendaSchedules is the schedule half of an agenda-shaped answer.
// TimeCut is the earliest instant a time-ordered cut stopped at; SetCut is set
// by a cut that is not ordered by time (the schedule scan, the time budget),
// which no instant can describe.
type agendaSchedules struct {
	Entries     []agendaEntry
	Unexpanded  []agendaUnexpanded
	Unavailable []agendaUnavailable
	Truncated   bool
	SetCut      bool
	TimeCut     time.Time
}

// expandAgendaSchedules expands schedules over [from, to) in loc, stopping once
// clock() passes deadline, and maps each occurrence to an agendaEntry. The
// scan's own "there were more schedules" flag is the caller's to record.
// Shared by the agenda, /me/agenda and the calendar.
func expandAgendaSchedules(st *store.Store, schedules []store.AgendaSchedule, loc *time.Location, from, to, now, deadline time.Time, clock func() time.Time) (agendaSchedules, error) {
	var res agendaSchedules
	for _, as := range schedules {
		if clock().After(deadline) {
			res.Truncated, res.SetCut = true, true
			break
		}
		sc := as.Schedule
		x, err := st.ExpandAgendaCut(sc, loc, from, to, now)
		if err != nil {
			return res, err
		}
		es, cut, unavailable := x.Entries, x.Truncated, x.Unavailable
		if unavailable != "" {
			res.Unavailable = append(res.Unavailable, agendaUnavailable{ItemRef: as.ItemRef, Code: unavailable})
		}
		if x.Unpromised {
			res.SetCut = true
		}
		if x.Unexpanded {
			res.Unexpanded = append(res.Unexpanded, agendaUnexpanded{
				ScheduleID: sc.ID, ItemRef: as.ItemRef, ItemTitle: as.ItemTitle, Collection: as.Collection,
				Audience: sc.Audience, Reason: "scan_limit",
			})
		}
		if !x.NextFrom.IsZero() && (res.TimeCut.IsZero() || x.NextFrom.Before(res.TimeCut)) {
			res.TimeCut = x.NextFrom
		}
		res.Truncated = res.Truncated || cut
		for _, e := range es {
			out := agendaEntry{
				Type: "schedule", At: e.At.In(loc).Format(time.RFC3339), Day: e.At.In(loc).Format("2006-01-02"),
				Wall: e.Wall.String(), AllDay: e.Roles.Target != nil && len(*e.Roles.Target) == 10, Floating: &sc.Floating, State: e.State,
				ItemRef: as.ItemRef, ItemTitle: as.ItemTitle, Collection: as.Collection, Audience: sc.Audience,
				Roles: &e.Roles, OccurrenceKey: e.OccurrenceKey, RolesOrigin: as.RolesOrigin, ScheduleID: sc.ID, Fired: e.Fired, Overdue: e.Overdue, ResolvedAt: e.ResolvedAt, Actor: e.Actor,
				CommentID: e.CommentID, MissedCount: e.MissedCount, Stale: e.Stale, instant: e.At, subKey: sc.ID,
			}
			if out.AllDay {
				out.Day = *e.Roles.Target // a date, not shifted by the zone it is read in
			}

			res.Entries = append(res.Entries, out)
		}
	}
	return res, nil
}
