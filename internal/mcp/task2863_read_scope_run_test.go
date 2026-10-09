package mcp

import (
	"context"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
)

// TASK-2863: over remote /mcp, pad_playbook.run with a pad:read grant now
// reaches the run handler, and a ref that would make the dispatcher's
// unescaped path route anywhere else is still refused for scope. Driven
// through the real catalog action and HTTPHandlerDispatcher, so the
// path the scope check sees is the one the dispatcher builds (CONVE-19).
func TestTASK2863_ReadGrantRunsAPlaybookOverHTTP(t *testing.T) {
	doc := liveCmdhelpDoc(t)
	var def ToolDef
	for _, d := range Catalog {
		if d.Name == "pad_playbook" {
			def = d
		}
	}
	run := def.Actions["run"]
	if run == nil {
		t.Fatal("pad_playbook has no run action")
	}

	call := func(ref string) (denied bool, got []string) {
		disp := &HTTPHandlerDispatcher{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = append(got, r.Method+" "+r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}),
			UserResolver: func(context.Context) *models.User {
				return &models.User{ID: "u-1", Email: "u@example.com", Name: "U"}
			},
			OnScopeDenied: func(string, string) { denied = true },
		}
		env := ActionEnv{Doc: doc, Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
		ctx := server.WithTokenScopes(context.Background(), `["pad:read"]`)
		_, _ = run(ctx, map[string]any{"workspace": "docapp", "ref": ref}, env)
		return denied, got
	}

	denied, got := call("ship")
	if denied || len(got) != 1 || got[0] != "POST /api/v1/workspaces/docapp/playbooks/ship/run" {
		t.Fatalf("pad:read run of ship: denied=%v, handler saw %q; want the run POST", denied, got)
	}
	for _, ref := range []string{"match?", "match#", "../items"} {
		denied, got := call(ref)
		if !denied || len(got) != 0 {
			t.Errorf("pad:read run of %q: denied=%v, handler saw %q; want refused for scope", ref, denied, got)
		}
	}
}
