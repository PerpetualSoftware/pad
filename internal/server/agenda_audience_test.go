package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func meAgendaGetWithHeaders(t *testing.T, w *agendaWorld, u *models.User, query string, headers map[string]string, mods ...func(context.Context) context.Context) *httptest.ResponseRecorder {
	t.Helper()
	if w.srv.agendaClock == nil {
		w.srv.agendaClock = func() time.Time { return meAgendaNow }
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/agenda?"+query, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	ctx := req.Context()
	if u != nil {
		ctx = WithCurrentUser(ctx, u)
	}
	for _, mod := range mods {
		ctx = mod(ctx)
	}
	rr := httptest.NewRecorder()
	w.srv.ensureRouter()
	w.srv.router.ServeHTTP(rr, req.WithContext(ctx))
	return rr
}

func workspaceAgendaGetWithHeaders(t *testing.T, w *agendaWorld, u *models.User, query string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	tok, err := w.srv.store.CreateSession(u.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/"+w.ws.Slug+"/agenda?"+query, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(false), Value: tok})
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	w.srv.ensureRouter()
	w.srv.router.ServeHTTP(rr, req)
	m := map[string]any{}
	_ = json.Unmarshal(rr.Body.Bytes(), &m)
	return rr.Code, m
}

func scheduleWithAudience(audience string) *models.ScheduleCreate {
	sc := oneShot()
	sc.Audience = audience
	return sc
}

func TestMeAgendaAudienceHumanMatchesUIMine(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	other := meAgendaWorkspace(t, w, "Other")

	w.item(t, "tasks", "WS1 Agent", `{"status":"open"}`, scheduleWithAudience("agent"))
	w.item(t, "tasks", "WS1 Human", `{"status":"open"}`, scheduleWithAudience("human"))
	w.item(t, "tasks", "WS1 Both", `{"status":"open"}`, scheduleWithAudience("both"))
	w.item(t, "tasks", "WS1 Dated", `{"status":"open","due_date":"2031-03-10"}`, nil)
	other.item(t, "tasks", "WS2 Agent", `{"status":"open"}`, scheduleWithAudience("agent"))
	other.item(t, "tasks", "WS2 Human", `{"status":"open"}`, scheduleWithAudience("human"))

	m := meAgendaOK(t, w, w.owner, agendaWindow+"&audience=human&include=")
	if m["audience"] != "human" {
		t.Fatalf("audience = %v, want human", m["audience"])
	}
	got := seen(m)
	want := "date:WS1 Dated,schedule:WS1 Both,schedule:WS1 Human,schedule:WS2 Human"
	if got != want {
		t.Fatalf("entries = %s, want %s", got, want)
	}
	workspaces := map[string]bool{}
	for _, e := range entriesOf(m) {
		if e["type"] == "schedule" && e["audience"] == "agent" {
			t.Errorf("agent schedule leaked into human view: %v", e)
		}
		if ws, _ := e["workspace"].(string); ws != "" {
			workspaces[ws] = true
		}
	}
	if !workspaces[w.ws.Slug] || !workspaces[other.ws.Slug] {
		t.Fatalf("expected rows from both workspaces %q and %q, got %v", w.ws.Slug, other.ws.Slug, workspaces)
	}
}

func TestMeAgendaAudienceAgentLayer(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.srv.agendaClock = func() time.Time { return meAgendaNow }

	agentItem := w.item(t, "tasks", "Agent Work", `{"status":"open"}`, scheduleWithAudience("agent"))
	humanItem := w.item(t, "tasks", "Human Work", `{"status":"open"}`, scheduleWithAudience("human"))
	w.item(t, "tasks", "Both Work", `{"status":"open"}`, scheduleWithAudience("both"))
	dated := w.item(t, "tasks", "Dated Work", `{"status":"open","due_date":"2031-03-10"}`, nil)
	meAgendaExec(t, w, `UPDATE items SET created_at = ? WHERE id = ?`, "2031-03-10T01:00:00Z", dated.ID)
	meAgendaTransition(t, w, dated, "2031-03-10T02:00:00Z", "done")

	for _, it := range []*models.Item{agentItem, humanItem} {
		if err := w.srv.store.SetForecast(w.ws.ID, it.ID, w.owner.ID, "user", "2031-04-02", "estimate"); err != nil {
			t.Fatal(err)
		}
	}

	m := meAgendaOK(t, w, w.owner, agendaWindow+"&audience=agent&include=created,completed")
	if m["audience"] != "agent" {
		t.Fatalf("audience = %v, want agent", m["audience"])
	}
	// The agent layer keeps agent and both schedules and dated items; only the
	// human-only schedule leaves.
	got := "," + seen(m) + ","
	for _, want := range []string{"schedule:Agent Work", "schedule:Both Work", "date:Dated Work"} {
		if !strings.Contains(got, ","+want+",") {
			t.Fatalf("agent layer entries = %s, want %s among them", got, want)
		}
	}
	if strings.Contains(got, "schedule:Human Work") {
		t.Fatalf("agent layer entries = %s, want no human-only schedule", got)
	}

	fm := meAgendaOK(t, w, w.owner, forecastApril+"&audience=agent")
	forecasts, _ := fm["forecasts"].([]any)
	if len(forecasts) != 1 {
		t.Fatalf("agent forecasts = %v, want 1", forecasts)
	}
	row := forecasts[0].(map[string]any)
	if row["item_ref"] != agentItem.Ref || row["audience"] != "agent" {
		t.Fatalf("unexpected forecast row: %v", row)
	}
}

func TestMeAgendaAudienceOverridesActor(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.srv.agendaClock = func() time.Time { return meAgendaNow }

	agentItem := w.item(t, "tasks", "Agent Only", `{"status":"open"}`, scheduleWithAudience("agent"))
	humanItem := w.item(t, "tasks", "Human Only", `{"status":"open"}`, scheduleWithAudience("human"))
	for _, it := range []*models.Item{agentItem, humanItem} {
		meAgendaExec(t, w, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, w.owner.ID, it.ID)
	}

	rr := meAgendaGetWithHeaders(t, w, w.owner, agendaWindow+"&audience=human", map[string]string{"X-Pad-Agent": "tern"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["audience"] != "human" {
		t.Fatalf("audience = %v, want human", m["audience"])
	}
	priority, _ := m["priority_list"].([]any)
	foundHuman := false
	for _, raw := range priority {
		row := raw.(map[string]any)
		if row["item_ref"] == agentItem.Ref {
			t.Fatalf("agent-only item leaked into human priority_list: %v", row)
		}
		if row["item_ref"] == humanItem.Ref {
			foundHuman = true
		}
	}
	if !foundHuman {
		t.Fatalf("expected human item %s in priority_list: %v", humanItem.Ref, priority)
	}
}

func TestMeAgendaAudienceAbsentUnchanged(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")

	w.item(t, "tasks", "Agent Work", `{"status":"open"}`, scheduleWithAudience("agent"))
	w.item(t, "tasks", "Human Work", `{"status":"open"}`, scheduleWithAudience("human"))
	w.item(t, "tasks", "Both Work", `{"status":"open"}`, scheduleWithAudience("both"))
	w.item(t, "tasks", "Dated Work", `{"status":"open","due_date":"2031-03-10"}`, nil)

	m := meAgendaOK(t, w, w.owner, agendaWindow+"&include=")
	if _, has := m["audience"]; has {
		t.Fatalf("expected audience key omitted when parameter absent, got %v", m["audience"])
	}
	want := "date:Dated Work,schedule:Agent Work,schedule:Both Work,schedule:Human Work"
	if got := seen(m); got != want {
		t.Fatalf("entries = %s, want %s", got, want)
	}
}

func TestMeAgendaAudienceInvalid(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")

	cases := map[string]string{
		"unknown":  agendaWindow + "&audience=x",
		"repeated": agendaWindow + "&audience=human&audience=agent",
		"empty":    agendaWindow + "&audience=",
	}
	for name, query := range cases {
		t.Run("me/"+name, func(t *testing.T) {
			rr := meAgendaGet(t, w, w.owner, query)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d %s, want 400", rr.Code, rr.Body.String())
			}
			var body struct {
				Error struct {
					Code    string         `json:"code"`
					Message string         `json:"message"`
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != "validation_error" || body.Error.Details["field"] != "audience" {
				t.Fatalf("unexpected error body: %s", rr.Body.String())
			}
			if !strings.Contains(body.Error.Message, "`audience` must be human, agent or all") {
				t.Fatalf("unexpected error message: %q", body.Error.Message)
			}
		})
		t.Run("workspace/"+name, func(t *testing.T) {
			code, m := w.get(t, w.owner, query)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d %v, want 400", code, m)
			}
			errObj, _ := m["error"].(map[string]any)
			details, _ := errObj["details"].(map[string]any)
			if errObj["code"] != "validation_error" || details["field"] != "audience" {
				t.Fatalf("unexpected error body: %v", m)
			}
		})
	}
}

func TestWorkspaceAgendaAudience(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.srv.agendaClock = func() time.Time { return meAgendaNow }

	agentItem := w.item(t, "tasks", "WS Agent", `{"status":"open"}`, scheduleWithAudience("agent"))
	humanItem := w.item(t, "tasks", "WS Human", `{"status":"open"}`, scheduleWithAudience("human"))
	w.item(t, "tasks", "WS Both", `{"status":"open"}`, scheduleWithAudience("both"))
	w.item(t, "tasks", "WS Dated", `{"status":"open","due_date":"2031-03-10"}`, nil)
	for _, it := range []*models.Item{agentItem, humanItem} {
		meAgendaExec(t, w, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, w.owner.ID, it.ID)
		if err := w.srv.store.SetForecast(w.ws.ID, it.ID, w.owner.ID, "user", "2031-04-02", "estimate"); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("human", func(t *testing.T) {
		code, m := workspaceAgendaGetWithHeaders(t, w, w.owner, agendaWindow+"&recipient=me&audience=human", map[string]string{"X-Pad-Agent": "tern"})
		if code != http.StatusOK {
			t.Fatalf("status = %d %v", code, m)
		}
		if m["audience"] != "human" {
			t.Fatalf("audience = %v, want human", m["audience"])
		}
		want := "date:WS Dated,schedule:WS Both,schedule:WS Human"
		if got := seen(m); got != want {
			t.Fatalf("entries = %s, want %s", got, want)
		}
		for _, raw := range m["priority_list"].([]any) {
			row := raw.(map[string]any)
			if row["item_ref"] == agentItem.Ref {
				t.Fatalf("agent-only item leaked into human priority_list: %v", row)
			}
		}
	})

	t.Run("agent", func(t *testing.T) {
		m := w.ok(t, w.owner, agendaWindow+"&audience=agent")
		if m["audience"] != "agent" {
			t.Fatalf("audience = %v, want agent", m["audience"])
		}
		want := "date:WS Dated,schedule:WS Agent,schedule:WS Both"
		if got := seen(m); got != want {
			t.Fatalf("entries = %s, want %s", got, want)
		}
		fm := w.ok(t, w.owner, forecastApril+"&audience=agent")
		forecasts, _ := fm["forecasts"].([]any)
		if len(forecasts) != 1 || forecasts[0].(map[string]any)["item_ref"] != agentItem.Ref {
			t.Fatalf("agent forecasts = %v, want only %s", forecasts, agentItem.Ref)
		}
	})
}

func TestMeAgendaAudienceAllowlist(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	other := meAgendaWorkspace(t, w, "WorkspaceB")

	w.item(t, "tasks", "Allowed Human", `{"status":"open"}`, scheduleWithAudience("human"))
	other.item(t, "tasks", "Blocked Human", `{"status":"open"}`, scheduleWithAudience("human"))

	mod := func(ctx context.Context) context.Context {
		return WithTokenAllowedWorkspaces(ctx, []string{w.ws.Slug})
	}
	m := meAgendaOK(t, w, w.owner, agendaWindow+"&audience=human&include=", mod)
	if m["audience"] != "human" {
		t.Fatalf("audience = %v, want human", m["audience"])
	}
	if got := seen(m); got != "schedule:Allowed Human" {
		t.Fatalf("entries = %s, want schedule:Allowed Human", got)
	}
	for _, key := range []string{"priority_list", "waiting_on", "no_date"} {
		rows, _ := m[key].([]any)
		for _, raw := range rows {
			row := raw.(map[string]any)
			if row["workspace"] == other.ws.Slug {
				t.Fatalf("allowlist leaked %s row from workspace B: %v", key, row)
			}
		}
	}
}

func TestMeAgendaAudienceNextFromFiltered(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.srv.agendaClock = func() time.Time { return meAgendaNow }
	w.srv.agendaScanLimit = 3

	for i := 0; i < 4; i++ {
		sc := hourly()
		sc.Audience = "agent"
		w.item(t, "tasks", "Agent Hourly", `{"status":"open"}`, sc)
	}
	humanHourly := hourly()
	humanHourly.Audience = "human"
	w.item(t, "tasks", "Human Hourly", `{"status":"open"}`, humanHourly)

	m := meAgendaOK(t, w, w.owner, hourlyWindow+"&audience=human&include=")
	if n := len(entriesOf(m)); n != 100 || m["truncated"] != true {
		t.Fatalf("entries=%d truncated=%v; want 100 human entries and truncated", n, m["truncated"])
	}
	if got := m["next_from"]; got != "2031-03-14T04:00:00Z" {
		t.Fatalf("next_from = %v; want 2031-03-14T04:00:00Z derived from human rows only", got)
	}
	for _, e := range entriesOf(m) {
		if e["audience"] == "agent" {
			t.Fatalf("agent entry present in human response: %v", e)
		}
	}
}
