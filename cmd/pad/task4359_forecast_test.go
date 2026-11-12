package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
	"github.com/spf13/cobra"
)

const forecast4359 = `{"value":"2031-03-12","set_by":"u1","set_by_name":"Alex","set_by_kind":"agent","set_at":"2026-11-11T10:00:00Z","source":"hand estimate","passed":false,"history":[]}`
const forecastView4359 = `{"item_ref":"TASK-5","forecast":` + forecast4359 + `,"milestone":{"counted":1,"total":2,"waivers":1,"result":"pending","children":[]}}`

func TestTASK4359_ForecastSetShowClear(t *testing.T) {
	for _, prefix := range [][]string{{"remind"}, {"item", "remind"}} {
		out, _, err, calls := runSched(t, okReply(forecastView4359), "", append(prefix, "forecast", "set", "TASK-5", "2031-03-12", "--source", "hand estimate")...)
		if err != nil || !strings.Contains(out, "Forecast for TASK-5: 2031-03-12 (set by Alex, agent; source: hand estimate)") {
			t.Fatalf("set = %q %v", out, err)
		}
		if len(calls) != 1 || calls[0].method != "PUT" || calls[0].path != schedPath+"/forecast" {
			t.Fatalf("calls = %+v", calls)
		}
		var body map[string]any
		_ = json.Unmarshal([]byte(calls[0].body), &body)
		if body["value"] != "2031-03-12" || body["source"] != "hand estimate" {
			t.Fatalf("body = %v", body)
		}
	}
	out, _, err, _ := runSched(t, okReply(forecastView4359), "", "remind", "show", "TASK-5")
	for _, want := range []string{"Forecast:", "2031-03-12", "Alex, agent", "2026-11-11T10:00:00Z", "hand estimate", "Children:", "1 of 2 counted, 1 waivers, result pending"} {
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("show missing %s: %q %v", want, out, err)
		}
	}
	out, _, err, calls := runSched(t, okReply(`{"forecast":null}`), "", "remind", "forecast", "clear", "TASK-5")
	if err != nil || strings.TrimSpace(out) != "Forecast cleared for TASK-5" || len(calls) != 1 || calls[0].method != "DELETE" {
		t.Fatalf("clear: %q %v %+v", out, err, calls)
	}
	_, _, err, calls = runSched(t, okReply(forecastView4359), "", "remind", "forecast", "set", "TASK-5", "2031-03-12T10:00:00+02:00")
	if err != nil || len(calls) != 1 || !strings.Contains(calls[0].body, "2031-03-12T10:00:00+02:00") || strings.Contains(calls[0].body, "source") {
		t.Fatalf("unchanged instant/source omission: %v %+v", err, calls)
	}
}

func TestTASK4359_ForecastAgentProvenance(t *testing.T) {
	t.Run("real server", func(t *testing.T) {
		root, _, it, _, u, _ := forecastCLI4359(t, "forecast-test")
		out, _, err := tzRun(t, root, "--format", "json", "remind", "forecast", "set", it.Ref, "2031-03-12", "--source", "real estimate")
		var body map[string]any
		_ = json.Unmarshal([]byte(out), &body)
		f, _ := body["forecast"].(map[string]any)
		if err != nil || f["set_by"] != u.ID || f["set_by_kind"] != "agent" || f["source"] != "real estimate" {
			t.Fatalf("real provenance: %q %v", out, err)
		}
	})
	out, _, err, _ := runSched(t, okReply(forecastView4359), "", "--format", "json", "remind", "forecast", "set", "TASK-5", "2031-03-12", "--source", "hand estimate")
	var body map[string]any
	_ = json.Unmarshal([]byte(out), &body)
	f, _ := body["forecast"].(map[string]any)
	if err != nil || f["set_by"] != "u1" || f["set_by_kind"] != "agent" || f["source"] != "hand estimate" {
		t.Fatalf("provenance: %q %v", out, err)
	}
}

func TestTASK4359_AgendaShowsForecastFlag(t *testing.T) {
	body := `{"window_complete":true,"entries":[{"type":"schedule","at":"2031-03-10T09:00:00Z","day":"2031-03-10","item_ref":"TASK-5","state":"outstanding","forecast":` + forecast4359 + `}]}`
	out, _, err, _ := runSched(t, okReply(body), "", "remind", "agenda", "--from", "2031-03-10")
	if err != nil || !strings.Contains(out, "forecast=2031-03-12") {
		t.Fatalf("agenda: %q %v", out, err)
	}
}

func TestTASK4359_ShowMilestoneSummary(t *testing.T) {
	t.Run("API selected child", func(t *testing.T) {
		root, h, it, kid, _, ws := forecastCLI4359(t, "")
		req := httptest.NewRequest("PUT", "/api/v1/workspaces/"+ws+"/items/"+it.Ref+"/commitment/children/"+kid.Ref, strings.NewReader(`{"kind":"waiver","reason":"not needed"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer pad_test")
		rr := httptest.NewRecorder()
		h(rr, req)
		if rr.Code != 200 {
			t.Fatalf("select child: %d %s", rr.Code, rr.Body.String())
		}
		out, _, err := tzRun(t, root, "remind", "show", it.Ref)
		if err != nil || !strings.Contains(out, "1 of 1 counted, 1 waivers, result completed") {
			t.Fatalf("real summary: %q %v", out, err)
		}
	})
	out, _, err, _ := runSched(t, okReply(strings.Replace(forecastView4359, `"pending"`, `"unknown"`, 1)), "", "remind", "show", "TASK-5")
	if err != nil || !strings.Contains(out, "unknown (includes items you cannot see)") {
		t.Fatalf("summary: %q %v", out, err)
	}
	out, _, err, _ = runSched(t, okReply(`{"milestone":{"total":0,"result":null}}`), "", "remind", "show", "TASK-5")
	if err != nil || strings.Contains(out, "Children:") {
		t.Fatalf("empty summary: %q %v", out, err)
	}
}

// Exercise the real write/read doors as well as the CLI wire formatting above.
//
// The CLI is built first because tzCLI writes time.Local, and the real
// server reads it from its own goroutines (background sweepers, and async
// work a request starts that outlives the request). Building the server after
// that write orders its reads behind it. Registering srv.Stop after tzCLI's
// restore makes it run first: Stop waits for the server's goroutines, so none
// is still reading when the zone is put back.
func forecastCLI4359(t *testing.T, agent string) (*cobra.Command, http.HandlerFunc, *models.Item, *models.Item, *models.User, string) {
	t.Helper()
	var handler http.HandlerFunc
	root := tzCLI(t, func(w http.ResponseWriter, r *http.Request) { handler(w, r) })
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)
	u, err := s.CreateUser(models.UserCreate{Email: "cli-forecast@example.com", Name: "Alex", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Test", Slug: "test", OwnerID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, u.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Prefix: "TASK", Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"],"default":"open"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.CreateItem(ws.ID, c.ID, models.ItemCreate{Title: "Milestone"})
	if err != nil {
		t.Fatal(err)
	}
	kid, err := s.CreateItem(ws.ID, c.ID, models.ItemCreate{Title: "Child"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSchedule(ws.ID, it.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC"}); err != nil {
		t.Fatal(err)
	}
	tok, err := s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "test"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	handler = func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+tok.Token)
		if agent != "" {
			r.Header.Set("X-Pad-Agent", agent)
		}
		srv.ServeHTTP(w, r.WithContext(server.WithCurrentUser(r.Context(), u)))
	}
	return root, handler, it, kid, u, ws.Slug
}
