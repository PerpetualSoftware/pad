package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
)

func TestMCPHTTPAgendaAllWorkspacesHonoursTokenAllowList(t *testing.T) {
	srv, st := newPadServer(t)

	owner, err := st.CreateUser(models.UserCreate{
		Email:    "agenda-allowlist@example.com",
		Name:     "Agenda Owner",
		Password: "password123",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}

	disp := &HTTPHandlerDispatcher{
		Handler:      http.HandlerFunc(srv.ServeHTTP),
		UserResolver: fixedUserResolver(owner),
	}
	doc := liveCmdhelpDoc(t)

	makeWorkspaceWithSchedule := func(name, slug, title string) *models.Workspace {
		t.Helper()
		ws, err := st.CreateWorkspace(models.WorkspaceCreate{Name: name, Slug: slug, Template: "blank"})
		if err != nil {
			t.Fatalf("create workspace %s: %v", slug, err)
		}
		if err := st.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
			t.Fatalf("add owner to %s: %v", slug, err)
		}
		coll, err := st.CreateCollection(ws.ID, models.CollectionCreate{
			Name:   "Tasks",
			Slug:   "tasks",
			Prefix: "TASK",
			Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
		})
		if err != nil {
			t.Fatalf("create collection in %s: %v", slug, err)
		}
		item, err := st.CreateItem(ws.ID, coll.ID, models.ItemCreate{
			Title:  title,
			Fields: `{"status":"open"}`,
		})
		if err != nil {
			t.Fatalf("create item in %s: %v", slug, err)
		}
		env := ActionEnv{Doc: doc, Workspace: NewWorkspaceState(ws.Slug), Dispatcher: disp}
		res, err := scheduleItemAction(t, "remind")(context.Background(), map[string]any{
			"workspace": ws.Slug,
			"ref":       item.Ref,
			"start":     "2031-03-10T09:00",
			"tz":        "UTC",
			"audience":  "both",
		}, env)
		if err != nil {
			t.Fatalf("create schedule in %s: %v", slug, err)
		}
		if res.IsError {
			t.Fatalf("create schedule in %s refused: %s", slug, textOf(res))
		}
		return ws
	}

	const (
		titleA = "Alpha 2031-03-10 Schedule"
		titleB = "Beta 2031-03-10 Schedule"
	)
	wsA := makeWorkspaceWithSchedule("Agenda Workspace A", "agenda-ws-a", titleA)
	_ = makeWorkspaceWithSchedule("Agenda Workspace B", "agenda-ws-b", titleB)

	agendaEnv := ActionEnv{
		Doc:        doc,
		Workspace:  NewWorkspaceState(wsA.Slug),
		Dispatcher: disp,
	}
	agenda := func(ctx context.Context) string {
		t.Helper()
		res, err := scheduleItemAction(t, "agenda")(ctx, map[string]any{
			"from":           "2031-03-10T00:00:00Z",
			"to":             "2031-03-11T00:00:00Z",
			"tz":             "UTC",
			"all_workspaces": true,
			"audience":       "all",
		}, agendaEnv)
		if err != nil {
			t.Fatalf("pad_item.agenda: %v", err)
		}
		if res.IsError {
			t.Fatalf("pad_item.agenda refused: %s", textOf(res))
		}
		return textOf(res)
	}

	// Control: no allow-list sees both schedules.
	gotAll := agenda(context.Background())
	if !strings.Contains(gotAll, titleA) || !strings.Contains(gotAll, titleB) {
		t.Fatalf("no allow-list: want both %q and %q, got:\n%s", titleA, titleB, gotAll)
	}

	// Allow-list with only A sees A and not B.
	gotOnlyA := agenda(server.WithTokenAllowedWorkspaces(context.Background(), []string{wsA.Slug}))
	if !strings.Contains(gotOnlyA, titleA) {
		t.Errorf("allow-list [A]: missing %q in:\n%s", titleA, gotOnlyA)
	}
	if strings.Contains(gotOnlyA, titleB) {
		t.Errorf("allow-list [A]: leaked %q in:\n%s", titleB, gotOnlyA)
	}

	// Empty allow-list sees neither.
	gotNone := agenda(server.WithTokenAllowedWorkspaces(context.Background(), []string{}))
	if strings.Contains(gotNone, titleA) || strings.Contains(gotNone, titleB) {
		t.Errorf("allow-list []: leaked schedule in:\n%s", gotNone)
	}
}
