package server

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// The calendar read (TASK-4239, DOC-4098 Views): the items of ONE collection
// placed on days by a date field, with the schedules of those items, in the
// caller's zone. Anyone who may read the collection's item list may read it.
// The window, zone, schedule expansion and every bound are the agenda's.

// calendarPageSize is how many rows one store read of the dated items takes.
const calendarPageSize = 500

// calendarQueryParams are the route's own parameters. Every other parameter is
// an item-list filter and goes through the item list's parsing.
var calendarQueryParams = []string{"date_field", "end_field", "from", "to", "tz"}

type calendarItem struct {
	Item models.Item `json:"item"`
	// Day is the viewer's day the item is placed on; EndDay, when set, is the
	// last day of a span that starts on Day (end_field after date_field).
	Day    string `json:"day"`
	EndDay string `json:"end_day,omitempty"`
}

type calendarResponse struct {
	From         string              `json:"from"`
	To           string              `json:"to"`
	Tz           string              `json:"tz"`
	TimezoneNote string              `json:"timezone_note,omitempty"`
	Items        []calendarItem      `json:"items"`
	Entries      []agendaEntry       `json:"entries"`
	Truncated    bool                `json:"truncated,omitempty"`
	NextFrom     string              `json:"next_from,omitempty"`
	PartialDay   string              `json:"partial_day,omitempty"`
	Unexpanded   []agendaUnexpanded  `json:"unexpanded,omitempty"`
	Unavailable  []agendaUnavailable `json:"unavailable,omitempty"`
}

// calendarCell is one shown thing on one day, for the single cap.
type calendarCell struct {
	day   string
	item  *calendarItem
	sort  string // items: the value's own text, then id
	entry *agendaEntry
}

// calendarSortableKey is the store's sort-key rule (validSortField).
var calendarSortableKey = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

const dayLayout = "2006-01-02"

