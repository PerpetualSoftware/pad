package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TASK-3413 (SPEC-6 U9c, ToolSurface v0.65): the MCP item projections an
// agent reads carry via_app / via_app_name for an app-created item: get,
// the list summary, and the history summary rows.
func TestTask3413_MCPProjectionsCarryViaApp(t *testing.T) {
	t.Parallel()
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)
	owner, err := s.CreateUser(models.UserCreate{Email: "owner-3413@example.com", Name: "Owner", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Via WS", Slug: "via-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TASK",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "From the app", Content: "body"})
	if err != nil {
		t.Fatal(err)
	}
	db := s.DB()
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO app_installs (id, workspace_id, origin, created_at, updated_at) VALUES ('inst-3413', ?, 'https://portal.example', ?, ?)`, ws.ID, ts, ts); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	bot, err := s.CreateAppUserTx(tx, "inst-3413", "Portal")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE app_installs SET bot_user_id = '` + bot.ID + `' WHERE id = 'inst-3413'`,
		`UPDATE items SET created_via_app = 'inst-3413' WHERE id = '` + item.ID + `'`,
		`UPDATE item_versions SET via_app = 'inst-3413' WHERE item_id = '` + item.ID + `'`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	d := &HTTPHandlerDispatcher{Handler: srv, UserResolver: func(context.Context) *models.User { return owner }}
	run := func(cmd []string, input map[string]any) string {
		t.Helper()
		res, err := d.Dispatch(WithDispatchInput(context.Background(), input), cmd, nil)
		if err != nil || res.IsError {
			t.Fatalf("%v: %v %s", cmd, err, textOf(res))
		}
		return textOf(res)
	}
	want := func(where string, m map[string]any) {
		t.Helper()
		if m["via_app"] != "inst-3413" || m["via_app_name"] != "Portal" {
			t.Errorf("%s: via_app=%v via_app_name=%v", where, m["via_app"], m["via_app_name"])
		}
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(run([]string{"item", "show"}, map[string]any{"workspace": "via-ws", "ref": "TASK-1"})), &got); err != nil {
		t.Fatal(err)
	}
	want("get", got)

	var list []map[string]any
	if err := json.Unmarshal([]byte(run([]string{"item", "list"}, map[string]any{"workspace": "via-ws", "collection": "tasks"})), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list: %d rows", len(list))
	}
	want("list summary", list[0])

	var hist []map[string]any
	if err := json.Unmarshal([]byte(run([]string{"item", "history"}, map[string]any{"workspace": "via-ws", "ref": "TASK-1"})), &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist) == 0 {
		t.Fatal("history: no rows")
	}
	for _, r := range hist {
		want("history summary", r)
	}
}
