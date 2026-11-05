package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4239: GET /workspaces/{ws}/collections/{coll}/calendar.

const calMarch = "from=2031-03-01T00:00:00Z&to=2031-03-31T00:00:00Z&date_field=due_date"

func (w *agendaWorld) cal(t *testing.T, u *models.User, coll, query string) (int, []byte, map[string]any) {
	t.Helper()
	tok, err := w.srv.store.CreateSession(u.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	rr := doRequestWithCookie(w.srv, "GET", "/api/v1/workspaces/"+w.ws.Slug+"/collections/"+coll+"/calendar?"+query, nil, tok)
	m := map[string]any{}
	_ = json.Unmarshal(rr.Body.Bytes(), &m)
	return rr.Code, rr.Body.Bytes(), m
}

func (w *agendaWorld) calOK(t *testing.T, u *models.User, coll, query string) map[string]any {
	t.Helper()
	code, body, m := w.cal(t, u, coll, query)
	if code != http.StatusOK {
		t.Fatalf("calendar %s?%s: %d %s", coll, query, code, body)
	}
	return m
}

func calItems(m map[string]any) []map[string]any {
	var out []map[string]any
	es, _ := m["items"].([]any)
	for _, e := range es {
		out = append(out, e.(map[string]any))
	}
	return out
}

func calItemTitles(m map[string]any) []string {
	var out []string
	for _, it := range calItems(m) {
		out = append(out, it["item"].(map[string]any)["title"].(string))
	}
	sort.Strings(out)
	return out
}

func calEntryTitles(m map[string]any) []string {
	var out []string
	for _, e := range entriesOf(m) {
		out = append(out, e["item_title"].(string))
	}
	sort.Strings(out)
	return out
}

func eq(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func (w *agendaWorld) setCreated(t *testing.T, it *models.Item, at string) {
	t.Helper()
	if _, err := w.srv.store.DB().Exec(`UPDATE items SET created_at = ? WHERE id = ?`, at, it.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCalendarItemsAndSchedulesMerge(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks", "ideas")
	w.item(t, "tasks", "CAL open", `{"status":"open","due_date":"2031-03-10"}`, oneShot())
	w.item(t, "tasks", "CAL done", `{"status":"done","due_date":"2031-03-12"}`, nil)
	w.item(t, "tasks", "CAL sched only", `{"status":"open"}`, oneShot())
	w.item(t, "tasks", "CAL outside", `{"status":"open","due_date":"2031-04-02"}`, nil)
	arch := w.item(t, "tasks", "CAL archived", `{"status":"open","due_date":"2031-03-11"}`, nil)
	if err := w.srv.store.DeleteItem(arch.ID); err != nil {
		t.Fatal(err)
	}
	w.item(t, "ideas", "CAL other collection", `{"status":"open","due_date":"2031-03-10"}`, oneShot())

	// A same-named collection in another workspace never shows.
	other := newAgendaWorld(t, "tasks")
	other.item(t, "tasks", "CAL other workspace", `{"status":"open","due_date":"2031-03-10"}`, oneShot())

	m := w.calOK(t, w.owner, "tasks", calMarch)
	eq(t, "items", calItemTitles(m), "CAL open", "CAL done")
	eq(t, "entries", calEntryTitles(m), "CAL open", "CAL sched only")
	if m["tz"] != "UTC" || m["from"] == nil || m["truncated"] != nil {
		t.Errorf("envelope: %v", m)
	}
	days := map[string]string{}
	for _, it := range calItems(m) {
		days[it["item"].(map[string]any)["title"].(string)] = it["day"].(string)
	}
	if days["CAL open"] != "2031-03-10" || days["CAL done"] != "2031-03-12" {
		t.Errorf("days = %v", days)
	}
	if c, ok := calItems(m)[0]["item"].(map[string]any)["content"]; ok && c != "" {
		t.Errorf("content must not be carried: %v", c)
	}

	// A viewer may read it; a calendar nobody saved needs no editor.
	viewer := mustUser(t, w.srv, "cal-viewer@example.com", "calviewer", "")
	if err := w.srv.store.AddWorkspaceMember(w.ws.ID, viewer.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	eq(t, "viewer items", calItemTitles(w.calOK(t, viewer, "tasks", calMarch)), "CAL open", "CAL done")
}

func TestCalendarWindowAndParameterRefusals(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.calOK(t, w.owner, "tasks", "from=2031-03-01T00:00:00Z&to=2031-06-01T00:00:00Z&date_field=due_date") // 92 days
	for name, c := range map[string]struct{ q, field, code string }{
		"93 days":        {"from=2031-03-01T00:00:00Z&to=2031-06-02T00:00:00Z&date_field=due_date", "", "validation_error"},
		"no date_field":  {"from=2031-03-01T00:00:00Z&to=2031-03-02T00:00:00Z", "date_field", "validation_error"},
		"stale field":    {"from=2031-03-01T00:00:00Z&to=2031-03-02T00:00:00Z&date_field=gone_date", "date_field", "validation_error"},
		"not a date":     {"from=2031-03-01T00:00:00Z&to=2031-03-02T00:00:00Z&date_field=status", "date_field", "validation_error"},
		"end is builtin": {"from=2031-03-01T00:00:00Z&to=2031-03-02T00:00:00Z&date_field=due_date&end_field=created", "end_field", "validation_error"},
		"stale end":      {"from=2031-03-01T00:00:00Z&to=2031-03-02T00:00:00Z&date_field=due_date&end_field=gone_end", "end_field", "validation_error"},
		"hostile key":    {"from=2031-03-01T00:00:00Z&to=2031-03-02T00:00:00Z&date_field=" + url.QueryEscape("due_date') OR 1=1 --"), "date_field", "validation_error"},
		"bad tz":         {"from=2031-03-01T00:00:00Z&to=2031-03-02T00:00:00Z&date_field=due_date&tz=Nope/Zone", "", "invalid_tz"},
	} {
		code, body, m := w.cal(t, w.owner, "tasks", c.q)
		e, _ := m["error"].(map[string]any)
		if code != http.StatusBadRequest || e == nil || e["code"] != c.code {
			t.Errorf("%s: %d %s, want 400 %s", name, code, body, c.code)
			continue
		}
		if d, _ := e["details"].(map[string]any); c.field != "" && (d == nil || d["field"] != c.field) {
			t.Errorf("%s: details = %v, want field %s", name, e["details"], c.field)
		}
	}
	// A stale field is named in the message.
	_, body, _ := w.cal(t, w.owner, "tasks", "from=2031-03-01T00:00:00Z&to=2031-03-02T00:00:00Z&date_field=gone_date")
	if !strings.Contains(string(body), "gone_date") {
		t.Errorf("a stale field must be named: %s", body)
	}
	// The built-ins and a schema date field are accepted.
	for _, f := range []string{"created", "completed", "due_date"} {
		w.calOK(t, w.owner, "tasks", "from=2031-03-01T00:00:00Z&to=2031-03-02T00:00:00Z&date_field="+f)
	}
}

func TestCalendarEndFieldOverlap(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.item(t, "tasks", "span in", `{"due_date":"2031-03-10","end_date":"2031-03-14"}`, nil)
	w.item(t, "tasks", "span from before", `{"due_date":"2031-02-20","end_date":"2031-03-03"}`, nil)
	w.item(t, "tasks", "span over", `{"due_date":"2031-02-01","end_date":"2031-04-30"}`, nil)
	w.item(t, "tasks", "span ended", `{"due_date":"2031-02-01","end_date":"2031-02-20"}`, nil)
	w.item(t, "tasks", "inverted", `{"due_date":"2031-03-10","end_date":"2031-03-05"}`, nil)
	w.item(t, "tasks", "inverted before", `{"due_date":"2031-02-10","end_date":"2031-02-05"}`, nil)
	w.item(t, "tasks", "no end", `{"due_date":"2031-03-20"}`, nil)
	w.item(t, "tasks", "empty end", `{"due_date":"2031-03-21","end_date":""}`, nil)

	m := w.calOK(t, w.owner, "tasks", calMarch+"&end_field=end_date")
	eq(t, "overlap", calItemTitles(m), "span in", "span from before", "span over", "inverted", "no end", "empty end")
	for _, it := range calItems(m) {
		title := it["item"].(map[string]any)["title"]
		switch title {
		case "span in":
			if it["day"] != "2031-03-10" || it["end_day"] != "2031-03-14" {
				t.Errorf("span in: %v", it)
			}
		case "inverted", "no end", "empty end":
			if it["end_day"] != nil {
				t.Errorf("%v is a single day, got end_day %v", title, it["end_day"])
			}
		}
	}
}

func TestCalendarViewerDayPlacement(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	// 23:30 on 10 March in New York (EDT, UTC-4) is already 11 March in UTC.
	late := w.item(t, "tasks", "CAL late", `{}`, nil)
	w.setCreated(t, late, "2031-03-11T03:30:00Z")
	// 23:30 on the 9th in New York is on the 10th in UTC: inside the widened
	// read, outside the viewer's day.
	early := w.item(t, "tasks", "CAL early", `{}`, nil)
	w.setCreated(t, early, "2031-03-10T03:30:00Z")
	// Another day entirely.
	far := w.item(t, "tasks", "CAL far", `{}`, nil)
	w.setCreated(t, far, "2031-03-20T12:00:00Z")

	q := "from=2031-03-10T04:00:00Z&to=2031-03-11T04:00:00Z&date_field=created&tz=America/New_York"
	m := w.calOK(t, w.owner, "tasks", q)
	eq(t, "the viewer's 10 March", calItemTitles(m), "CAL late")
	if got := calItems(m)[0]["day"]; got != "2031-03-10" {
		t.Errorf("day = %v, want the viewer's 2031-03-10", got)
	}
	// In UTC the same instants fall on the UTC days.
	m = w.calOK(t, w.owner, "tasks", "from=2031-03-10T00:00:00Z&to=2031-03-11T00:00:00Z&date_field=created")
	eq(t, "the UTC 10 March", calItemTitles(m), "CAL early")
}

func TestCalendarCompletedPlacement(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	done := w.item(t, "tasks", "CAL finished", `{"status":"open"}`, nil)
	f := `{"status":"done"}`
	if _, err := w.srv.store.UpdateItem(done.ID, models.ItemUpdate{Fields: &f}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.srv.store.DB().Exec(`UPDATE status_transitions SET created_at = ? WHERE item_id = ? AND to_status = 'done'`, "2031-03-11T03:30:00Z", done.ID); err != nil {
		t.Fatal(err)
	}
	w.item(t, "tasks", "CAL open", `{"status":"open"}`, nil)
	m := w.calOK(t, w.owner, "tasks", "from=2031-03-10T04:00:00Z&to=2031-03-11T04:00:00Z&date_field=completed&tz=America/New_York")
	eq(t, "completed on the viewer's day", calItemTitles(m), "CAL finished")
	if calItems(m)[0]["day"] != "2031-03-10" {
		t.Errorf("day = %v", calItems(m)[0]["day"])
	}
}

func TestCalendarGuestScoping(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks", "secret")
	w.item(t, "tasks", "CAL granted", `{"status":"open","due_date":"2031-03-10"}`, oneShot())
	w.item(t, "secret", "CAL secret", `{"status":"open","due_date":"2031-03-10"}`, oneShot())
	guest := mustUser(t, w.srv, "cal-guest@example.com", "calguest", "")
	if _, err := w.srv.store.CreateItemGrant(w.ws.ID, w.items["CAL granted"].ID, guest.ID, "view", w.owner.ID); err != nil {
		t.Fatal(err)
	}
	w.item(t, "tasks", "CAL hidden sched", `{"status":"open","due_date":"2031-03-10"}`, oneShot())
	// A busy hidden neighbour: far over the cap, on the same day, in the same
	// collection. The guest's answer must be the one it would get without it.
	for i := 0; i < agendaMaxEntries+100; i++ {
		w.item(t, "tasks", fmt.Sprintf("CAL hidden %04d", i), `{"status":"open","due_date":"2031-03-10"}`, nil)
	}
	for _, q := range []string{calMarch, calMarch + "&status=open", calMarch + "&end_field=end_date"} {
		m := w.calOK(t, guest, "tasks", q)
		eq(t, "guest items "+q, calItemTitles(m), "CAL granted")
		eq(t, "guest entries "+q, calEntryTitles(m), "CAL granted")
		for _, k := range []string{"truncated", "next_from", "partial_day", "unexpanded", "unavailable"} {
			if _, has := m[k]; has {
				t.Errorf("%s: the hidden items leaked into %q: %v", q, k, m[k])
			}
		}
	}
	// The guest's filter that the granted item fails removes it and its schedule.
	m := w.calOK(t, guest, "tasks", calMarch+"&status=done")
	eq(t, "filtered out", calItemTitles(m))
	eq(t, "filtered out entries", calEntryTitles(m))

	// A collection the guest has no access to answers as the item list does,
	// and as a collection that does not exist.
	tok, _ := w.srv.store.CreateSession(guest.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	base := "/api/v1/workspaces/" + w.ws.Slug + "/collections/"
	list := doRequestWithCookie(w.srv, "GET", base+"secret/items", nil, tok)
	hidden := doRequestWithCookie(w.srv, "GET", base+"secret/calendar?"+calMarch, nil, tok)
	missing := doRequestWithCookie(w.srv, "GET", base+"no-such-collection/calendar?"+calMarch, nil, tok)
	if list.Code != http.StatusNotFound || hidden.Code != http.StatusNotFound {
		t.Fatalf("hidden: list %d calendar %d", list.Code, hidden.Code)
	}
	if hidden.Body.String() != list.Body.String() || missing.Body.String() != list.Body.String() {
		t.Errorf("404 bytes differ: list %q calendar %q missing %q", list.Body, hidden.Body, missing.Body)
	}
	// The refusal comes before any parameter is read: no oracle in a 400.
	bad := doRequestWithCookie(w.srv, "GET", base+"secret/calendar?from=x", nil, tok)
	if bad.Body.String() != list.Body.String() {
		t.Errorf("a hidden collection with bad parameters answered %q", bad.Body)
	}
}

func TestCalendarCollectionGuestKeepsOtherGrants(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks", "ideas")
	w.item(t, "tasks", "CAL granted", `{"status":"open","due_date":"2031-03-10"}`, oneShot())
	w.item(t, "tasks", "CAL not granted", `{"status":"open","due_date":"2031-03-10"}`, oneShot())
	w.item(t, "ideas", "CAL idea", `{"status":"open","due_date":"2031-03-10"}`, oneShot())
	guest := mustUser(t, w.srv, "cal-guest2@example.com", "calguest2", "")
	if _, err := w.srv.store.CreateItemGrant(w.ws.ID, w.items["CAL granted"].ID, guest.ID, "view", w.owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.srv.store.CreateCollectionGrant(w.ws.ID, w.colls["ideas"].ID, guest.ID, "view", w.owner.ID); err != nil {
		t.Fatal(err)
	}
	// Item grant in tasks plus a full grant on ideas: the path collection narrows
	// to tasks and the full grant on ideas does not widen it.
	for _, q := range []string{calMarch, calMarch + "&status=open"} {
		m := w.calOK(t, guest, "tasks", q)
		eq(t, "tasks items "+q, calItemTitles(m), "CAL granted")
		eq(t, "tasks entries "+q, calEntryTitles(m), "CAL granted")
		m = w.calOK(t, guest, "ideas", q)
		eq(t, "ideas items "+q, calItemTitles(m), "CAL idea")
		eq(t, "ideas entries "+q, calEntryTitles(m), "CAL idea")
	}
}

func TestCalendarPageFiltersExcludeItemsAndTheirSchedules(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.item(t, "tasks", "CAL keep", `{"status":"open","due_date":"2031-03-10"}`, oneShot())
	// Its date is outside the month, its schedule inside: a schedule's item need
	// not be dated in the window, and the filter must still apply to it.
	w.item(t, "tasks", "CAL keep undated", `{"status":"open"}`, oneShot())
	w.item(t, "tasks", "CAL drop", `{"status":"done","due_date":"2031-03-10"}`, oneShot())

	m := w.calOK(t, w.owner, "tasks", calMarch) // no filter: the whole page, terminal included
	eq(t, "unfiltered items", calItemTitles(m), "CAL keep", "CAL drop")
	// A done item's schedule is not on the calendar: the schedules are the
	// agenda's read, which is of open items.
	eq(t, "unfiltered entries", calEntryTitles(m), "CAL keep", "CAL keep undated")

	m = w.calOK(t, w.owner, "tasks", calMarch+"&status=open")
	eq(t, "filtered items", calItemTitles(m), "CAL keep")
	eq(t, "filtered entries", calEntryTitles(m), "CAL keep", "CAL keep undated")

	// An OPEN item whose schedule is filtered out with it: its date is not the
	// asked one, so its schedule entry must not stay on the page.
	m = w.calOK(t, w.owner, "tasks", calMarch+"&due_date=2031-03-10")
	eq(t, "field-filtered items", calItemTitles(m), "CAL keep")
	eq(t, "field-filtered entries", calEntryTitles(m), "CAL keep")

	// A filter key the collection does not have is refused as the list refuses it.
	code, body, _ := w.cal(t, w.owner, "tasks", calMarch+"&nosuchfield=1")
	if code != http.StatusBadRequest || !strings.Contains(string(body), "invalid list filter") {
		t.Errorf("unknown filter: %d %s", code, body)
	}
	// The request cannot reach the query's own switches.
	m = w.calOK(t, w.owner, "tasks", calMarch+"&include_archived=true&non_terminal=true&limit=1&offset=5&sort=title:desc")
	eq(t, "request switches ignored", calItemTitles(m), "CAL keep", "CAL drop")
}

func TestCalendarCapIsOneAndCutsBetweenDays(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	// 3 days x 200 created items = 600 > 500: two whole days, then a cut.
	want := map[string]bool{}
	for d := 1; d <= 3; d++ {
		for i := 0; i < 200; i++ {
			it := w.item(t, "tasks", fmt.Sprintf("CAP d%d-%03d", d, i), `{}`, nil)
			w.setCreated(t, it, fmt.Sprintf("2031-03-%02dT%02d:%02d:00Z", d, 8+i/60, i%60))
			want[it.Title] = true
		}
	}
	q := "from=2031-03-01T00:00:00Z&to=2031-03-11T00:00:00Z&date_field=created"
	m := w.calOK(t, w.owner, "tasks", q)
	if len(calItems(m)) != 400 || m["truncated"] != true || m["next_from"] != "2031-03-03" {
		t.Fatalf("first page: %d items truncated=%v next_from=%v", len(calItems(m)), m["truncated"], m["next_from"])
	}
	if _, has := m["partial_day"]; has {
		t.Errorf("a cut between days must not set partial_day: %v", m["partial_day"])
	}
	for _, it := range calItems(m) {
		if d := it["day"].(string); d >= "2031-03-03" {
			t.Errorf("a day at or after next_from was shown: %v", it["day"])
		}
	}
	// Page from next_from: the rest, no gap, no repeat.
	m2 := w.calOK(t, w.owner, "tasks", "from=2031-03-03T00:00:00Z&to=2031-03-11T00:00:00Z&date_field=created")
	if m2["truncated"] != nil || len(calItems(m2)) != 200 {
		t.Fatalf("second page: %d items truncated=%v", len(calItems(m2)), m2["truncated"])
	}
	seen := map[string]int{}
	for _, t := range append(calItemTitles(m), calItemTitles(m2)...) {
		seen[t]++
	}
	if len(seen) != 600 {
		t.Errorf("pages cover %d distinct items, want 600", len(seen))
	}
	for title, n := range seen {
		if n != 1 || !want[title] {
			t.Errorf("%s seen %d times", title, n)
		}
	}
}

func TestCalendarCapSchedulesCountToo(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	// 300 items dated the 10th and 300 schedules on the 10th... on the 11th: the
	// two days sum to 600 but fit one by one, so day 11 is the cut.
	for i := 0; i < 300; i++ {
		w.item(t, "tasks", fmt.Sprintf("CAS item %03d", i), `{"due_date":"2031-03-10"}`, nil)
	}
	for i := 0; i < 250; i++ {
		w.item(t, "tasks", fmt.Sprintf("CAS sched %03d", i), `{}`, &models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-11T09:00", Tz: "UTC"})
	}
	m := w.calOK(t, w.owner, "tasks", calMarch)
	if len(calItems(m))+len(entriesOf(m)) != 300 || m["truncated"] != true || m["next_from"] != "2031-03-11" {
		t.Fatalf("items %d entries %d truncated=%v next_from=%v", len(calItems(m)), len(entriesOf(m)), m["truncated"], m["next_from"])
	}
}

func TestCalendarOneDayOverCapIsPartial(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	for i := 0; i < 520; i++ {
		it := w.item(t, "tasks", fmt.Sprintf("OVR a-%03d", i), `{}`, nil)
		w.setCreated(t, it, fmt.Sprintf("2031-03-02T%02d:%02d:00Z", 8+i/60, i%60))
	}
	for i := 0; i < 5; i++ {
		it := w.item(t, "tasks", fmt.Sprintf("OVR b-%d", i), `{}`, nil)
		w.setCreated(t, it, "2031-03-03T09:00:00Z")
	}
	q := "from=2031-03-01T00:00:00Z&to=2031-03-11T00:00:00Z&date_field=created"
	m := w.calOK(t, w.owner, "tasks", q)
	if len(calItems(m)) != agendaMaxEntries || m["truncated"] != true || m["partial_day"] != "2031-03-02" || m["next_from"] != "2031-03-03" {
		t.Fatalf("over-full day: %d items truncated=%v partial_day=%v next_from=%v", len(calItems(m)), m["truncated"], m["partial_day"], m["next_from"])
	}
	// Stable: the same request answers the same items, oldest first.
	m2 := w.calOK(t, w.owner, "tasks", q)
	a, b := calItemTitles(m), calItemTitles(m2)
	if strings.Join(a, "|") != strings.Join(b, "|") {
		t.Errorf("an over-full day changed between identical requests")
	}
	if first := calItems(m)[0]["item"].(map[string]any)["title"]; first != "OVR a-000" {
		t.Errorf("first item = %v, want the day's oldest", first)
	}
	// The next day is reachable from next_from.
	m3 := w.calOK(t, w.owner, "tasks", "from=2031-03-03T00:00:00Z&to=2031-03-11T00:00:00Z&date_field=created")
	if len(calItems(m3)) != 5 || m3["truncated"] != nil {
		t.Errorf("paged day: %d items truncated=%v", len(calItems(m3)), m3["truncated"])
	}
}

func TestCalendarShareLink(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.item(t, "tasks", "CAL shared", `{"status":"open","due_date":"2031-03-10"}`, nil)
	coll := w.colls["tasks"]
	if _, err := w.srv.store.CreateView(w.ws.ID, models.ViewCreate{CollectionID: &coll.ID, Name: "Cal", ViewType: "calendar", Config: `{"date_field":"due_date"}`}); err != nil {
		t.Fatal(err)
	}
	link, err := w.srv.store.CreateShareLink(w.ws.ID, "collection", coll.ID, "view", w.owner.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	rr := doRequest(w.srv, "GET", "/api/v1/s/"+link.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("share resolve: %d %s", rr.Code, rr.Body)
	}
	var resp struct {
		Collection struct {
			Views []struct {
				Slug     string `json:"slug"`
				ViewType string `json:"view_type"`
			} `json:"views"`
		} `json:"collection"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || len(resp.Collection.Views) != 1 {
		t.Fatalf("share payload: %v %s", err, rr.Body)
	}
	if got := resp.Collection.Views[0].ViewType; got != "list" {
		t.Errorf("public view_type = %q, want list", got)
	}

	// The calendar route is not a share route: a share token authenticates nothing.
	path := "/api/v1/workspaces/" + w.ws.Slug + "/collections/tasks/calendar?" + calMarch
	for name, rr := range map[string]*httptest.ResponseRecorder{
		"bearer":    doRequestWithBearer(w.srv, "GET", path, link.Token, nil),
		"anonymous": doRequest(w.srv, "GET", path, nil),
		"query":     doRequest(w.srv, "GET", path+"&token="+link.Token, nil),
	} {
		if rr.Code == http.StatusOK || strings.Contains(rr.Body.String(), "CAL shared") {
			t.Errorf("%s: %d %s", name, rr.Code, rr.Body)
		}
	}
}

func TestViewTypeValidation(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	tok, err := w.srv.store.CreateSession(w.owner.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/workspaces/" + w.ws.Slug + "/collections/tasks/views"
	create := func(viewType, config string) (int, map[string]any) {
		body := map[string]any{"name": "V " + viewType, "view_type": viewType}
		if config != "" {
			var cfg any
			if json.Unmarshal([]byte(config), &cfg) == nil {
				body["config"] = cfg
			} else {
				body["config"] = config
			}
		}
		rr := doRequestWithCookie(w.srv, "POST", base, body, tok)
		m := map[string]any{}
		_ = json.Unmarshal(rr.Body.Bytes(), &m)
		return rr.Code, m
	}
	refuse := func(what string, code int, m map[string]any, field string) {
		t.Helper()
		e, _ := m["error"].(map[string]any)
		d, _ := e["details"].(map[string]any)
		if field == "" { // refused before the calendar check, by the config's own shape rule
			if code != http.StatusBadRequest {
				t.Errorf("%s: %d %v, want 400", what, code, m)
			}
			return
		}
		if code != http.StatusBadRequest || e == nil || d == nil || d["field"] != field || d["code"] == nil {
			t.Errorf("%s: %d %v, want 400 with details.field %s", what, code, m, field)
		}
	}
	for name, c := range map[string]struct{ config, field string }{
		"no config":             {``, "date_field"},
		"empty object":          {`{}`, "date_field"},
		"unknown field":         {`{"date_field":"nope"}`, "date_field"},
		"not a date":            {`{"date_field":"status"}`, "date_field"},
		"date_field not string": {`{"date_field":5}`, "date_field"},
		"unknown end":           {`{"date_field":"due_date","end_field":"nope"}`, "end_field"},
		"builtin end":           {`{"date_field":"due_date","end_field":"created"}`, "end_field"},
		"array config":          {`[1]`, ""},
		"null config":           {`null`, ""},
		"string config":         {`"x"`, ""},
	} {
		code, m := create("calendar", c.config)
		refuse("create "+name, code, m, c.field)
	}
	for name, config := range map[string]string{
		"date field":           `{"date_field":"due_date"}`,
		"created":              `{"date_field":"created"}`,
		"completed":            `{"date_field":"completed"}`,
		"span":                 `{"date_field":"due_date","end_field":"end_date"}`,
		"with filters":         `{"date_field":"due_date","filters":[{"field":"status","op":"eq","value":"open"}]}`,
		"with a reserved item": `{"date_field":"due_date","filters":[{"field":"$unparented","op":"eq","value":true}]}`,
	} {
		if code, m := create("calendar", config); code != http.StatusCreated {
			t.Errorf("create %s: %d %v", name, code, m)
		}
	}
	// Plain types are not checked, whatever their config says.
	for _, vt := range []string{"list", "board", "table", "something_new"} {
		if code, m := create(vt, `{"date_field":"nope"}`); code != http.StatusCreated {
			t.Errorf("create %s: %d %v", vt, code, m)
		}
	}

	// Patch: the merged result is checked.
	_, list := create("list", `{"group_by":"status"}`)
	listID := list["id"].(string)
	patch := func(id string, body map[string]any) (int, map[string]any) {
		rr := doRequestWithCookie(w.srv, "PATCH", base+"/"+id, body, tok)
		m := map[string]any{}
		_ = json.Unmarshal(rr.Body.Bytes(), &m)
		return rr.Code, m
	}
	code, m := patch(listID, map[string]any{"view_type": "calendar"}) // list config has no date_field
	refuse("patch list -> calendar without date_field", code, m, "date_field")
	code, m = patch(listID, map[string]any{"view_type": "calendar", "config": map[string]any{"date_field": "due_date"}})
	if code != http.StatusOK {
		t.Fatalf("patch list -> calendar with a date field: %d %v", code, m)
	}
	calID := m["id"].(string)
	code, m = patch(calID, map[string]any{"config": map[string]any{"date_field": "status"}})
	refuse("patch config to a non-date", code, m, "date_field")
	code, m = patch(calID, map[string]any{"config": map[string]any{"date_field": "due_date", "end_field": "created"}})
	refuse("patch config to a built-in end", code, m, "end_field")
	code, m = patch(calID, map[string]any{"config": []any{1}})
	refuse("patch config to an array", code, m, "")
	if code, m = patch(calID, map[string]any{"name": "Renamed"}); code != http.StatusOK {
		t.Errorf("a rename of a valid calendar: %d %v", code, m)
	}
	if code, m = patch(calID, map[string]any{"view_type": "list"}); code != http.StatusOK {
		t.Errorf("calendar -> list: %d %v", code, m)
	}
	// Back to list with a stale date_field config, then a rename: not re-checked.
	if code, m = patch(calID, map[string]any{"config": map[string]any{"date_field": "gone"}}); code != http.StatusOK {
		t.Errorf("a list view keeps any config: %d %v", code, m)
	}
	if code, m = patch(calID, map[string]any{"name": "Again"}); code != http.StatusOK {
		t.Errorf("rename: %d %v", code, m)
	}
}