// handleGetCalendar serves
// GET /api/v1/workspaces/{ws}/collections/{collSlug}/calendar.
func (s *Server) handleGetCalendar(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	// The gates, in the item list's order and with its bytes: a collection the
	// caller cannot see answers like one that does not exist, before any
	// parameter is read.
	coll, err := s.resolveItemCollectionSlug(workspaceID, chi.URLParam(r, "collSlug"))
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if coll == nil {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if !isCollectionVisible(coll.ID, visibleIDs) {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
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
	loc := zone.Loc

	var collSchema models.CollectionSchema
	if coll.Schema != "" {
		_ = models.UnmarshalItemFieldSchema([]byte(coll.Schema), &collSchema)
	}
	dateField := strings.TrimSpace(r.URL.Query().Get("date_field"))
	endField := strings.TrimSpace(r.URL.Query().Get("end_field"))
	if cerr := models.ValidateCalendarFields(dateField, endField, collSchema); cerr != nil {
		writeCalendarConfigError(w, cerr)
		return
	}

	// The item list's parameters, through the item list's parsing. The route's
	// own parameters are removed first: the parser reads every name it does not
	// know as a field filter.
	q := url.Values{}
	for k, v := range r.URL.Query() {
		q[k] = v
	}
	for _, k := range calendarQueryParams {
		q.Del(k)
	}
	pr := r.Clone(r.Context())
	pu := *r.URL
	pu.RawQuery = q.Encode()
	pr.URL = &pu
	params := parseItemListParams(pr)
	// Request values never reach the query for these: the calendar pages its
	// own reads, orders its own rows, and shows terminal items but not
	// archived ones.
	params.Limit, params.Offset = 0, 0
	params.Sort, params.GroupBy = "", ""
	params.IncludeArchived, params.NonTerminal = false, false
	params.NoContent = true
	// The path collection is an AND on its own (ScopeCollectionID), never the
	// CollectionIDs/ItemIDs permission pair, which is a union.
	params.ScopeCollectionID = coll.ID
	if err := validateUnparentedListRequest(pr, params); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if params.Unparented && visibleIDs != nil {
		writeError(w, http.StatusForbidden, "forbidden", "unparented filtering requires unrestricted workspace access")
		return
	}
	fullColl, granted, err := s.guestResourceFilter(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	restricted := false // the collection is visible through item grants only
	if len(granted) > 0 {
		inFull := false
		for _, id := range fullColl {
			if id == coll.ID {
				inFull = true
				break
			}
		}
		if !inFull && workspaceRole(r) != "guest" {
			memberColls, _ := s.store.GetMemberCollectionAccess(workspaceID, currentUserID(r))
			for _, id := range memberColls {
				if id == coll.ID {
					inFull = true
					break
				}
			}
		}
		if !inFull {
			restricted = true
			params.ItemIDs = granted
		}
	}
	if err := s.resolveParentFilter(pr, workspaceID, &params, collSchema); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	undeclared, err := s.checkListFilterKeys(workspaceID, params, []models.CollectionSchema{collSchema}, fmt.Sprintf("collection %q", coll.Slug))
	if err != nil {
		writeListFilterKeyError(w, err)
		return
	}
	if len(undeclared) > 0 {
		w.Header().Set(undeclaredFilterKeysHeader, strings.Join(undeclared, ","))
	}

	fromDay, toDay := from.In(loc).Format(dayLayout), to.Add(-time.Nanosecond).In(loc).Format(dayLayout)
	resp := calendarResponse{
		From: from.Format(time.RFC3339), To: to.Format(time.RFC3339), Tz: zone.Name, TimezoneNote: note,
		Items: []calendarItem{}, Entries: []agendaEntry{},
	}

	placed, err := s.calendarItems(workspaceID, coll.ID, params, dateField, endField, fromDay, toDay, loc)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// The schedules: the agenda's read, narrowed to this collection in SQL
	// (before its scan bound), then to the page's filters.
	scope := store.PendingReminderScope{CollectionIDs: []string{coll.ID}}
	if restricted {
		ids, err := s.store.ItemIDsInCollectionsQ(s.store.Q(), granted, []string{coll.ID})
		if err != nil {
			writeInternalError(w, err)
			return
		}
		scope = store.PendingReminderScope{CollectionIDs: []string{}, ItemIDs: ids}
	}
	now := time.Now()
	schedules, more, err := s.store.ListSchedulesForAgenda(workspaceID, scope, from, to, agendaMaxScan)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if calendarFilterActive(params) && len(schedules) > 0 {
		schedules, err = s.calendarFollowItems(workspaceID, params, schedules)
		if err != nil {
			writeInternalError(w, err)
			return
		}
	}
	sch, err := expandAgendaSchedules(s.store, schedules, more, loc, from, to, now)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	resp.Unexpanded, resp.Unavailable = sch.Unexpanded, sch.Unavailable
	resp.Truncated = sch.Truncated || placed.more

	// One cap across both, in day order, cut only between days.
	sort.SliceStable(sch.Entries, func(i, j int) bool {
		a, b := sch.Entries[i], sch.Entries[j]
		switch {
		case !a.instant.Equal(b.instant):
			return a.instant.Before(b.instant)
		case a.ItemRef != b.ItemRef:
			return a.ItemRef < b.ItemRef
		}
		return a.subKey < b.subKey
	})
	cells := make([]calendarCell, 0, len(placed.items)+len(sch.Entries))
	for i := range placed.items {
		it := &placed.items[i]
		cells = append(cells, calendarCell{day: it.sortDay, item: &it.calendarItem, sort: it.sort})
	}
	for i := range sch.Entries {
		cells = append(cells, calendarCell{day: sch.Entries[i].Day, entry: &sch.Entries[i]})
	}
	// Day, then items before entries, then each kind in its own stable order:
	// an item by its value's text then id; an entry by instant, item, schedule.
	sort.SliceStable(cells, func(i, j int) bool {
		a, b := cells[i], cells[j]
		if a.day != b.day {
			return a.day < b.day
		}
		if (a.item != nil) != (b.item != nil) {
			return a.item != nil
		}
		if a.item != nil {
			return a.sort < b.sort
		}
		return false
	})
	keep, nextFrom, partial := cutCalendarDays(cells, agendaMaxEntries, toDay)
	if len(keep) < len(cells) || placed.more {
		resp.Truncated = true
	}
	resp.PartialDay = partial
	if !sch.SetCut {
		if !sch.TimeCut.IsZero() {
			if d := sch.TimeCut.In(loc).Format(dayLayout); d > fromDay && d <= toDay && (nextFrom == "" || d < nextFrom) {
				nextFrom = d
			}
		}
		resp.NextFrom = nextFrom
	}
	// Nothing at or after next_from is shown, so paging from it repeats nothing.
	var shown []models.Item
	for _, c := range keep {
		if resp.NextFrom != "" && c.day >= resp.NextFrom {
			continue
		}
		if c.item != nil {
			shown = append(shown, c.item.Item)
		}
	}
	s.enrichItemsWithParent(r, workspaceID, shown, visibleIDs)
	n := 0
	for _, c := range keep {
		if resp.NextFrom != "" && c.day >= resp.NextFrom {
			continue
		}
		if c.item != nil {
			c.item.Item = shown[n]
			n++
			resp.Items = append(resp.Items, *c.item)
		} else {
			resp.Entries = append(resp.Entries, *c.entry)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// cutCalendarDays applies the cap to cells, which are in day order. It keeps
// whole days; the first day not kept is next_from (empty when it falls outside
// the window). If the FIRST day alone is over the cap it keeps what fits of it,
// reports it as partial, and resumes at the day after.
func cutCalendarDays(cells []calendarCell, limit int, toDay string) (keep []calendarCell, nextFrom, partial string) {
	if len(cells) <= limit {
		return cells, "", ""
	}
	shown := 0
	for i := 0; i < len(cells); {
		j := i
		for j < len(cells) && cells[j].day == cells[i].day {
			j++
		}
		if shown+(j-i) > limit {
			if shown == 0 {
				partial = cells[i].day
				nextFrom = nextDay(partial)
				keep = cells[:limit]
			} else {
				nextFrom = cells[i].day
				keep = cells[:i]
			}
			break
		}
		shown += j - i
		i = j
	}
	if nextFrom > toDay {
		nextFrom = ""
	}
	return keep, nextFrom, partial
}

func nextDay(day string) string {
	t, err := time.Parse(dayLayout, day)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, 1).Format(dayLayout)
}

func prevDay(day string) string {
	t, err := time.Parse(dayLayout, day)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, -1).Format(dayLayout)
}

// calendarFilterActive reports whether the request carries item-list filters.
func calendarFilterActive(p models.ItemListParams) bool {
	return len(p.Fields) > 0 || len(p.FieldsAnyOf) > 0 || p.Tag != "" || p.ParentID != "" ||
		p.ParentLinkID != "" || p.AssignedUserID != "" || p.AgentRoleID != "" || p.Unparented ||
		strings.TrimSpace(p.Search) != ""
}

// calendarFollowItems keeps the schedules whose item passes the page's filters.
// The item read carries the SAME filters, no date window (a schedule's item need
// not have its date in the window), and ItemIDs set to the schedules' items with
// CollectionIDs nil: the pair is a union, so passing both would widen the read.
// The ids came out of a read already scoped to the caller, so this narrows. It
// is never called with no schedules: an empty ItemIDs and nil CollectionIDs
// carries no scope clause at all.
func (s *Server) calendarFollowItems(workspaceID string, params models.ItemListParams, schedules []store.AgendaSchedule) ([]store.AgendaSchedule, error) {
	seen := map[string]bool{}
	var ids []string
	for _, as := range schedules {
		if id := as.Schedule.ItemID; !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	fp := params
	fp.FieldRanges, fp.FieldSpans = nil, nil
	fp.CollectionIDs, fp.ItemIDs = nil, ids
	fp.Limit, fp.Offset, fp.Sort = 0, 0, ""
	items, err := s.store.ListItems(workspaceID, fp)
	if err != nil {
		return nil, err
	}
	kept := map[string]bool{}
	for _, it := range items {
		if seen[it.ID] {
			kept[it.ID] = true
		}
	}
	out := schedules[:0:0]
	for _, as := range schedules {
		if kept[as.Schedule.ItemID] {
			out = append(out, as)
		}
	}
	return out, nil
}

type calendarPlaced struct {
	calendarItem
	sortDay string // Day, or the window's first day for a span that began before it
	sort    string
}

type calendarItems struct {
	items []calendarPlaced
	more  bool // the read stopped with rows unread
}

// calendarItems reads the items the window selects and places each on its day.
// A date field is placed by its value (a date, or an instant in the value's own
// offset); created and completed are instants and are placed on the viewer's
// day, so the store window is widened by a day each side and the entries that
// fall outside it after placement are dropped. Rows come in date order (SQL for
// a date field and created; completed is read whole and ordered here), and the
// read stops once more than the cap is in hand.
func (s *Server) calendarItems(workspaceID, collID string, params models.ItemListParams, dateField, endField, fromDay, toDay string, loc *time.Location) (calendarItems, error) {
	var res calendarItems
	instantKey := models.IsCalendarBuiltinDateField(dateField)
	qFrom, qTo := fromDay, toDay
	if instantKey {
		qFrom, qTo = prevDay(fromDay), nextDay(toDay)
	}
	if endField == "" {
		params.FieldRanges = []models.FieldRange{{Key: dateField, From: qFrom, To: qTo}}
	} else {
		params.FieldSpans = []models.FieldSpan{{StartKey: dateField, EndKey: endField, From: qFrom, To: qTo}}
	}
	// Rows arrive in date order only where the store can sort by the key; a key
	// it cannot sort by (a hyphen) and completed are read whole and ordered here.
	ordered := dateField != "completed" && (instantKey || calendarSortableKey.MatchString(dateField))
	switch dateField {
	case "created", "completed":
		params.Sort = "created_at:asc"
	default:
		params.Sort = dateField + ":asc"
	}
	params.Limit = calendarPageSize
	for offset := 0; ; offset += calendarPageSize {
		params.Offset = offset
		page, err := s.store.ListItems(workspaceID, params)
		if err != nil {
			return res, err
		}
		var completed map[string]string
		if dateField == "completed" {
			ids := make([]string, len(page))
			for i, it := range page {
				ids[i] = it.ID
			}
			if completed, err = s.store.CompletedInstants(workspaceID, collID, ids); err != nil {
				return res, err
			}
		}
		for _, it := range page {
			day, text := "", ""
			switch dateField {
			case "created":
				day, text = it.CreatedAt.In(loc).Format(dayLayout), it.CreatedAt.UTC().Format(time.RFC3339)
			case "completed":
				if at, err := time.Parse(time.RFC3339, completed[it.ID]); err == nil {
					day, text = at.In(loc).Format(dayLayout), completed[it.ID]
				}
			default:
				text = extractFieldValue(it.Fields, dateField)
				if len(text) >= 10 {
					day = text[:10]
				}
			}
			if _, err := time.Parse(dayLayout, day); err != nil || len(day) != 10 {
				continue // a value that is no date is on no day
			}
			end := ""
			if endField != "" {
				if e := extractFieldValue(it.Fields, endField); len(e) >= 10 {
					if _, err := time.Parse(dayLayout, e[:10]); err == nil && e[:10] > day {
						end = e[:10]
					}
				}
			}
			last := day
			if end != "" {
				last = end
			}
			if day > toDay || last < fromDay {
				continue // inside the widened read, outside the window
			}
			sortDay := day
			if sortDay < fromDay {
				sortDay = fromDay
			}
			res.items = append(res.items, calendarPlaced{
				calendarItem: calendarItem{Item: it, Day: day, EndDay: end},
				sortDay:      sortDay, sort: text + "\x00" + it.ID,
			})
		}
		if len(page) < calendarPageSize {
			break
		}
		if ordered && len(res.items) > agendaMaxEntries {
			res.more = true
			break
		}
	}
	return res, nil
}
