package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

func TestTASK4412_GraphEndToEnd(t *testing.T) {
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)
	owner, err := s.CreateUser(models.UserCreate{Email: "links@example.com", Name: "Owner", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Links", Slug: "links", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TASK", Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Parent", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Child", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState(ws.Slug), Dispatcher: &HTTPHandlerDispatcher{Handler: http.HandlerFunc(srv.ServeHTTP), UserResolver: func(context.Context) *models.User { return owner }}}
	call := func(action string, in map[string]any) map[string]any {
		t.Helper()
		in["workspace"] = ws.Slug
		res, err := scheduleItemAction(t, action)(context.Background(), in, env)
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %+v %s", action, err, res, resultText(res))
		}
		out := map[string]any{}
		_ = json.Unmarshal([]byte(resultText(res)), &out)
		return out
	}
	base := time.Now().UTC().AddDate(0, 0, 3).Truncate(time.Second)
	for _, item := range []*models.Item{parent, child} {
		call("remind", map[string]any{"ref": item.Ref, "start": base.Format("2006-01-02T15:04:05"), "tz": "UTC"})
	}
	link := map[string]any{"ref": parent.Ref, "child": child.Ref, "parent_role": "target", "child_role": "target", "link_offset": map[string]any{"mode": "elapsed", "elapsed_secs": 7200}}
	p := call("schedule-link", link)
	if p["link"] != nil {
		t.Fatal("preview created link")
	}
	link["apply"], link["revision"] = true, p["moved"].(map[string]any)["revision"]
	created := call("schedule-link", link)["link"].(map[string]any)
	view := call("schedule", map[string]any{"ref": parent.Ref})
	if len(view["links"].([]any)) != 1 {
		t.Fatalf("links=%v", view)
	}
	to := base.Add(24 * time.Hour)
	move := map[string]any{"ref": parent.Ref, "role": "target", "to": to.Format(time.RFC3339)}
	p = call("schedule-move", move)
	if len(p["changes"].([]any)) != 2 {
		t.Fatalf("cascade=%v", p)
	}
	before := call("schedule", map[string]any{"ref": child.Ref})["roles"].(map[string]any)["target"]
	if before != base.Add(2*time.Hour).Format(time.RFC3339) {
		t.Fatalf("link offset=%v", before)
	}
	move["apply"], move["revision"] = true, p["revision"]
	call("schedule-move", move)
	view = call("schedule", map[string]any{"ref": child.Ref})
	if got := view["roles"].(map[string]any)["target"]; got != to.Add(2*time.Hour).Format(time.RFC3339) {
		t.Fatalf("child=%v", view)
	}
	call("schedule-unlink", map[string]any{"link_id": created["id"]})
	view = call("schedule", map[string]any{"ref": parent.Ref})
	if len(view["links"].([]any)) != 0 {
		t.Fatal("link remains")
	}
}

func TestTASK4412_GraphBodyErrorParity(t *testing.T) {
	for _, code := range []string{"invalid_body", "validation_error"} {
		body := fmt.Sprintf(`{"error":{"code":%q,"message":"Invalid offset","details":{"field":"offset","code":"combination"}}}`, code)
		for _, res := range []*CallToolResult{
			classifyHTTPStatusKind(context.Background(), "item remind link add", "/api/v1/workspaces/test/items/TASK-5/commitment/links", 400, []byte(body), nil, ResourceItem, "TASK-5"),
			classifyExecError(context.Background(), []string{"item", "remind", "link", "add"}, errors.New("exit status 1"), structuredErrorMarker+body, nil),
		} {
			env, ok := res.StructuredContent.(ErrorEnvelope)
			if !ok || string(env.Error.Code) != code {
				t.Errorf("%s: %+v", code, res)
			}
		}
	}
}

func TestTASK4412_InvalidBodyRoutes(t *testing.T) {
	for _, tc := range []struct {
		route string
		graph bool
	}{
		{"/api/v1/workspaces/test/items/TASK-5/commitment", true},
		{"/api/v1/workspaces/test/items/TASK-5/commitment/resolve", true},
		{"/api/v1/workspaces/test/items/TASK-5/commitment/renegotiate", true},
		{"/api/v1/workspaces/test/items/TASK-5/commitment/links", true},
		{"/api/v1/workspaces/test/items/TASK-5/commitment/move-preview", true},
		{"/api/v1/workspaces/test/items/TASK-5/commitment/move", true},
		{"/api/v1/workspaces/test/items/TASK-5/commitment/gates", true},
		{"/api/v1/workspaces/test/commitment-links/link-id", true},
		{"/api/v1/workspaces/test/commitment-gates/gate-id", true},
		{"/api/v1/workspaces/test/items/TASK-5/move", false},
		{"/api/v1/workspaces/test/items/TASK-5/copy", false},
		{"/api/v1/workspaces/test/items/TASK-5/copy/resolve", false},
		{"/api/v1/workspaces/test/items/TASK-5/reminders", false},
		{"/api/v1/workspaces/test/items/TASK-5/verifications", false},
		{"/api/v1/workspaces/test/items/import", false},
		// A resource name or query value is not a commitment route.
		{"/api/v1/workspaces/commitment/items/commitment/move", false},
		{"/api/v1/workspaces/test/items/TASK-5/commitment-notes", false},
		{"/api/v1/workspaces/test/items/TASK-5/move?next=/commitment", false},
	} {
		t.Run(tc.route, func(t *testing.T) {
			const body = `{"error":{"code":"invalid_body","message":"Invalid JSON body"}}`
			res := classifyHTTPStatusKind(context.Background(), "item", tc.route, 400, []byte(body), nil, ResourceItem, "TASK-5")
			want := ErrorPayload{Code: ErrValidationFailed, Message: "Validation failed.", Hint: "Backend: Invalid JSON body Route: " + tc.route + ". Adjust the input shape and retry."}
			if tc.graph {
				want = ErrorPayload{Code: ErrInvalidBody, Message: "Invalid JSON body"}
			}
			gotJSON, _ := json.Marshal(res.StructuredContent)
			wantJSON, _ := json.Marshal(ErrorEnvelope{Error: want})
			if !res.IsError || string(gotJSON) != string(wantJSON) {
				t.Errorf("got %s; want %s", gotJSON, wantJSON)
			}
		})
	}
}
