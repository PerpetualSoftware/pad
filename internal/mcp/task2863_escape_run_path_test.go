package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TASK-2863 follow-up: mapPlaybookRun was the one catalog mapper that
// built its path from unescaped input, so a ref of "match?" routed
// pad_playbook.run to POST /playbooks/match. It now escapes both
// segments, so that ref reaches the run route as "match%3F" and is
// answered as a playbook that does not exist. Driven through the real
// catalog action, the real dispatcher and the real server.
func TestTASK2863_RunPathSegmentsAreEscaped(t *testing.T) {
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)
	owner, err := s.CreateUser(models.UserCreate{
		Email: "run-owner@example.com", Name: "Run Owner", Password: "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Run WS", Slug: "run-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatal(err)
	}

	var paths []string
	disp := &HTTPHandlerDispatcher{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.Method+" "+r.URL.EscapedPath())
			srv.ServeHTTP(w, r)
		}),
		UserResolver: func(context.Context) *models.User { return owner },
	}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState(ws.Slug), Dispatcher: disp}
	var run ActionFn
	for _, d := range Catalog {
		if d.Name == "pad_playbook" {
			run = d.Actions["run"]
		}
	}
	call := func(ref string) (string, string) {
		paths = nil
		ctx := server.WithTokenScopes(context.Background(), `["pad:read"]`)
		res, err := run(ctx, map[string]any{"workspace": ws.Slug, "ref": ref}, env)
		if err != nil {
			t.Fatalf("run %q: %v", ref, err)
		}
		return strings.Join(paths, ", "), resultText(res)
	}

	// Control: an ordinary ref still runs, through the same escaping.
	if got, text := call("ship"); got != "POST /api/v1/workspaces/run-ws/playbooks/ship/run" || strings.Contains(text, `"error"`) {
		t.Fatalf("run ship: paths %q, result %s", got, text)
	}

	// The read grant is refused for scope before any request is built, as
	// TASK-2863's plain-name guard requires...
	if got, text := call("match?"); got != "" || !strings.Contains(text, "permission_denied") {
		t.Errorf("read grant, ref match?: paths %q, result %s; want refused for scope", got, text)
	}

	// ...and with a write grant the escaped ref reaches the RUN route and
	// is not a playbook, where it used to reach POST /playbooks/match.
	paths = nil
	ctx := server.WithTokenScopes(context.Background(), `["pad:write"]`)
	res, err := run(ctx, map[string]any{"workspace": ws.Slug, "ref": "match?"}, env)
	if err != nil {
		t.Fatal(err)
	}
	got, text := strings.Join(paths, ", "), resultText(res)
	if got != "POST /api/v1/workspaces/run-ws/playbooks/match%3F/run" {
		t.Errorf("write grant, ref match?: paths %q; want the escaped run path", got)
	}
	if !strings.Contains(text, "not_found") || strings.Contains(text, "decision_provider") {
		t.Errorf("write grant, ref match?: result %s; want the run route's not-found", text)
	}
}
