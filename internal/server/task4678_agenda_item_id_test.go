package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4678: every agenda row tied to an item carries the item's id, and a
// dated entry carries the item's ends_on value when it has one.

// task4678Assigned creates an open item assigned to the world's owner, with a
// one-shot schedule when sc is non-nil.
func task4678Assigned(t *testing.T, w *agendaWorld, title, fields string, sc *models.ScheduleCreate) *models.Item {
	t.Helper()
	it, err := w.srv.store.CreateItem(w.ws.ID, w.colls["tasks"].ID, models.ItemCreate{Title: title, Fields: fields, AssignedUserID: &w.owner.ID})
	if err != nil {
		t.Fatalf("CreateItem(%s): %v", title, err)
	}
	if sc != nil {
		if _, err := w.srv.store.CreateSchedule(w.ws.ID, it.ID, *sc); err != nil {
			t.Fatalf("CreateSchedule(%s): %v", title, err)
		}
	}
	return it
}

// task4678Rows returns the rows of one group of an agenda response.
func task4678Rows(m map[string]any, group string) []map[string]any {
	var out []map[string]any
	rows, _ := m[group].([]any)
	for _, r := range rows {
		out = append(out, r.(map[string]any))
	}
	return out
}

// task4678AssertItemIDs checks that every row of the given type in rows that
// names one of want's titles carries that item's id, and that each title was
// seen at least once.
func task4678AssertItemIDs(t *testing.T, where string, rows []map[string]any, typ string, want map[string]*models.Item) {
	t.Helper()
	found := map[string]bool{}
	for _, r := range rows {
		if typ != "" && r["type"] != typ {
			continue
		}
		title, _ := r["item_title"].(string)
		it, ok := want[title]
		if !ok {
			continue
		}
		found[title] = true
		if r["item_id"] != it.ID {
			t.Errorf("%s: %s row %q item_id = %v, want %s", where, typ, title, r["item_id"], it.ID)
		}
	}
	for title := range want {
		if !found[title] {
			t.Errorf("%s: no %s row for %q in %v", where, typ, title, rows)
		}
	}
}

func TestAgendaEntriesCarryItemID(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	dated := w.item(t, "tasks", "IID dated", `{"status":"open","due_date":"2031-03-10"}`, nil)
	sched := w.item(t, "tasks", "IID sched", `{"status":"open"}`, oneShot())
	es := entriesOf(w.ok(t, w.owner, agendaWindow))
	task4678AssertItemIDs(t, "workspace agenda", es, "date", map[string]*models.Item{"IID dated": dated})
	task4678AssertItemIDs(t, "workspace agenda", es, "schedule", map[string]*models.Item{"IID sched": sched})
}

func TestMeAgendaRowsCarryItemID(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	dated := task4678Assigned(t, w, "IID me dated", `{"status":"open","due_date":"2031-03-10"}`, nil)
	sched := task4678Assigned(t, w, "IID me sched", `{"status":"open"}`, oneShot())
	unplaced := task4678Assigned(t, w, "IID me unplaced", `{"status":"open"}`, nil)
	blocked := task4678Assigned(t, w, "IID me blocked", `{"status":"open"}`, oneShot())
	blocker := w.item(t, "tasks", "IID me blocker", `{"status":"open"}`, nil)
	access := models.CommitmentAccess{Readable: map[string]bool{blocked.ID: true, blocker.ID: true}, Editable: map[string]bool{blocked.ID: true, blocker.ID: true}}
	if _, err := w.srv.store.CreateScheduleGate(w.ws.ID, blocked.ID, blocker.ID, access); err != nil {
		t.Fatal(err)
	}
	done := w.item(t, "tasks", "IID me delivered", `{"status":"done"}`, nil)
	meAgendaExec(t, w, `UPDATE items SET created_at = ? WHERE id = ?`, "2031-03-10T01:00:00Z", done.ID)
	meAgendaTransition(t, w, done, "2031-03-10T02:00:00Z", "done")

	m := meAgendaOK(t, w, w.owner, agendaWindow+"&include=created,completed")
	es := entriesOf(m)
	task4678AssertItemIDs(t, "me agenda", es, "date", map[string]*models.Item{"IID me dated": dated})
	task4678AssertItemIDs(t, "me agenda", es, "schedule", map[string]*models.Item{"IID me sched": sched})
	task4678AssertItemIDs(t, "me agenda", es, "created", map[string]*models.Item{"IID me delivered": done})
	task4678AssertItemIDs(t, "me agenda", es, "completed", map[string]*models.Item{"IID me delivered": done})
	task4678AssertItemIDs(t, "me agenda priority_list", task4678Rows(m, "priority_list"), "", map[string]*models.Item{"IID me sched": sched})
	task4678AssertItemIDs(t, "me agenda waiting_on", task4678Rows(m, "waiting_on"), "", map[string]*models.Item{"IID me blocked": blocked})
	noDate := task4678Rows(m, "no_date")
	task4678AssertItemIDs(t, "me agenda no_date", noDate, "", map[string]*models.Item{"IID me unplaced": unplaced})
	for _, r := range noDate {
		if r["item_title"] == "IID me unplaced" && r["reason"] != "unplaced" {
			t.Errorf("no_date row reason = %v, want unplaced", r["reason"])
		}
	}
}

