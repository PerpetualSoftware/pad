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

// TASK-3611: pad_item link-pr writes the PR metadata the agent supplies (no
// GitHub fetch) and unlink-pr clears it, over the remote transport against a
// real server. Permission and validation are the server's, as for `pad github
// link`: a viewer and a read token are refused, and so is a bad URL.
func TestTASK3611_LinkPRRemote(t *testing.T) {
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
	owner, viewer := mkUser("pr-owner@example.com"), mkUser("pr-viewer@example.com")
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "PR WS", Slug: "pr-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		u    *models.User
		role string
	}{{owner, "owner"}, {viewer, "viewer"}} {
		if err := s.AddWorkspaceMember(ws.ID, m.u.ID, m.role); err != nil {
			t.Fatal(err)
		}
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Ship it", Fields: `{"status":"open"}`})
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
	linkedNumber := func() int {
		t.Helper()
		got, err := s.GetItem(item.ID)
		if err != nil {
			t.Fatal(err)
		}
		var f map[string]any
		_ = json.Unmarshal([]byte(got.Fields), &f)
		pr, _ := f["github_pr"].(map[string]any)
		n, _ := pr["number"].(float64)
		return int(n)
	}
	link := map[string]any{"ref": item.Ref, "pr_number": float64(42), "pr_url": "https://github.com/acme/app/pull/42", "pr_state": "open", "pr_title": "Ship it", "pr_repo": "acme/app"}
	clone := func(m map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range m {
			out[k] = v
		}
		return out
	}

	if text, isErr := call(viewer, "", "link-pr", clone(link)); !isErr || linkedNumber() != 0 {
		t.Fatalf("a viewer must not link a PR: isErr=%v linked=%d %s", isErr, linkedNumber(), text)
	}
	if text, isErr := call(owner, `["pad:read"]`, "link-pr", clone(link)); !isErr || linkedNumber() != 0 {
		t.Fatalf("a read token must not link a PR: isErr=%v %s", isErr, text)
	}
	bad := clone(link)
	bad["pr_url"] = "not a url"
	if text, isErr := call(owner, "", "link-pr", bad); !isErr || linkedNumber() != 0 {
		t.Fatalf("a bad URL must be refused: isErr=%v %s", isErr, text)
	}

	if text, isErr := call(owner, "", "link-pr", clone(link)); isErr {
		t.Fatalf("owner link-pr: %s", text)
	}
	if n := linkedNumber(); n != 42 {
		t.Fatalf("after link-pr the item links PR %d, want 42", n)
	}
	// Linking again replaces the link, as the catalog says.
	again := clone(link)
	again["pr_number"], again["pr_url"] = float64(43), "https://github.com/acme/app/pull/43"
	if text, isErr := call(owner, "", "link-pr", again); isErr || linkedNumber() != 43 {
		t.Fatalf("re-link must replace: isErr=%v linked=%d %s", isErr, linkedNumber(), text)
	}
	// An unknown ref is the server's not-found on both actions; unlink's
	// read falls through to the PATCH, which resolves the item the same way.
	for _, a := range []string{"link-pr", "unlink-pr"} {
		in := map[string]any{"ref": "TASK-999"}
		if a == "link-pr" {
			in = clone(link)
			in["ref"] = "TASK-999"
		}
		if text, isErr := call(owner, "", a, in); !isErr || !strings.Contains(text, "not_found") {
			t.Fatalf("%s on an unknown ref must be not_found: %s", a, text)
		}
	}
	if text, isErr := call(viewer, "", "unlink-pr", map[string]any{"ref": item.Ref}); !isErr || linkedNumber() != 43 {
		t.Fatalf("a viewer must not unlink: isErr=%v %s", isErr, text)
	}
	if text, isErr := call(owner, "", "unlink-pr", map[string]any{"ref": item.Ref}); isErr {
		t.Fatalf("owner unlink-pr: %s", text)
	}
	if n := linkedNumber(); n != 0 {
		t.Fatalf("after unlink-pr the item still links PR %d", n)
	}
	// As `pad github unlink` does, an item with no link is refused.
	if text, isErr := call(owner, "", "unlink-pr", map[string]any{"ref": item.Ref}); !isErr {
		t.Fatalf("unlinking an item with no PR must refuse: %s", text)
	}
}
