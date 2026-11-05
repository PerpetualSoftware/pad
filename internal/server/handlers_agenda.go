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
	// agendaBudget bounds the expansion of a guest-callable read: a thousand
	// schedules at the engine's own per-call time cap are otherwise unbounded.
	agendaBudget = 1500 * time.Millisecond
)

type agendaEntry struct {
	Type        string `json:"type"` // schedule | date
	At          string `json:"at"`
	Day         string `json:"day"`
	Wall        string `json:"wall,omitempty"`
	AllDay      bool   `json:"all_day,omitempty"`
	Floating    *bool  `json:"floating,omitempty"`
	State       string `json:"state,omitempty"`
	ItemRef     string `json:"item_ref"`
	ItemTitle   string `json:"item_title"`
	Collection  string `json:"collection"`
	Kind        string `json:"kind,omitempty"`
	Audience    string `json:"audience,omitempty"`
	ScheduleID  string `json:"schedule_id,omitempty"`
	Field       string `json:"field,omitempty"`
	Date        string `json:"date,omitempty"`
	Fired       bool   `json:"fired,omitempty"`
	Overdue     bool   `json:"overdue,omitempty"`
	EffectiveAt string `json:"effective_at,omitempty"`
	ResolvedAt  string `json:"resolved_at,omitempty"`
	Actor       string `json:"actor,omitempty"`
	CommentID   string `json:"comment_id,omitempty"`
	MissedCount int    `json:"missed_count,omitempty"`
	Stale       bool   `json:"stale,omitempty"`

	instant time.Time // sort key, with Type and ItemRef
	subKey  string
}

type agendaUnavailable struct {
	ItemRef string `json:"item_ref"`
	Code    string `json:"code"`
}

// agendaUnexpanded names a schedule the window could not be expanded for (a
// scan, time or stall cap with nothing examined). It is never dropped silently.
type agendaUnexpanded struct {
	ScheduleID string `json:"schedule_id"`
	ItemRef    string `json:"item_ref"`
	ItemTitle  string `json:"item_title"`
	Collection string `json:"collection"`
	Kind       string `json:"kind,omitempty"`
	Audience   string `json:"audience,omitempty"`
	Reason     string `json:"reason"` // scan_limit
}

