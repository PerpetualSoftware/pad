package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-4104 D1: GET /workspaces/{ws}/agenda.

const agendaSchema = `{"fields":[` +
	`{"key":"status","type":"select","options":["open","done"],"default":"open","terminal_options":["done"]},` +
	`{"key":"due_date","type":"date"},{"key":"end_date","type":"date"}]}`

// agendaWindow is a one-day window in 2031, well after any clock a test runs at,
// so a schedule created with a 2031 start is always armed.
const agendaWindow = "from=2031-03-10T00:00:00Z&to=2031-03-11T00:00:00Z"

type agendaWorld struct {
	srv   *Server
	ws    *models.Workspace
	owner *models.User
	colls map[string]*models.Collection
	items map[string]*models.Item
}

func newAgendaWorld(t *testing.T, slugs ...string) *agendaWorld {
	t.Helper()
	srv := testServer(t)
	owner := mustUser(t, srv, "agenda-owner@example.com", "agendaowner", "")
	w := &agendaWorld{srv: srv, ws: mustWorkspace(t, srv, "Agenda", owner.ID), owner: owner,
		colls: map[string]*models.Collection{}, items: map[string]*models.Item{}}
	for i, slug := range slugs {
		c, err := srv.store.CreateCollection(w.ws.ID, models.CollectionCreate{Name: strings.ToUpper(slug[:1]) + slug[1:], Slug: slug, Prefix: fmt.Sprintf("P%d", i), Schema: agendaSchema})
		if err != nil {
			t.Fatalf("CreateCollection(%s): %v", slug, err)
		}
		w.colls[slug] = c
	}
	return w
}

// item creates an item and, when sc is non-nil, its schedule.
func (w *agendaWorld) item(t *testing.T, coll, title, fields string, sc *models.ScheduleCreate) *models.Item {
	t.Helper()
	it, err := w.srv.store.CreateItem(w.ws.ID, w.colls[coll].ID, models.ItemCreate{Title: title, Fields: fields})
	if err != nil {
		t.Fatalf("CreateItem(%s): %v", title, err)
	}
	w.items[title] = it
	if sc != nil {
		if _, err := w.srv.store.CreateSchedule(w.ws.ID, it.ID, *sc); err != nil {
			t.Fatalf("CreateSchedule(%s): %v", title, err)
		}
	}
	return it
}

// oneShot is a schedule at 09:00 UTC on the agendaWindow's day.
func oneShot() *models.ScheduleCreate {
	return &models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC"}
}

