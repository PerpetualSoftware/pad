package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

const forecastApril = "from=2031-04-01T00:00:00Z&to=2031-04-08T00:00:00Z&tz=UTC"

func forecastOnlyItem(t *testing.T, w *agendaWorld, title, value string) (*models.Item, string) {
	t.Helper()
	it := w.item(t, "tasks", title, `{"status":"open"}`, nil)
	sc, err := w.srv.store.CreateCommitmentSchedule(w.ws.ID, it.ID, *oneShot(), models.CommitmentRolesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.srv.store.SetForecast(w.ws.ID, it.ID, w.owner.ID, "user", value, "estimate"); err != nil {
		t.Fatal(err)
	}
	return it, sc.ID
}

func forecastOnlyPaths(w *agendaWorld) map[string]string {
	return map[string]string{"workspace": "/api/v1/workspaces/" + w.ws.Slug + "/agenda", "personal": "/api/v1/me/agenda", "calendar": "/api/v1/workspaces/" + w.ws.Slug + "/collections/tasks/calendar"}
}

func forecastOnlyGet(t *testing.T, w *agendaWorld, user *models.User, path, query string, mods ...func(context.Context) context.Context) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path+"?date_field=due_date&"+query, nil)
	ctx := WithCurrentUser(req.Context(), user)
	for _, mod := range mods {
		ctx = mod(ctx)
	}
	rr := httptest.NewRecorder()
	w.srv.ensureRouter()
	w.srv.router.ServeHTTP(rr, req.WithContext(ctx))
	return rr
}

func forecastOnlyBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rr.Code != 200 {
		t.Fatalf("response: %d %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func requireForecastOnly(t *testing.T, body map[string]any, id string) map[string]any {
	t.Helper()
	rows, ok := body["forecasts"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("forecasts: got %v, want one forecast", body["forecasts"])
	}
	row := rows[0].(map[string]any)
	if row["schedule_id"] != id || row["day"] != "2031-04-02" || row["all_day"] != true || body["forecasts_complete"] != true {
		t.Fatalf("forecast: %v complete=%v", row, body["forecasts_complete"])
	}
	return row
}

func TestAgendaForecastOnlyAdmission(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.srv.agendaClock = func() time.Time { return meAgendaNow }
	it, id := forecastOnlyItem(t, w, "April forecast", "2031-04-02")
	for name, path := range forecastOnlyPaths(w) {
		t.Run(name, func(t *testing.T) {
			body := forecastOnlyBody(t, forecastOnlyGet(t, w, w.owner, path, forecastApril))
			if len(entriesOf(body)) != 0 {
				t.Fatalf("entries: %v", body["entries"])
			}
			row := requireForecastOnly(t, body, id)
			if row["item_ref"] != it.Ref || row["item_title"] != it.Title || row["collection"] != "tasks" {
				t.Fatalf("item facts: %v", row)
			}
			if name == "personal" && (row["workspace"] != w.ws.Slug || row["workspace_name"] != w.ws.Name || row["owner_username"] != w.owner.Username) {
				t.Fatalf("workspace facts: %v", row)
			}
			if name != "personal" && row["workspace"] != nil {
				t.Fatalf("unexpected workspace: %v", row)
			}
			f := row["forecast"].(map[string]any)
			if f["set_by_name"] != w.owner.Name || f["source"] != "estimate" {
				t.Fatalf("provenance: %v", f)
			}
		})
	}
}

func TestForecastOnlyGuestItemAOnly(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	a, aid := forecastOnlyItem(t, w, "Granted April forecast", "2031-04-02")
	b, bid := forecastOnlyItem(t, w, "Hidden April forecast", "2031-04-02")
	guest := mustUser(t, w.srv, "forecast-only-guest@example.com", "forecastonlyguest", "")
	if _, err := w.srv.store.CreateItemGrant(w.ws.ID, a.ID, guest.ID, "view", w.owner.ID); err != nil {
		t.Fatal(err)
	}
	for name, path := range forecastOnlyPaths(w) {
		t.Run(name, func(t *testing.T) {
			rr := forecastOnlyGet(t, w, guest, path, forecastApril)
			if strings.Contains(rr.Body.String(), b.Ref) || strings.Contains(rr.Body.String(), bid) {
				t.Fatalf("hidden forecast leaked: %s", rr.Body.String())
			}
			requireForecastOnly(t, forecastOnlyBody(t, rr), aid)
		})
	}
}

func TestForecastOnlyOAuthAllowList(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	other := meAgendaWorkspace(t, w, "Other forecasts")
	_, aid := forecastOnlyItem(t, w, "Allowed April", "2031-04-02")
	mod := func(ctx context.Context) context.Context { return WithTokenAllowedWorkspaces(ctx, []string{w.ws.Slug}) }
	before := map[string]*httptest.ResponseRecorder{}
	for name, path := range forecastOnlyPaths(other) {
		before[name] = forecastOnlyGet(t, w, w.owner, path, forecastApril, mod)
	}
	b, bid := forecastOnlyItem(t, other, "Excluded April", "2031-04-02")
	for name, path := range forecastOnlyPaths(other) {
		t.Run(name, func(t *testing.T) {
			rr := forecastOnlyGet(t, w, w.owner, path, forecastApril, mod)
			if strings.Contains(rr.Body.String(), b.Ref) || strings.Contains(rr.Body.String(), bid) {
				t.Fatalf("allowlist leak: %s", rr.Body.String())
			}
			if name == "personal" {
				requireForecastOnly(t, forecastOnlyBody(t, rr), aid)
			} else if rr.Code != before[name].Code || rr.Body.String() != before[name].Body.String() || rr.Code == 200 {
				t.Fatalf("refusal changed: before %d %s after %d %s", before[name].Code, before[name].Body.String(), rr.Code, rr.Body.String())
			}
		})
	}
}

func TestForecastOnlyPinnedWorkspace(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	other := meAgendaWorkspace(t, w, "Other forecasts")
	_, aid := forecastOnlyItem(t, w, "Pinned April", "2031-04-02")
	_, bid := forecastOnlyItem(t, other, "Excluded pinned April", "2031-04-02")
	pin := func(ctx context.Context) context.Context {
		ctx, err := w.srv.withTokenPin(WithTokenWorkspaceID(ctx, w.ws.ID), &models.APIToken{UserID: w.owner.ID, WorkspaceID: w.ws.ID})
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	rr := forecastOnlyGet(t, w, w.owner, "/api/v1/me/agenda", forecastApril, pin)
	requireForecastOnly(t, forecastOnlyBody(t, rr), aid)
	if strings.Contains(rr.Body.String(), bid) {
		t.Fatalf("pin leak: %s", rr.Body.String())
	}
}

func TestForecastOnlyLeavesPagingUnchanged(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.srv.agendaClock = func() time.Time { return meAgendaNow }
	for i := 0; i < 6; i++ {
		sc := hourly()
		sc.Dtstart = "2031-04-01T00:00"
		w.item(t, "tasks", fmt.Sprintf("Hourly %d", i), `{"status":"open"}`, sc)
	}
	before := map[string]map[string]any{}
	for name, path := range forecastOnlyPaths(w) {
		before[name] = forecastOnlyBody(t, forecastOnlyGet(t, w, w.owner, path, forecastApril))
		if before[name]["truncated"] != true {
			t.Fatalf("fixture did not trip entry cap: %s", name)
		}
	}
	for i := 599; i >= 0; i-- {
		forecastOnlyItem(t, w, fmt.Sprintf("Forecast %d", i), time.Date(2031, 4, 2, 0, 0, i, 0, time.UTC).Format(time.RFC3339))
	}
	for name, path := range forecastOnlyPaths(w) {
		t.Run(name, func(t *testing.T) {
			body := forecastOnlyBody(t, forecastOnlyGet(t, w, w.owner, path, forecastApril))
			for _, key := range []string{"entries", "truncated", "next_from", "window_complete"} {
				a, _ := json.Marshal(before[name][key])
				b, _ := json.Marshal(body[key])
				if !bytes.Equal(a, b) {
					t.Fatalf("%s changed: %s -> %s", key, a, b)
				}
			}
			rows, _ := body["forecasts"].([]any)
			if len(rows) != 500 || body["forecasts_complete"] != false {
				t.Fatalf("forecast cap: length=%d complete=%v", len(rows), body["forecasts_complete"])
			}
			if got := rows[499].(map[string]any)["forecast"].(map[string]any)["value"]; got != "2031-04-02T00:08:19Z" {
				t.Fatalf("earliest forecasts: last=%v", got)
			}
		})
	}
}