type agendaResponse struct {
	From         string              `json:"from"`
	To           string              `json:"to"`
	Tz           string              `json:"tz"`
	TimezoneNote string              `json:"timezone_note,omitempty"`
	Entries      []agendaEntry       `json:"entries"`
	Truncated    bool                `json:"truncated,omitempty"`
	NextFrom     string              `json:"next_from,omitempty"`
	Unexpanded   []agendaUnexpanded  `json:"unexpanded,omitempty"`
	Unavailable  []agendaUnavailable `json:"unavailable,omitempty"`
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
	zone, note := viewerZone(r), ""
	if tz := strings.TrimSpace(r.URL.Query().Get("tz")); tz != "" || r.URL.Query().Has("tz") {
		loc, err := usertz.Validate(tz)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_tz", "`tz` must be an IANA time zone name, such as America/Los_Angeles")
			return
		}
		zone = usertz.Zone{Name: tz, Loc: loc, Set: true}
	} else {
		note = usertz.Note(zone)
	}
	loc := zone.Loc

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

	now := time.Now()
	resp := agendaResponse{From: from.Format(time.RFC3339), To: to.Format(time.RFC3339), Tz: zone.Name, TimezoneNote: note, Entries: []agendaEntry{}}
	schedules, more, err := s.store.ListSchedulesForAgenda(workspaceID, scope, from, to, agendaMaxScan)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	resp.Truncated = more
	// timeCut is the earliest instant a time-ordered cut stopped at; setCut is
	// set by a cut that is not ordered by time (the schedule scan, the time
	// budget, the dated half), which no instant can describe: next_from is then
	// omitted and the client narrows the window or filters (TASK-4236).
	var timeCut time.Time
	setCut := more
	deadline := now.Add(agendaBudget)
	for _, as := range schedules {
		if time.Now().After(deadline) {
			resp.Truncated, setCut = true, true
			break
		}
		sc := as.Schedule
		x, err := s.store.ExpandAgendaCut(sc, loc, from, to, now)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		es, cut, unavailable := x.Entries, x.Truncated, x.Unavailable
		if unavailable != "" {
			resp.Unavailable = append(resp.Unavailable, agendaUnavailable{ItemRef: as.ItemRef, Code: unavailable})
		}
		if x.Unpromised {
			setCut = true
		}
		if x.Unexpanded {
			resp.Unexpanded = append(resp.Unexpanded, agendaUnexpanded{
				ScheduleID: sc.ID, ItemRef: as.ItemRef, ItemTitle: as.ItemTitle, Collection: as.Collection,
				Kind: sc.Kind, Audience: sc.Audience, Reason: "scan_limit",
			})
		}
		if !x.NextFrom.IsZero() && (timeCut.IsZero() || x.NextFrom.Before(timeCut)) {
			timeCut = x.NextFrom
		}
		resp.Truncated = resp.Truncated || cut
		for _, e := range es {
			out := agendaEntry{
				Type: "schedule", At: e.At.In(loc).Format(time.RFC3339), Day: e.At.In(loc).Format("2006-01-02"),
				Wall: e.Wall.String(), AllDay: e.Wall.AllDay, Floating: &sc.Floating, State: e.State,
				ItemRef: as.ItemRef, ItemTitle: as.ItemTitle, Collection: as.Collection, Kind: sc.Kind, Audience: sc.Audience,
				ScheduleID: sc.ID, Fired: e.Fired, Overdue: e.Overdue, ResolvedAt: e.ResolvedAt, Actor: e.Actor,
				CommentID: e.CommentID, MissedCount: e.MissedCount, Stale: e.Stale, instant: e.At, subKey: sc.ID,
			}
			if e.Wall.AllDay {
				out.Day = e.Wall.String() // a date, not shifted by the zone it is read in
			}
			if e.EffectiveAt != nil {
				out.EffectiveAt = e.EffectiveAt.In(loc).Format(time.RFC3339)
			}
			resp.Entries = append(resp.Entries, out)
		}
	}

	// The dated half: ListItems carries the scope, archived and terminal clauses.
	fromDay, toDay := from.In(loc).Format("2006-01-02"), to.Add(-time.Nanosecond).In(loc).Format("2006-01-02")
	items, err := s.store.ListItems(workspaceID, models.ItemListParams{
		CollectionIDs: scope.CollectionIDs, ItemIDs: scope.ItemIDs, NonTerminal: true, NoContent: true,
		FieldDateWindow: &models.FieldDateWindow{Keys: overdueDateFields, From: fromDay, To: toDay},
		Limit:           agendaMaxEntries + 1,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	resp.Truncated = resp.Truncated || len(items) > agendaMaxEntries
	setCut = setCut || len(items) > agendaMaxEntries
	today := overdueToday(now.In(loc))
	for _, it := range items {
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

	sort.SliceStable(resp.Entries, func(i, j int) bool {
		a, b := resp.Entries[i], resp.Entries[j]
		switch {
		case !a.instant.Equal(b.instant):
			return a.instant.Before(b.instant)
		case a.Type != b.Type:
			return a.Type == "schedule"
		case a.ItemRef != b.ItemRef:
			return a.ItemRef < b.ItemRef
		}
		return a.subKey < b.subKey
	})
	if len(resp.Entries) > agendaMaxEntries {
		// Entries are in time order, so the first one not shown is where a client
		// resumes, when it is a schedule entry. A dated entry is selected by its
		// calendar day, not by instant, so a cut on one promises nothing.
		if cut := resp.Entries[agendaMaxEntries]; cut.Type == "date" {
			setCut = true
		} else if timeCut.IsZero() || cut.instant.Before(timeCut) {
			timeCut = cut.instant
		}
		resp.Entries, resp.Truncated = resp.Entries[:agendaMaxEntries], true
	}
	// next_from promises every instant before it was examined, so it is never
	// emitted when a cut that no instant describes also fired. A schedule that
	// could not be expanded at all resumes at the window's end.
	switch {
	case setCut:
	case !timeCut.IsZero():
		// A resume point must move the client forward and stay inside the window
		// it asked for; one that does not promises nothing.
		if timeCut.After(to) {
			timeCut = to
		}
		if timeCut.After(from) {
			resp.NextFrom = timeCut.UTC().Format(time.RFC3339)
		}
	case len(resp.Unexpanded) > 0:
		resp.NextFrom = to.UTC().Format(time.RFC3339)
	}
	if len(resp.Unexpanded) > 0 {
		resp.Truncated = true
	}
	writeJSON(w, http.StatusOK, resp)
}
