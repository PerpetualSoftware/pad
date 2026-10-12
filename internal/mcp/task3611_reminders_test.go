package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TASK-3611: pad_item `reminders` lists an item's reminders and `unremind`
// disarms one, over the remote transport against a real server. Permission is
// the server's, as for the CLI: a viewer may list but not disarm, and a
// read-scoped token likewise.
func TestTASK3611_RemindersListAndUnremindRemote(t *testing.T) {
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)
	mkUser := func(email string) *models.User {
		u, err := s.CreateUser(models.UserCreate{Email: email, Name: email, Password: "correct-horse-battery-staple"})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	owner, viewer := mkUser("rem-owner@example.com"), mkUser("rem-viewer@example.com")
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Rem WS", Slug: "rem-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, viewer.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Deferred", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}

	actions := map[string]ActionFn{}
	for _, d := range Catalog {
		if d.Name == "pad_item" {
			actions = d.Actions
		}
	}
	call := func(as *models.User, scopes, action string, in map[string]any) (string, bool) {
		t.Helper()
		disp := &HTTPHandlerDispatcher{Handler: http.HandlerFunc(srv.ServeHTTP), UserResolver: func(context.Context) *models.User { return as }}
		env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState(ws.Slug), Dispatcher: disp}
		ctx := context.Background()
		if scopes != "" {
			ctx = server.WithTokenScopes(ctx, scopes)
		}
		in["workspace"] = ws.Slug
		res, err := actions[action](ctx, in, env)
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		return resultText(res), res.IsError
	}
	list := func(as *models.User, scopes string) (ids []string, isErr bool) {
		t.Helper()
		text, isErr := call(as, scopes, "reminders", map[string]any{"ref": item.Ref})
		if isErr {
			return nil, true
		}
		var rows struct {
			Reminders []struct {
				ID string `json:"id"`
			} `json:"reminders"`
		}
		if err := json.Unmarshal([]byte(text), &rows); err != nil {
			t.Fatalf("reminders: not a list: %v\n%.400s", err, text)
		}
		for _, r := range rows.Reminders {
			ids = append(ids, r.ID)
		}
		return ids, false
	}

	text, isErr := call(owner, "", "remind", map[string]any{"ref": item.Ref, "remind_at": "2099-01-01T09:00:00Z"})
	if isErr {
		t.Fatalf("remind: %s", text)
	}
	var armed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &armed); err != nil || armed.ID == "" {
		t.Fatalf("remind answer has no id: %v\n%s", err, text)
	}

	if ids, isErr := list(owner, ""); isErr || len(ids) != 1 || ids[0] != armed.ID {
		t.Fatalf("owner list = %v (err=%v), want [%s]", ids, isErr, armed.ID)
	}
	// A viewer and a read token may look...
	if ids, isErr := list(viewer, ""); isErr || len(ids) != 1 {
		t.Fatalf("viewer list = %v (err=%v), want the one reminder", ids, isErr)
	}
	if ids, isErr := list(owner, `["pad:read"]`); isErr || len(ids) != 1 {
		t.Fatalf("read-token list = %v (err=%v), want the one reminder", ids, isErr)
	}
	// ...but not disarm, and the reminder survives both refusals.
	if text, isErr := call(viewer, "", "unremind", map[string]any{"reminder_id": armed.ID}); !isErr {
		t.Fatalf("a viewer must not disarm a reminder: %s", text)
	}
	if text, isErr := call(owner, `["pad:read"]`, "unremind", map[string]any{"reminder_id": armed.ID}); !isErr {
		t.Fatalf("a read token must not disarm a reminder: %s", text)
	}
	if ids, _ := list(owner, ""); len(ids) != 1 {
		t.Fatalf("after refused disarms, list = %v, want the reminder still there", ids)
	}

	if text, isErr := call(owner, "", "unremind", map[string]any{"reminder_id": armed.ID}); isErr {
		t.Fatalf("owner unremind: %s", text)
	}
	if ids, isErr := list(owner, ""); isErr || len(ids) != 0 {
		t.Fatalf("after unremind, list = %v (err=%v), want none", ids, isErr)
	}
	if text, isErr := call(owner, "", "unremind", map[string]any{"reminder_id": armed.ID}); !isErr || !strings.Contains(strings.ToLower(text), "not") {
		t.Fatalf("disarming a gone reminder must refuse: %s", text)
	}
}
