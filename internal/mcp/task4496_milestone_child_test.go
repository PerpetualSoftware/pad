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

func TestTASK4496_ScheduleChildParity(t *testing.T) {
	cases := []struct {
		action, verb, method string
		reason               string
		body                 map[string]any
	}{
		{"select", "select", "PUT", "", map[string]any{"kind": "child"}},
		{"waive", "waive", "PUT", "not needed", map[string]any{"kind": "waiver", "reason": "not needed"}},
		{"remove", "remove", "DELETE", "", nil},
	}
	for _, tc := range cases {
		in := map[string]any{"ref": "TASK-1", "workspace": "docapp", "child": "TASK-2", "child_action": tc.action}
		if tc.reason != "" {
			in["reason"] = tc.reason
		}
		res, disp := stdioCall(t, "schedule-child", in)
		if res.IsError || strings.Join(disp.gotPath, " ") != "item remind child "+tc.verb || !contains(disp.gotArgs, "TASK-1") || !contains(disp.gotArgs, "TASK-2") {
			t.Fatalf("stdio %s: %s %v %v", tc.action, textOf(res), disp.gotPath, disp.gotArgs)
		}
		if tc.reason != "" && !hasFlagPair(disp.gotArgs, "--reason", tc.reason) {
			t.Fatalf("stdio %s args: %v", tc.action, disp.gotArgs)
		}
		if tc.reason == "" && contains(disp.gotArgs, "--reason") {
			t.Fatalf("stdio %s sent a reason: %v", tc.action, disp.gotArgs)
		}
		res, seen := remoteCall(t, "schedule-child", in)
		if res.IsError || len(seen) != 1 || seen[0].Method != tc.method || seen[0].Path != "/api/v1/workspaces/docapp/items/TASK-1/commitment/children/TASK-2" {
			t.Fatalf("remote %s: %s %+v", tc.action, textOf(res), seen)
		}
		var b map[string]any
		if seen[0].Body != "" {
			if err := json.Unmarshal([]byte(seen[0].Body), &b); err != nil {
				t.Fatalf("remote %s body %q: %v", tc.action, seen[0].Body, err)
			}
		}
		if !reflect.DeepEqual(b, tc.body) {
			t.Fatalf("remote %s body = %v", tc.action, b)
		}
	}
}

func TestTASK4496_ScheduleChildRefusalParity(t *testing.T) {
	refused := []map[string]any{
		{"child_action": "select"},
		{"child_action": "select", "child": ""},
		{"child_action": "select", "child": 12},
		{"child": "TASK-2"},
		{"child": "TASK-2", "child_action": "drop"},
		{"child": "TASK-2", "child_action": true},
		{"child": "TASK-2", "child_action": "waive"},
		{"child": "TASK-2", "child_action": "waive", "reason": "   "},
		{"child": "TASK-2", "child_action": "waive", "reason": 5},
		{"child": "TASK-2", "child_action": "select", "reason": "x"},
		{"child": "TASK-2", "child_action": "remove", "reason": "x"},
	}
	for _, in := range refused {
		in = withWorkspace(in)
		in["ref"] = "TASK-1"
		a, disp := stdioCall(t, "schedule-child", in)
		b, seen := remoteCall(t, "schedule-child", in)
		if !a.IsError || !b.IsError || textOf(a) != textOf(b) || len(seen) != 0 || len(disp.gotPath) != 0 {
			t.Fatalf("refusal parity %v: %s / %s", in, textOf(a), textOf(b))
		}
		if !strings.Contains(textOf(a), "validation_failed") {
			t.Fatalf("refusal %v is not validation_failed: %s", in, textOf(a))
		}
	}
	in := map[string]any{"child": "TASK-2", "child_action": "select", "workspace": "docapp"}
	a, _ := stdioCall(t, "schedule-child", in)
	b, seen := remoteCall(t, "schedule-child", in)
	if !a.IsError || !b.IsError || textOf(a) != textOf(b) || len(seen) != 0 {
		t.Fatalf("missing ref: %s / %s", textOf(a), textOf(b))
	}
}

func TestTASK4496_ScheduleChildRemoteAgainstTheServer(t *testing.T) {
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)
	u, err := s.CreateUser(models.UserCreate{Email: "children@example.com", Name: "Alex", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Children", OwnerID: u.ID})
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
	it, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Milestone"})
	if err != nil {
		t.Fatal(err)
	}
	kid, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Child"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSchedule(ws.ID, it.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC"}); err != nil {
		t.Fatal(err)
	}
	disp := &HTTPHandlerDispatcher{Handler: http.HandlerFunc(srv.ServeHTTP), UserResolver: func(context.Context) *models.User { return u }}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState(ws.Slug), Dispatcher: disp}
	call := func(in map[string]any) (map[string]any, *CallToolResult) {
		t.Helper()
		in["ref"], in["workspace"] = it.Ref, ws.Slug
		res, err := scheduleItemAction(t, "schedule-child")(context.Background(), in, env)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(resultText(res)), &out)
		return out, res
	}
	milestone := func(out map[string]any) map[string]any { m, _ := out["milestone"].(map[string]any); return m }
	out, res := call(map[string]any{"child": kid.Ref, "child_action": "select"})
	if res.IsError || milestone(out)["total"] != float64(1) || milestone(out)["result"] != "pending" {
		t.Fatalf("select: %s", resultText(res))
	}
	out, res = call(map[string]any{"child": kid.Ref, "child_action": "waive", "reason": "not needed"})
	if res.IsError || milestone(out)["waivers"] != float64(1) || milestone(out)["result"] != "completed" {
		t.Fatalf("waive: %s", resultText(res))
	}
	out, res = call(map[string]any{"child": kid.Ref, "child_action": "remove"})
	if res.IsError || milestone(out)["total"] != float64(0) {
		t.Fatalf("remove: %s", resultText(res))
	}
	_, res = call(map[string]any{"child": "TASK-999", "child_action": "select"})
	if !res.IsError || !strings.Contains(resultText(res), "Item not found") {
		t.Fatalf("unknown child: %s", resultText(res))
	}
}

func TestTASK4496_ScheduleChildParamsDeclared(t *testing.T) {
	want := map[string]string{"child": "string", "child_action": "string", "reason": "string"}
	for _, p := range padItemSchemaParams {
		if typ, ok := want[p.Name]; ok {
			if p.Type != typ {
				t.Errorf("%s: %s", p.Name, p.Type)
			}
			if !strings.Contains(p.Description, "schedule-child") {
				t.Errorf("%s does not describe schedule-child: %q", p.Name, p.Description)
			}
			delete(want, p.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing params: %v", want)
	}
}
