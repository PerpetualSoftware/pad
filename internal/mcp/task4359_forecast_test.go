package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

func TestTASK4359_ForecastParity(t *testing.T) {
	for _, clear := range []bool{false, true} {
		in := map[string]any{"ref": "TASK-1", "workspace": "docapp"}
		verb, method := "set", "PUT"
		if clear {
			in["clear_forecast"] = true
			verb, method = "clear", "DELETE"
		} else {
			in["forecast"] = "2031-03-12"
			in["forecast_source"] = "hand estimate"
		}
		res, disp := stdioCall(t, "schedule-forecast", in)
		if res.IsError || strings.Join(disp.gotPath, " ") != "item remind forecast "+verb || !contains(disp.gotArgs, "TASK-1") {
			t.Fatalf("stdio: %s %v %v", textOf(res), disp.gotPath, disp.gotArgs)
		}
		if !clear && (!contains(disp.gotArgs, "2031-03-12") || !hasFlagPair(disp.gotArgs, "--source", "hand estimate")) {
			t.Fatalf("args: %v", disp.gotArgs)
		}
		res, seen := remoteCall(t, "schedule-forecast", in)
		if res.IsError || len(seen) != 1 || seen[0].Method != method || seen[0].Path != "/api/v1/workspaces/docapp/items/TASK-1/commitment/forecast" {
			t.Fatalf("remote: %s %+v", textOf(res), seen)
		}
		if !clear {
			var b map[string]any
			_ = json.Unmarshal([]byte(seen[0].Body), &b)
			if !reflect.DeepEqual(b, map[string]any{"value": "2031-03-12", "source": "hand estimate"}) {
				t.Fatalf("body = %v", b)
			}
		}
	}
	for _, in := range []map[string]any{{}, {"forecast": 12}, {"forecast": nil}, {"clear_forecast": "true"}, {"clear_forecast": false}, {"forecast": "2031-03-12", "clear_forecast": true}, {"forecast": "2031-03-12", "forecast_source": true}, {"clear_forecast": true, "forecast_source": "x"}} {
		in = withWorkspace(in)
		in["ref"] = "TASK-1"
		a, disp := stdioCall(t, "schedule-forecast", in)
		b, seen := remoteCall(t, "schedule-forecast", in)
		if !a.IsError || !b.IsError || textOf(a) != textOf(b) || len(seen) != 0 || len(disp.gotPath) != 0 {
			t.Fatalf("refusal parity %v: %s / %s", in, textOf(a), textOf(b))
		}
	}
}

func TestTASK4359_ForecastRemoteAgainstTheServer(t *testing.T) {
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)
	u, err := s.CreateUser(models.UserCreate{Email: "forecast@example.com", Name: "Alex", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Forecast", OwnerID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, u.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	col, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Prefix: "TASK"})
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Forecast"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSchedule(ws.ID, it.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC"}); err != nil {
		t.Fatal(err)
	}
	disp := &HTTPHandlerDispatcher{Handler: http.HandlerFunc(srv.ServeHTTP), UserResolver: func(context.Context) *models.User { return u }}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState(ws.Slug), Dispatcher: disp}
	call := func(action string, in map[string]any) map[string]any {
		t.Helper()
		in["ref"], in["workspace"] = it.Ref, ws.Slug
		res, err := scheduleItemAction(t, action)(context.Background(), in, env)
		if err != nil || res.IsError {
			t.Fatalf("call: %v %s", err, resultText(res))
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(resultText(res)), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := call("schedule-forecast", map[string]any{"forecast": "2031-03-12", "forecast_source": "hand estimate"})
	f, _ := out["forecast"].(map[string]any)
	if f["set_by"] != u.ID || f["set_by_name"] != u.Name || f["set_by_kind"] != "agent" || f["source"] != "hand estimate" {
		t.Fatalf("forecast = %v", f)
	}
	if read := call("schedule", map[string]any{}); !reflect.DeepEqual(read["forecast"], f) {
		t.Fatalf("schedule disagrees: %v", read)
	}
	if out := call("schedule-forecast", map[string]any{"clear_forecast": true}); out["forecast"] != nil {
		t.Fatalf("not cleared: %v", out)
	}
}

func TestTASK4359_ForecastParamsDeclared(t *testing.T) {
	want := map[string]string{"forecast": "string", "clear_forecast": "bool", "forecast_source": "string"}
	for _, p := range padItemSchemaParams {
		if typ, ok := want[p.Name]; ok {
			if p.Type != typ {
				t.Errorf("%s: %s", p.Name, p.Type)
			}
			delete(want, p.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing params: %v", want)
	}
}
