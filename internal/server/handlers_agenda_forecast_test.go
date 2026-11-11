package server

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func forecastWorld(t *testing.T) (*agendaWorld, *models.Item) {
	t.Helper()
	w := newAgendaWorld(t, "tasks")
	w.srv.agendaClock = func() time.Time { return time.Date(2031, 3, 10, 8, 0, 0, 0, time.UTC) }
	sc := oneShot()
	sc.Rrule = "FREQ=WEEKLY"
	it := w.item(t, "tasks", "Forecast work", `{"status":"open"}`, nil)
	days, secs, behavior := 2, 0, "stays_owed"
	if _, err := w.srv.store.CreateCommitmentSchedule(w.ws.ID, it.ID, *sc, models.CommitmentRolesInput{DeadlineDays: &days, DeadlineSecs: &secs, DeadlineBehavior: &behavior}); err != nil {
		t.Fatal(err)
	}
	meAgendaExec(t, w, `UPDATE items SET created_by_user_id=? WHERE id=?`, w.owner.ID, it.ID)
	if err := w.srv.store.SetForecast(w.ws.ID, it.ID, w.owner.ID, "agent", "2031-03-12", "hand estimate"); err != nil {
		t.Fatal(err)
	}
	return w, it
}

func assertForecastMember(t *testing.T, e map[string]any) {
	t.Helper()
	f, ok := e["forecast"].(map[string]any)
	if !ok || f["value"] != "2031-03-12" || f["set_by_name"] != "agendaowner" || f["set_by_kind"] != "agent" || f["source"] != "hand estimate" {
		t.Fatalf("forecast: %v", e)
	}
	for _, key := range []string{"passed", "history"} {
		if _, ok := f[key]; ok {
			t.Fatalf("feed includes %s: %v", key, f)
		}
	}
}

func TestAgendaEntryCarriesForecast(t *testing.T) {
	t.Parallel()
	w, it := forecastWorld(t)
	before, err := w.srv.store.GetCommitment(w.ws.ID, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := w.ok(t, w.owner, "from=2031-03-10T00:00:00Z&to=2031-03-25T00:00:00Z")
	outstanding, future := 0, 0
	for _, e := range entriesOf(m) {
		if e["state"] == "outstanding" {
			outstanding++
			assertForecastMember(t, e)
		} else {
			future++
			if e["forecast"] != nil {
				t.Fatalf("future forecast: %v", e)
			}
		}
	}
	if outstanding != 1 || future != 2 {
		t.Fatalf("entries = %v", m)
	}
	after, err := w.srv.store.GetCommitment(w.ws.ID, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Roles, after.Roles) {
		t.Fatalf("roles changed: %+v -> %+v", before.Roles, after.Roles)
	}
}

func TestMeAgendaPriorityListCarriesForecast(t *testing.T) {
	t.Parallel()
	w, _ := forecastWorld(t)
	m := meAgendaOK(t, w, w.owner, agendaWindow)
	es := entriesOf(m)
	ps, _ := m["priority_list"].([]any)
	if len(es) != 1 || len(ps) != 1 {
		t.Fatalf("response = %v", m)
	}
	assertForecastMember(t, es[0])
	assertForecastMember(t, ps[0].(map[string]any))
	if !reflect.DeepEqual(es[0]["forecast"], ps[0].(map[string]any)["forecast"]) {
		t.Fatalf("forecast mismatch: %v", m)
	}
}

func TestCalendarEntryCarriesForecast(t *testing.T) {
	t.Parallel()
	w, _ := forecastWorld(t)
	es := entriesOf(w.calOK(t, w.owner, "tasks", calMarch))
	count := 0
	for _, e := range es {
		if e["state"] == "outstanding" {
			count++
			assertForecastMember(t, e)
		}
	}
	if count != 1 {
		t.Fatalf("entries = %v", es)
	}
}

func TestAgendaPastForecastIsNotAMiss(t *testing.T) {
	t.Parallel()
	w, it := forecastWorld(t)
	w.srv.agendaClock = func() time.Time { return time.Date(2031, 3, 9, 8, 0, 0, 0, time.UTC) }
	if err := w.srv.store.SetForecast(w.ws.ID, it.ID, w.owner.ID, "user", "2031-03-08", ""); err != nil {
		t.Fatal(err)
	}
	count := func(table string) int {
		var n int
		if err := w.srv.store.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	occ, journal := count("schedule_occurrences"), count("schedule_changes")
	m := meAgendaOK(t, w, w.owner, agendaWindow)
	ps, _ := m["priority_list"].([]any)
	if len(ps) != 1 {
		t.Fatalf("priority = %v", m)
	}
	e := ps[0].(map[string]any)
	if e["overdue"] == true || (e["missed_count"] != nil && e["missed_count"] != float64(0)) || e["reason"] != "outstanding" || e["forecast"] == nil {
		t.Fatalf("past estimate: %v", e)
	}
	if count("schedule_occurrences") != occ || count("schedule_changes") != journal {
		t.Fatal("agenda read wrote history")
	}
}

func TestAgendaForecastGuestScope(t *testing.T) {
	t.Parallel()
	w, a := forecastWorld(t)
	b := w.item(t, "tasks", "Hidden forecast", `{"status":"open"}`, oneShot())
	if err := w.srv.store.SetForecast(w.ws.ID, b.ID, w.owner.ID, "user", "2031-03-13", "SECRET-B-source"); err != nil {
		t.Fatal(err)
	}
	guest := mustUser(t, w.srv, "forecast-guest@example.com", "forecastguest", "")
	if _, err := w.srv.store.CreateItemGrant(w.ws.ID, a.ID, guest.ID, "view", w.owner.ID); err != nil {
		t.Fatal(err)
	}
	for _, m := range []map[string]any{w.ok(t, guest, agendaWindow), meAgendaOK(t, w, guest, agendaWindow), w.calOK(t, guest, "tasks", calMarch)} {
		es := entriesOf(m)
		if len(es) < 1 {
			t.Fatalf("empty: %v", m)
		}
		assertForecastMember(t, es[0])
		raw, _ := json.Marshal(m)
		if strings.Contains(string(raw), b.Ref) || strings.Contains(string(raw), b.Slug) || strings.Contains(string(raw), "SECRET-B-source") {
			t.Fatalf("guest leak: %s", raw)
		}
	}
}

func TestCommitmentViewForecastSetterName(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t).milestone()
	for _, agent := range []string{"", "test-agent"} {
		tok := f.token(f.owner, "")
		req := httptest.NewRequest("PUT", f.ms("/forecast"), strings.NewReader(`{"value":"2031-03-12"}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		if agent != "" {
			req.Header.Set("X-Pad-Agent", agent)
		}
		rr := httptest.NewRecorder()
		f.srv.ensureRouter()
		f.srv.router.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("write: %d %s", rr.Code, rr.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		fc := body["forecast"].(map[string]any)
		kind := "user"
		if agent != "" {
			kind = "agent"
		}
		if fc["set_by_name"] != f.owner.Name || fc["set_by_kind"] != kind {
			t.Fatalf("forecast = %v", fc)
		}
	}
}