func (w *agendaWorld) get(t *testing.T, u *models.User, query string) (int, map[string]any) {
	t.Helper()
	tok, err := w.srv.store.CreateSession(u.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	rr := doRequestWithCookie(w.srv, "GET", "/api/v1/workspaces/"+w.ws.Slug+"/agenda?"+query, nil, tok)
	m := map[string]any{}
	_ = json.Unmarshal(rr.Body.Bytes(), &m)
	return rr.Code, m
}

func (w *agendaWorld) ok(t *testing.T, u *models.User, query string) map[string]any {
	t.Helper()
	code, m := w.get(t, u, query)
	if code != http.StatusOK {
		t.Fatalf("agenda %s: %d %v", query, code, m)
	}
	return m
}

func entriesOf(m map[string]any) []map[string]any {
	var out []map[string]any
	es, _ := m["entries"].([]any)
	for _, e := range es {
		out = append(out, e.(map[string]any))
	}
	return out
}

// seen is the sorted "type:title" set of a response.
func seen(m map[string]any) string {
	var out []string
	for _, e := range entriesOf(m) {
		out = append(out, fmt.Sprintf("%v:%v", e["type"], e["item_title"]))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestAgendaGuestVisibility(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks", "secret", "ideas")
	for _, c := range []string{"tasks", "secret", "ideas"} {
		w.item(t, c, "AG "+c+" sched", `{"status":"open"}`, oneShot())
		w.item(t, c, "AG "+c+" dated", `{"status":"open","due_date":"2031-03-10"}`, nil)
	}
	all := "date:AG ideas dated,date:AG secret dated,date:AG tasks dated,schedule:AG ideas sched,schedule:AG secret sched,schedule:AG tasks sched"
	if got := seen(w.ok(t, w.owner, agendaWindow)); got != all {
		t.Fatalf("owner sees %s, want %s", got, all)
	}

	member := mustUser(t, w.srv, "agenda-member@example.com", "agendamember", "")
	if err := w.srv.store.AddWorkspaceMember(w.ws.ID, member.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if err := w.srv.store.SetMemberCollectionAccess(w.ws.ID, member.ID, "specific", []string{w.colls["tasks"].ID}); err != nil {
		t.Fatal(err)
	}
	if got, want := seen(w.ok(t, member, agendaWindow)), "date:AG tasks dated,schedule:AG tasks sched"; got != want {
		t.Errorf("restricted member sees %s, want %s", got, want)
	}

	guest := mustUser(t, w.srv, "agenda-guest@example.com", "agendaguest", "")
	for _, title := range []string{"AG ideas sched", "AG secret dated"} {
		if _, err := w.srv.store.CreateItemGrant(w.ws.ID, w.items[title].ID, guest.ID, "view", w.owner.ID); err != nil {
			t.Fatal(err)
		}
	}
	m := w.ok(t, guest, agendaWindow)
	if got, want := seen(m), "date:AG secret dated,schedule:AG ideas sched"; got != want {
		t.Errorf("item-grant guest sees %s, want %s", got, want)
	}
	if _, has := m["unavailable"]; has {
		t.Errorf("unavailable must not name anything for a guest here: %v", m["unavailable"])
	}

	cguest := mustUser(t, w.srv, "agenda-cguest@example.com", "agendacguest", "")
	if _, err := w.srv.store.CreateCollectionGrant(w.ws.ID, w.colls["ideas"].ID, cguest.ID, "view", w.owner.ID); err != nil {
		t.Fatal(err)
	}
	if got, want := seen(w.ok(t, cguest, agendaWindow)), "date:AG ideas dated,schedule:AG ideas sched"; got != want {
		t.Errorf("collection-grant guest sees %s, want %s", got, want)
	}

	// A guest with no grant at all sees an empty agenda, not an error.
	none := mustUser(t, w.srv, "agenda-none@example.com", "agendanone", "")
	if err := w.srv.store.AddWorkspaceMember(w.ws.ID, none.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := w.srv.store.SetMemberCollectionAccess(w.ws.ID, none.ID, "specific", []string{}); err != nil {
		t.Fatal(err)
	}
	if got := seen(w.ok(t, none, agendaWindow)); got != "" {
		t.Errorf("a member with no collections sees %q, want nothing", got)
	}
}

func TestAgendaWindowAndParameterRefusals(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.ok(t, w.owner, "from=2031-03-01T00:00:00Z&to=2031-06-01T00:00:00Z") // exactly 92 days
	for name, c := range map[string]struct{ q, code, detail string }{
		"92d + 1s":     {"from=2031-03-01T00:00:00Z&to=2031-06-01T00:00:01Z", "validation_error", "cap_window"},
		"missing from": {"to=2031-03-11T00:00:00Z", "validation_error", "required"},
		"missing to":   {"from=2031-03-10T00:00:00Z", "validation_error", "required"},
		"bad from":     {"from=yesterday&to=2031-03-11T00:00:00Z", "validation_error", "format"},
		"bad to":       {"from=2031-03-10T00:00:00Z&to=2031-03-11", "validation_error", "format"},
		"to == from":   {"from=2031-03-10T00:00:00Z&to=2031-03-10T00:00:00Z", "validation_error", "range"},
		"to < from":    {"from=2031-03-11T00:00:00Z&to=2031-03-10T00:00:00Z", "validation_error", "range"},
		"tz Local":     {agendaWindow + "&tz=Local", "invalid_tz", ""},
		"tz unknown":   {agendaWindow + "&tz=Nope/Zone", "invalid_tz", ""},
		"tz empty":     {agendaWindow + "&tz=", "invalid_tz", ""},
		"tz blank":     {agendaWindow + "&tz=%20", "invalid_tz", ""},
	} {
		code, m := w.get(t, w.owner, c.q)
		e, _ := m["error"].(map[string]any)
		if code != http.StatusBadRequest || e == nil || e["code"] != c.code {
			t.Errorf("%s: %d %v, want 400 %s", name, code, m, c.code)
			continue
		}
		if d, _ := e["details"].(map[string]any); c.detail != "" && (d == nil || d["code"] != c.detail) {
			t.Errorf("%s: details = %v, want code %s", name, e["details"], c.detail)
		}
	}
}

func TestAgendaTimezoneDefaultsToTheCaller(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.item(t, "tasks", "AG tz", `{"status":"open"}`, oneShot())
	m := w.ok(t, w.owner, agendaWindow)
	if m["tz"] != "UTC" || m["timezone_note"] == nil || m["timezone_note"] == "" {
		t.Errorf("no account zone: tz %v note %v, want UTC with a note", m["tz"], m["timezone_note"])
	}
	la := "America/Los_Angeles"
	if _, err := w.srv.store.UpdateUser(w.owner.ID, models.UserUpdate{Timezone: &la}); err != nil {
		t.Fatal(err)
	}
	m = w.ok(t, w.owner, agendaWindow)
	if m["tz"] != la || m["timezone_note"] != nil {
		t.Errorf("account zone set: tz %v note %v, want %s and no note", m["tz"], m["timezone_note"], la)
	}
	if m = w.ok(t, w.owner, agendaWindow+"&tz=Asia/Tokyo"); m["tz"] != "Asia/Tokyo" || m["timezone_note"] != nil {
		t.Errorf("explicit tz: %v %v", m["tz"], m["timezone_note"])
	}
}

func TestAgendaRendersPinnedAndFloatingInTheCallersZone(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.item(t, "tasks", "AG pinned", `{"status":"open"}`, &models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "Asia/Tokyo"})
	w.item(t, "tasks", "AG floating", `{"status":"open"}`, &models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Floating: true})
	la, _ := time.LoadLocation("America/Los_Angeles")
	q := "from=2031-03-09T00:00:00Z&to=2031-03-12T00:00:00Z&tz=America/Los_Angeles"
	byTitle := map[string]map[string]any{}
	for _, e := range entriesOf(w.ok(t, w.owner, q)) {
		byTitle[e["item_title"].(string)] = e
	}
	pinnedInstant := time.Date(2031, 3, 10, 0, 0, 0, 0, time.UTC).In(la) // 09:00 Tokyo
	if e := byTitle["AG pinned"]; e == nil || e["at"] != pinnedInstant.Format(time.RFC3339) || e["day"] != pinnedInstant.Format("2006-01-02") || e["floating"] != false {
		t.Errorf("pinned entry = %v, want the Tokyo instant rendered in LA (%s)", e, pinnedInstant.Format(time.RFC3339))
	}
	wall := time.Date(2031, 3, 10, 9, 0, 0, 0, la)
	if e := byTitle["AG floating"]; e == nil || e["at"] != wall.Format(time.RFC3339) || e["day"] != "2031-03-10" || e["floating"] != true {
		t.Errorf("floating entry = %v, want 09:00 LA (%s)", e, wall.Format(time.RFC3339))
	}
}

func TestAgendaAllDay(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.item(t, "tasks", "AG allday", `{"status":"open"}`, &models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10", Tz: "Asia/Tokyo"})
	e := entriesOf(w.ok(t, w.owner, "from=2031-03-09T00:00:00Z&to=2031-03-12T00:00:00Z&tz=Asia/Tokyo"))
	if len(e) != 1 || e[0]["at"] != "2031-03-10T00:00:00+09:00" || e[0]["all_day"] != true || e[0]["day"] != "2031-03-10" || e[0]["wall"] != "2031-03-10" {
		t.Fatalf("all-day in Tokyo = %v, want local midnight, all_day, day 2031-03-10", e)
	}
	la, _ := time.LoadLocation("America/Los_Angeles")
	e = entriesOf(w.ok(t, w.owner, "from=2031-03-08T00:00:00Z&to=2031-03-12T00:00:00Z&tz=America/Los_Angeles"))
	want := time.Date(2031, 3, 9, 15, 0, 0, 0, time.UTC).In(la).Format(time.RFC3339) // Tokyo midnight, same instant
	if len(e) != 1 || e[0]["at"] != want || e[0]["day"] != "2031-03-10" {
		t.Fatalf("all-day viewed in LA = %v, want the same instant (%s) on the wall day 2031-03-10", e, want)
	}
}

func TestAgendaOneRunawayScheduleDoesNotTakeItDown(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.item(t, "tasks", "AG normal", `{"status":"open"}`, oneShot())
	w.item(t, "tasks", "AG runaway", `{"status":"open"}`, &models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-01T00:00", Tz: "UTC", Rrule: "FREQ=MINUTELY"})
	broken := w.item(t, "tasks", "AG broken", `{"status":"open"}`, oneShot())
	if _, err := w.srv.store.DB().Exec(w.srv.store.D().Rebind(`UPDATE item_schedules SET error_code = 'cap_scan' WHERE item_id = ?`), broken.ID); err != nil {
		t.Fatal(err)
	}
	code, m := w.get(t, w.owner, "from=2031-03-01T00:00:00Z&to=2031-05-30T00:00:00Z")
	if code != http.StatusOK {
		t.Fatalf("one runaway schedule must not fail the agenda: %d %v", code, m)
	}
	runawayRef := fmt.Sprintf("P0-%d", *w.items["AG runaway"].ItemNumber)
	var normal, runaway int
	for _, e := range entriesOf(m) {
		switch e["item_title"] {
		case "AG normal":
			normal++
		case "AG runaway":
			runaway++
		case "AG broken":
			t.Errorf("a broken schedule has no entries: %v", e)
		}
	}
	if normal != 1 {
		t.Errorf("the healthy schedule is still listed once, got %d", normal)
	}
	un, _ := m["unavailable"].([]any)
	var names []string
	runawayUnavailable := false
	for _, u := range un {
		names = append(names, fmt.Sprint(u))
		if um, ok := u.(map[string]any); ok && um["item_ref"] == runawayRef {
			runawayUnavailable = true
		}
	}
	if !strings.Contains(strings.Join(names, ";"), "cap_scan") {
		t.Errorf("unavailable = %v, want the broken schedule with cap_scan", un)
	}
	if !runawayUnavailable && !(m["truncated"] == true && runaway > 0 && runaway <= 100) {
		t.Errorf("the runaway (%s) is neither unavailable nor listed (%d entries) and truncated: %v", runawayRef, runaway, m)
	}
	if runaway > 100 {
		t.Errorf("the runaway schedule lists %d entries, over the per-schedule cap of 100", runaway)
	}
}

func TestAgendaHidesDoneItems(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.item(t, "tasks", "AG open sched", `{"status":"open"}`, oneShot())
	w.item(t, "tasks", "AG done sched", `{"status":"done"}`, oneShot())
	w.item(t, "tasks", "AG open dated", `{"status":"open","due_date":"2031-03-10"}`, nil)
	w.item(t, "tasks", "AG done dated", `{"status":"done","due_date":"2031-03-10"}`, nil)
	if got, want := seen(w.ok(t, w.owner, agendaWindow)), "date:AG open dated,schedule:AG open sched"; got != want {
		t.Errorf("agenda = %s, want %s", got, want)
	}
}

func TestAgendaDatedItems(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.item(t, "tasks", "AG day", `{"status":"open","due_date":"2031-03-10"}`, nil)
	w.item(t, "tasks", "AG stamp", `{"status":"open","end_date":"2031-03-10T23:30:00-08:00"}`, nil)
	w.item(t, "tasks", "AG outside", `{"status":"open","due_date":"2031-03-12"}`, nil)
	w.item(t, "tasks", "AG old", `{"status":"open","due_date":"2020-01-01"}`, nil)
	e := entriesOf(w.ok(t, w.owner, agendaWindow))
	if len(e) != 2 {
		t.Fatalf("entries = %v, want the two dated items inside the window", e)
	}
	for _, x := range e {
		if x["type"] != "date" || x["day"] != "2031-03-10" || x["overdue"] != nil {
			t.Errorf("dated entry = %v", x)
		}
		switch x["item_title"] {
		case "AG day":
			if x["field"] != "due_date" || x["date"] != "2031-03-10" {
				t.Errorf("day entry = %v", x)
			}
		case "AG stamp":
			if x["field"] != "end_date" || x["date"] != "2031-03-10T23:30:00-08:00" {
				t.Errorf("stamp entry = %v", x)
			}
		default:
			t.Errorf("unexpected %v", x)
		}
	}
	// Past dates are overdue; an item before `from` is never pulled in.
	e = entriesOf(w.ok(t, w.owner, "from=2020-01-01T00:00:00Z&to=2020-01-02T00:00:00Z"))
	if len(e) != 1 || e[0]["item_title"] != "AG old" || e[0]["overdue"] != true {
		t.Errorf("past window = %v, want AG old flagged overdue", e)
	}
	if got := seen(w.ok(t, w.owner, "from=2031-03-11T00:00:00Z&to=2031-03-12T00:00:00Z")); got != "" {
		t.Errorf("nothing is pulled in from before from: %q", got)
	}
}

func TestAgendaOrderIsDeterministic(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.item(t, "tasks", "AG b", `{"status":"open","due_date":"2031-03-10"}`, oneShot())
	w.item(t, "tasks", "AG a", `{"status":"open","due_date":"2031-03-10"}`, oneShot())
	var order []string
	for _, e := range entriesOf(w.ok(t, w.owner, agendaWindow)) {
		order = append(order, fmt.Sprintf("%v:%v", e["type"], e["item_ref"]))
	}
	if len(order) != 4 {
		t.Fatalf("entries = %v", order)
	}
	// By time, then schedule before date, then ref.
	if !(strings.HasPrefix(order[0], "date:") && strings.HasPrefix(order[1], "date:") && strings.HasPrefix(order[2], "schedule:") && strings.HasPrefix(order[3], "schedule:")) ||
		order[0] > order[1] || order[2] > order[3] {
		t.Errorf("order = %v, want the 00:00 dates first (by ref), then the 09:00 schedules (by ref)", order)
	}
}

// hourlyWindow holds 120 occurrences of an HOURLY schedule.
const hourlyWindow = "from=2031-03-10T00:00:00Z&to=2031-03-15T00:00:00Z" // 120 hours

func hourly() *models.ScheduleCreate {
	return &models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-01T00:00", Tz: "UTC", Rrule: "FREQ=HOURLY"}
}

func TestAgendaPerScheduleCutIs100(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.item(t, "tasks", "AG hourly", `{"status":"open"}`, hourly())
	m := w.ok(t, w.owner, hourlyWindow)
	if n := len(entriesOf(m)); n != 100 || m["truncated"] != true {
		t.Fatalf("120 occurrences: %d entries, truncated=%v, want 100 and truncated", n, m["truncated"])
	}
}

func TestAgendaResponseCutIs500(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	for i := 0; i < 6; i++ {
		w.item(t, "tasks", fmt.Sprintf("AG hourly %d", i), `{"status":"open"}`, hourly())
	}
	m := w.ok(t, w.owner, hourlyWindow)
	if n := len(entriesOf(m)); n != 500 || m["truncated"] != true {
		t.Fatalf("600 entries: %d returned, truncated=%v, want 500 and truncated", n, m["truncated"])
	}
}

// r1-1: the scan bound must not be spent on schedules that cannot appear. 1001
// ended one-shots precede (by id) one live schedule; it is still listed.
func TestAgendaScanBoundIsNotSpentOnEndedSchedules(t *testing.T) {
	t.Parallel()
	scanBoundHolds(t, false)
}

// r2-1 (a pin): the same when the ended schedules' only history lies AFTER the
// window, which the history lookup must not count as reaching it.
func TestAgendaScanBoundIsNotSpentOnEndedSchedulesWithLaterHistory(t *testing.T) {
	t.Parallel()
	scanBoundHolds(t, true)
}

func scanBoundHolds(t *testing.T, laterHistory bool) {
	t.Helper()
	w := newAgendaWorld(t, "tasks")
	for i := 0; i < 1002; i++ {
		w.item(t, "tasks", fmt.Sprintf("AG s%04d", i), `{"status":"open"}`, oneShot())
	}
	db, d := w.srv.store.DB(), w.srv.store.D()
	var liveItem, liveID string
	if err := db.QueryRow(`SELECT item_id, id FROM item_schedules WHERE workspace_id = '`+w.ws.ID+`' ORDER BY id DESC LIMIT 1`).Scan(&liveItem, &liveID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(d.Rebind(`UPDATE item_schedules SET ended_at = '2031-01-01T00:00:00Z', next_at = NULL WHERE workspace_id = ? AND id <> ?`), w.ws.ID, liveID); err != nil {
		t.Fatal(err)
	}
	if laterHistory {
		if _, err := db.Exec(d.Rebind(`INSERT INTO schedule_occurrences (id, schedule_id, occurrence_at, occurrence_wall, state, actor, resolved_at)
			SELECT 'o' || id, id, '2031-06-01T09:00:00Z', '2031-06-01T09:00', 'done', 'user', '2031-01-01T00:00:00Z'
			FROM item_schedules WHERE workspace_id = ? AND id <> ?`), w.ws.ID, liveID); err != nil {
			t.Fatal(err)
		}
	}
	m := w.ok(t, w.owner, agendaWindow)
	var found bool
	for _, e := range entriesOf(m) {
		found = found || e["schedule_id"] == liveID
	}
	if !found || m["truncated"] == true {
		t.Fatalf("the live schedule with the highest id is listed=%v, truncated=%v; want listed and not truncated (%d entries)", found, m["truncated"], len(entriesOf(m)))
	}
}

// r2-1: marking a deadline done EARLY ends the schedule at the resolve time,
// while its history row keeps the due instant. The window around the due
// instant still lists it, as done.
func TestAgendaListsAnEarlyResolvedScheduleAsDone(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	it := w.item(t, "tasks", "AG early", `{"status":"open"}`, oneShot())
	if _, err := w.srv.store.ResolveOccurrence(w.ws.ID, it.ID, store.ResolveInput{OccurrenceAt: time.Date(2031, 3, 10, 9, 0, 0, 0, time.UTC), State: "done", Actor: "user"}); err != nil {
		t.Fatalf("resolve early: %v", err)
	}
	m := w.ok(t, w.owner, agendaWindow)
	es := entriesOf(m)
	if len(es) != 1 || es[0]["item_title"] != "AG early" || es[0]["state"] != "done" {
		t.Fatalf("entries = %v, want the early-resolved schedule listed as done", es)
	}
}

// r2-2: a database failure while expanding is a 500, not an incomplete 200.
func TestAgendaDatabaseFailureDuringExpansionIs500(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.item(t, "tasks", "AG db", `{"status":"open"}`, oneShot())
	if _, err := w.srv.store.DB().Exec(`DROP TABLE schedule_occurrences`); err != nil {
		t.Fatal(err)
	}
	if code, m := w.get(t, w.owner, agendaWindow); code != http.StatusInternalServerError {
		t.Fatalf("a failing history read gave %d %v, want 500", code, m)
	}
}

// r1-2: one schedule whose stored state cannot be read is that schedule's
// problem, not a 500 for the agenda.
func TestAgendaCorruptScheduleIsUnavailableNotA500(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.item(t, "tasks", "AG good one", `{"status":"open"}`, oneShot())
	bad := w.item(t, "tasks", "AG corrupt", `{"status":"open"}`, oneShot())
	w.item(t, "tasks", "AG good two", `{"status":"open"}`, oneShot())
	if _, err := w.srv.store.DB().Exec(w.srv.store.D().Rebind(`UPDATE item_schedules SET next_at = 'not-an-instant' WHERE item_id = ?`), bad.ID); err != nil {
		t.Fatal(err)
	}
	code, m := w.get(t, w.owner, agendaWindow)
	if code != http.StatusOK {
		t.Fatalf("a corrupt schedule gave %d %v, want 200", code, m)
	}
	if got, want := seen(m), "schedule:AG good one,schedule:AG good two"; got != want {
		t.Errorf("entries = %s, want %s", got, want)
	}
	un, _ := m["unavailable"].([]any)
	wantRef := fmt.Sprintf("P0-%d", *bad.ItemNumber)
	if len(un) != 1 || fmt.Sprint(un[0]) != fmt.Sprintf("map[code:internal item_ref:%s]", wantRef) {
		t.Errorf("unavailable = %v, want the corrupt schedule (%s) with code internal", un, wantRef)
	}
}

// A read-scoped app token sees what its item list shows, no more.
func TestAgendaAppTokenCeilingMatchesItemList(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks", "secret")
	for _, c := range []string{"tasks", "secret"} {
		w.item(t, c, "AG "+c+" sched", `{"status":"open"}`, oneShot())
		w.item(t, c, "AG "+c+" dated", `{"status":"open","due_date":"2031-03-10"}`, nil)
	}
	member := mustUser(t, w.srv, "agenda-app@example.com", "agendaapp", "")
	if err := w.srv.store.AddWorkspaceMember(w.ws.ID, member.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	call := func(ceiling []string, h http.HandlerFunc, target string) []byte {
		t.Helper()
		req := httptest.NewRequest("GET", target, nil)
		ctx := WithCurrentUser(req.Context(), member)
		ctx = WithAPITokenAuth(ctx)
		ctx = context.WithValue(ctx, ctxResolvedWorkspaceID, w.ws.ID)
		ctx = context.WithValue(ctx, ctxWorkspaceRole, "editor")
		if ceiling != nil {
			ctx = context.WithValue(ctx, appCtxKey{}, &appContext{WorkspaceID: w.ws.ID, ReadCeiling: ceiling})
		}
		rr := httptest.NewRecorder()
		h(rr, req.WithContext(ctx))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", target, rr.Code, rr.Body.String())
		}
		return rr.Body.Bytes()
	}
	titles := func(ceiling []string) (agenda, list string) {
		var a map[string]any
		_ = json.Unmarshal(call(ceiling, w.srv.handleGetAgenda, "/x?"+agendaWindow), &a)
		seenTitles := map[string]bool{}
		for _, e := range entriesOf(a) {
			seenTitles[fmt.Sprint(e["item_title"])] = true
		}
		var items []models.Item
		_ = json.Unmarshal(call(ceiling, w.srv.handleListItems, "/x"), &items)
		listTitles := map[string]bool{}
		for _, it := range items {
			listTitles[it.Title] = true
		}
		join := func(m map[string]bool) string {
			var out []string
			for k := range m {
				out = append(out, k)
			}
			sort.Strings(out)
			return strings.Join(out, ",")
		}
		return join(seenTitles), join(listTitles)
	}
	if a, l := titles(nil); a != l || !strings.Contains(a, "AG secret sched") {
		t.Fatalf("control, no ceiling: agenda %q, list %q; want both to show everything", a, l)
	}
	a, l := titles([]string{w.colls["tasks"].ID})
	if a != l || a != "AG tasks dated,AG tasks sched" {
		t.Errorf("ceiling on tasks: agenda %q, list %q, want both AG tasks dated,AG tasks sched", a, l)
	}
}