func TestBootstrapAgendaRowsCarryItemID(t *testing.T) {
	t.Parallel()
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	sched := w.sched(t, w.owner, "IID boot sched", "2031-03-10T15:00", "both")
	blocked := w.sched(t, w.owner, "IID boot blocked", "2031-03-10T09:00", "both")
	blocker := w.item(t, "tasks", "IID boot blocker", `{"status":"open"}`, nil)
	access := models.CommitmentAccess{Readable: map[string]bool{blocked.ID: true, blocker.ID: true}, Editable: map[string]bool{blocked.ID: true, blocker.ID: true}}
	if _, err := w.srv.store.CreateScheduleGate(w.ws.ID, blocked.ID, blocker.ID, access); err != nil {
		t.Fatal(err)
	}
	unplaced := task4678Assigned(t, w.agendaWorld, "IID boot unplaced", `{"status":"open"}`, nil)
	rr := doRequestWithHeaders(w.srv, "GET", "/api/v1/workspaces/"+w.ws.Slug+"/agent/bootstrap", nil, map[string]string{"Cookie": "pad_session=" + w.tok})
	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap: %d %s", rr.Code, rr.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var agenda map[string]any
	if err := json.Unmarshal(raw["agenda"], &agenda); err != nil {
		t.Fatalf("agenda: %v %s", err, raw["agenda"])
	}
	task4678AssertItemIDs(t, "bootstrap priority_list", task4678Rows(agenda, "priority_list"), "", map[string]*models.Item{"IID boot sched": sched})
	task4678AssertItemIDs(t, "bootstrap waiting_on", task4678Rows(agenda, "waiting_on"), "", map[string]*models.Item{"IID boot blocked": blocked})
	task4678AssertItemIDs(t, "bootstrap no_date", task4678Rows(agenda, "no_date"), "", map[string]*models.Item{"IID boot unplaced": unplaced})
}

func TestCalendarScheduleEntriesCarryItemID(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	sched := w.item(t, "tasks", "IID cal sched", `{"status":"open"}`, oneShot())
	es := entriesOf(w.calOK(t, w.owner, "tasks", calMarch))
	task4678AssertItemIDs(t, "calendar", es, "schedule", map[string]*models.Item{"IID cal sched": sched})
}

// ends_on is read from the item, whatever collection declares it; it never
// produces a date entry of its own.
func TestAgendaDateEntryCarriesEndsOn(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t)
	schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open","terminal_options":["done"]},` +
		`{"key":"due_date","type":"date"},{"key":"ends_on","type":"date"}]}`
	coll, err := w.srv.store.CreateCollection(w.ws.ID, models.CollectionCreate{Name: "Events", Slug: "events", Prefix: "EV", Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	w.colls["events"] = coll
	spanning := w.item(t, "events", "EO spanning", `{"status":"open","due_date":"2031-03-10","ends_on":"2031-03-12"}`, nil)
	w.item(t, "events", "EO single", `{"status":"open","due_date":"2031-03-10"}`, nil)
	w.item(t, "events", "EO ends only", `{"status":"open","ends_on":"2031-03-10"}`, nil)

	code, m := w.get(t, w.owner, agendaWindow)
	if code != http.StatusOK {
		t.Fatalf("agenda: %d %v", code, m)
	}
	var titles []string
	for _, e := range entriesOf(m) {
		titles = append(titles, e["type"].(string)+":"+e["item_title"].(string))
		switch e["item_title"] {
		case "EO spanning":
			if e["ends_on"] != "2031-03-12" || e["field"] != "due_date" || e["item_id"] != spanning.ID {
				t.Errorf("spanning entry = %v, want ends_on 2031-03-12 on its due_date entry", e)
			}
		case "EO single":
			if _, ok := e["ends_on"]; ok {
				t.Errorf("single-day entry has an ends_on key: %v", e)
			}
		}
	}
	sort.Strings(titles)
	if got, want := strings.Join(titles, ","), "date:EO single,date:EO spanning"; got != want {
		t.Errorf("entries = %s, want %s (ends_on makes no entry of its own)", got, want)
	}
}

// The ICS UIDs keep their shape: <item id>.<field>.<day> for a dated item and
// <schedule id>.<occurrence key> for a schedule occurrence, as the ics writer
// encodes them. Pinned to the literal bytes written before TASK-4678.
func TestCalendarFeedUIDsUnchanged(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.srv.agendaClock = func() time.Time { return meAgendaNow }
	dated := w.item(t, "tasks", "UID dated", `{"status":"open","due_date":"2031-03-10"}`, nil)
	it := w.item(t, "tasks", "UID sched", `{"status":"open"}`, nil)
	sc, err := w.srv.store.CreateSchedule(w.ws.ID, it.ID, *oneShot())
	if err != nil {
		t.Fatal(err)
	}
	f := createTestCalendarFeed(t, w.srv, w.owner, "mine")
	rr := fetchTestCalendarFeed(w.srv, f.Token)
	if rr.Code != http.StatusOK {
		t.Fatalf("fetch %d", rr.Code)
	}
	var uids []string
	for _, line := range strings.Split(strings.ReplaceAll(rr.Body.String(), "\r\n ", ""), "\r\n") {
		if strings.HasPrefix(line, "UID:") {
			uids = append(uids, strings.TrimPrefix(line, "UID:"))
		}
	}
	sort.Strings(uids)
	want := []string{dated.ID + ".due_date.2031-03-10@pad", sc.ID + ".2031-03-10T090000Z2031-03-10T090000@pad"}
	sort.Strings(want)
	if strings.Join(uids, "|") != strings.Join(want, "|") {
		t.Errorf("UIDs = %v, want %v", uids, want)
	}
}
