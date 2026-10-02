package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3346: IDs taken from a request body must name something in the
// workspace the route authorized. Three places took them unchecked: a
// top-level comment's parent_id, an item's raw parent_id on create and
// update, and a personal token's workspace_id.

type bug3346Env struct {
	srv                 *Server
	ownerCookie         string
	editorCookie        string
	wsA, wsB            *models.Workspace
	itemA, itemA2       *models.Item
	itemB               *models.Item
	commentA, commentB  *models.Comment
	commentOnOtherItemA *models.Comment
}

func bug3346Setup(t *testing.T) *bug3346Env {
	t.Helper()
	srv := testServer(t)
	e := &bug3346Env{srv: srv}
	e.ownerCookie = bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	mkWS := func(name string) *models.Workspace {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": name}, e.ownerCookie)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, rr.Code, rr.Body.String())
		}
		var ws models.Workspace
		parseJSON(t, rr, &ws)
		return &ws
	}
	e.wsA, e.wsB = mkWS("Alpha"), mkWS("Bravo")
	mkItem := func(ws *models.Workspace, title string) *models.Item {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections/tasks/items", map[string]any{"title": title}, e.ownerCookie)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", title, rr.Code, rr.Body.String())
		}
		var it models.Item
		parseJSON(t, rr, &it)
		return &it
	}
	e.itemA, e.itemA2, e.itemB = mkItem(e.wsA, "A one"), mkItem(e.wsA, "A two"), mkItem(e.wsB, "B one")
	mkComment := func(ws *models.Workspace, it *models.Item, body string) *models.Comment {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/items/"+it.Slug+"/comments", map[string]any{"body": body}, e.ownerCookie)
		if rr.Code != http.StatusCreated {
			t.Fatalf("comment: %d %s", rr.Code, rr.Body.String())
		}
		var c models.Comment
		parseJSON(t, rr, &c)
		return &c
	}
	e.commentA = mkComment(e.wsA, e.itemA, "A's comment")
	e.commentOnOtherItemA = mkComment(e.wsA, e.itemA2, "comment on another A item")
	e.commentB = mkComment(e.wsB, e.itemB, "B's comment")

	editor, err := srv.store.CreateUser(models.UserCreate{Email: "editor@test.com", Name: "Editor", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(e.wsA.ID, editor.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if e.editorCookie, err = srv.store.CreateSession(editor.ID, "web-test", "192.0.2.1", "", webSessionTTL); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestBUG3346_CommentParentMustBeOnTheSameItem(t *testing.T) {
	e := bug3346Setup(t)
	post := func(parent string) int {
		rr := doRequestWithCookie(e.srv, "POST", "/api/v1/workspaces/"+e.wsA.Slug+"/items/"+e.itemA.Slug+"/comments",
			map[string]any{"body": "reply", "parent_id": parent}, e.editorCookie)
		return rr.Code
	}
	if code := post(e.commentB.ID); code != http.StatusNotFound {
		t.Errorf("parent in another workspace: %d, want 404", code)
	}
	if code := post(e.commentOnOtherItemA.ID); code != http.StatusNotFound {
		t.Errorf("parent on another item of the same workspace: %d, want 404", code)
	}
	// The foreign parent is answered exactly like one that does not exist.
	if code := post("00000000-0000-0000-0000-000000000000"); code != http.StatusNotFound {
		t.Errorf("nonexistent parent: %d, want 404", code)
	}
	if code := post(e.commentA.ID); code != http.StatusCreated {
		t.Errorf("control: parent on the same item: %d, want 201", code)
	}
	// B's comment gained no reply.
	var n int
	if err := e.srv.store.DB().QueryRow(`SELECT COUNT(*) FROM comments WHERE parent_id = ?`, e.commentB.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("B's comment has %d replies from workspace A", n)
	}
}

func TestBUG3346_RawItemParentMustBeInTheWorkspace(t *testing.T) {
	e := bug3346Setup(t)
	create := func(parent string) int {
		rr := doRequestWithCookie(e.srv, "POST", "/api/v1/workspaces/"+e.wsA.Slug+"/collections/tasks/items",
			map[string]any{"title": "child", "parent_id": parent}, e.editorCookie)
		return rr.Code
	}
	update := func(parent string) int {
		rr := doRequestWithCookie(e.srv, "PATCH", "/api/v1/workspaces/"+e.wsA.Slug+"/items/"+e.itemA2.Slug,
			map[string]any{"parent_id": parent}, e.editorCookie)
		return rr.Code
	}
	if code := create(e.itemB.ID); code != http.StatusBadRequest {
		t.Errorf("create with a parent in another workspace: %d, want 400", code)
	}
	if code := update(e.itemB.ID); code != http.StatusBadRequest {
		t.Errorf("update with a parent in another workspace: %d, want 400", code)
	}
	var n int
	if err := e.srv.store.DB().QueryRow(`SELECT COUNT(*) FROM items WHERE parent_id = ?`, e.itemB.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d item(s) in A point at B's item", n)
	}
	if code := create(e.itemA.ID); code != http.StatusCreated {
		t.Errorf("control: create with a parent in the same workspace: %d, want 201", code)
	}
	if code := update(e.itemA.ID); code != http.StatusOK {
		t.Errorf("control: update with a parent in the same workspace: %d, want 200", code)
	}
}

func TestBUG3346_TokenWorkspaceMustBeTheCallers(t *testing.T) {
	e := bug3346Setup(t)
	mint := func(wsID string) int {
		rr := doRequestWithCookie(e.srv, "POST", "/api/v1/auth/tokens", map[string]any{"name": "t", "workspace_id": wsID}, e.editorCookie)
		return rr.Code
	}
	if code := mint(e.wsB.ID); code != http.StatusNotFound {
		t.Errorf("token pinned to a workspace the caller is not in: %d, want 404", code)
	}
	if code := mint("00000000-0000-0000-0000-000000000000"); code != http.StatusNotFound {
		t.Errorf("token pinned to a nonexistent workspace: %d, want 404", code)
	}
	if code := mint(e.wsA.ID); code != http.StatusCreated {
		t.Errorf("control: token pinned to the caller's own workspace: %d, want 201", code)
	}
	var n int
	if err := e.srv.store.DB().QueryRow(`SELECT COUNT(*) FROM api_tokens WHERE workspace_id = ?`, e.wsB.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d token(s) pinned to workspace B by a non-member", n)
	}
}
