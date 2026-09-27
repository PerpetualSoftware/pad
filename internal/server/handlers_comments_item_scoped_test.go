package server

import (
	"net/http"
	"strings"
	"testing"
)

// TestItemScopedCommentWrites pins the item-scoped comment routes the agent
// doors use (TASK-2695): PATCH and DELETE /items/{ref}/comments/{id} refuse a
// comment that is not on {ref}, and otherwise behave exactly like the
// workspace-scoped routes they delegate to, ACL included.
func TestItemScopedCommentWrites(t *testing.T) {
	t.Parallel()
	env := setupRBACEnv(t)
	ws := "/api/v1/workspaces/" + env.wsSlug

	newItem := func(title string) string {
		t.Helper()
		rr := doRequestWithCookie(env.srv, "POST", ws+"/collections/docs/items",
			map[string]any{"title": title}, env.ownerToken)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
		}
		var item map[string]any
		parseJSON(t, rr, &item)
		return item["slug"].(string)
	}
	newComment := func(slug, body, token string) string {
		t.Helper()
		rr := doRequestWithCookie(env.srv, "POST", ws+"/items/"+slug+"/comments",
			map[string]any{"body": body}, token)
		if rr.Code != http.StatusCreated {
			t.Fatalf("post comment: %d %s", rr.Code, rr.Body.String())
		}
		var c map[string]any
		parseJSON(t, rr, &c)
		if c["edited"] != false {
			t.Fatalf("fresh comment: want edited=false, got %v", c["edited"])
		}
		return c["id"].(string)
	}
	listBodies := func(slug string) map[string]map[string]any {
		t.Helper()
		rr := doRequestWithCookie(env.srv, "GET", ws+"/items/"+slug+"/comments", nil, env.ownerToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("list comments: %d %s", rr.Code, rr.Body.String())
		}
		var list []map[string]any
		parseJSON(t, rr, &list)
		out := map[string]map[string]any{}
		for _, c := range list {
			out[c["id"].(string)] = c
		}
		return out
	}

	itemA := newItem("Scoped A")
	itemB := newItem("Scoped B")
	onA := newComment(itemA, "on A", env.editorToken)
	onB := newComment(itemB, "on B", env.editorToken)

	t.Run("edit through the right item", func(t *testing.T) {
		rr := doRequestWithCookie(env.srv, "PATCH", ws+"/items/"+itemA+"/comments/"+onA,
			map[string]any{"body": "on A, corrected"}, env.editorToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
		}
		got := listBodies(itemA)[onA]
		if got["body"] != "on A, corrected" {
			t.Fatalf("body not updated: %v", got["body"])
		}
	})

	t.Run("edit through the wrong item is refused and writes nothing", func(t *testing.T) {
		rr := doRequestWithCookie(env.srv, "PATCH", ws+"/items/"+itemA+"/comments/"+onB,
			map[string]any{"body": "hijacked"}, env.editorToken)
		if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "Comment not found") {
			t.Fatalf("want 404 Comment not found, got %d: %s", rr.Code, rr.Body.String())
		}
		if got := listBodies(itemB)[onB]["body"]; got != "on B" {
			t.Fatalf("comment on B changed through A: %v", got)
		}
	})

	t.Run("unknown item is an item 404", func(t *testing.T) {
		rr := doRequestWithCookie(env.srv, "PATCH", ws+"/items/no-such-item/comments/"+onA,
			map[string]any{"body": "x"}, env.editorToken)
		if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "Item not found") {
			t.Fatalf("want 404 Item not found, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("the edit ACL is the workspace route's", func(t *testing.T) {
		rr := doRequestWithCookie(env.srv, "PATCH", ws+"/items/"+itemA+"/comments/"+onA,
			map[string]any{"body": "not mine"}, env.viewerToken)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("non-author edit: want 403, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("delete through the wrong item is refused and deletes nothing", func(t *testing.T) {
		rr := doRequestWithCookie(env.srv, "DELETE", ws+"/items/"+itemA+"/comments/"+onB, nil, env.editorToken)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d: %s", rr.Code, rr.Body.String())
		}
		if _, ok := listBodies(itemB)[onB]; !ok {
			t.Fatal("comment on B was deleted through A")
		}
	})

	t.Run("the delete ACL is the workspace route's", func(t *testing.T) {
		rr := doRequestWithCookie(env.srv, "DELETE", ws+"/items/"+itemB+"/comments/"+onB, nil, env.viewerToken)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("viewer delete: want 403, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("delete through the right item", func(t *testing.T) {
		rr := doRequestWithCookie(env.srv, "DELETE", ws+"/items/"+itemB+"/comments/"+onB, nil, env.editorToken)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("want 204, got %d: %s", rr.Code, rr.Body.String())
		}
		if _, ok := listBodies(itemB)[onB]; ok {
			t.Fatal("comment still listed after delete")
		}
	})
}

func TestServerCapabilitiesAdvertisesItemScopedCommentWrites(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/server/capabilities", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("capabilities: %d %s", rr.Code, rr.Body.String())
	}
	var caps map[string]any
	parseJSON(t, rr, &caps)
	if caps["item_scoped_comment_writes"] != true {
		t.Fatalf("want item_scoped_comment_writes=true, got %v", caps["item_scoped_comment_writes"])
	}
}
