package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestUpdateComment_Permissions pins the comment-edit authorization model
// (TASK-1663 / PLAN-1662): only the comment author or a platform admin may
// edit; non-author non-admins are rejected; empty bodies are rejected.
func TestUpdateComment_Permissions(t *testing.T) {
	env := setupRBACEnv(t)

	// Owner (the bootstrap user) is the platform admin. Create an item.
	rr := doRequestWithCookie(env.srv, "POST",
		"/api/v1/workspaces/"+env.wsSlug+"/collections/docs/items",
		map[string]any{"title": "Commentable"}, env.ownerToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item map[string]any
	parseJSON(t, rr, &item)
	slug, _ := item["slug"].(string)

	// Editor posts a comment → user_id should be populated with the editor.
	rr = doRequestWithCookie(env.srv, "POST",
		"/api/v1/workspaces/"+env.wsSlug+"/items/"+slug+"/comments",
		map[string]any{"body": "original"}, env.editorToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("post comment: %d %s", rr.Code, rr.Body.String())
	}
	var c map[string]any
	parseJSON(t, rr, &c)
	cid, _ := c["id"].(string)
	if uid, _ := c["user_id"].(string); uid == "" {
		t.Error("expected user_id to be populated on create, got empty")
	}

	patch := func(token string, body string) *httptest.ResponseRecorder {
		return doRequestWithCookie(env.srv, "PATCH",
			"/api/v1/workspaces/"+env.wsSlug+"/comments/"+cid,
			map[string]any{"body": body}, token)
	}

	// 1. Author (editor) edits own comment → 200, body updated.
	rr = patch(env.editorToken, "edited by author")
	if rr.Code != http.StatusOK {
		t.Fatalf("author edit: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var up map[string]any
	parseJSON(t, rr, &up)
	if up["body"] != "edited by author" {
		t.Errorf("body not updated: %v", up["body"])
	}

	// 2. Viewer (non-author, non-admin) edits → 403.
	rr = patch(env.viewerToken, "hacked")
	if rr.Code != http.StatusForbidden {
		t.Errorf("non-author edit: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}

	// 3. Admin (owner) edits the editor's comment → 200.
	rr = patch(env.ownerToken, "edited by admin")
	if rr.Code != http.StatusOK {
		t.Errorf("admin edit: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// 4. Empty / whitespace body → 400 (delete is the way to remove).
	rr = patch(env.editorToken, "   ")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty body: expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestUpdateComment_NullUserIDIsAdminOnly pins the fallback: a comment with
// no recorded user_id (pre-identity / agent / imported) has no provable
// author, so only an admin can edit it.
func TestUpdateComment_NullUserIDIsAdminOnly(t *testing.T) {
	env := setupRBACEnv(t)
	wsID := workspaceIDForSlug(t, env.srv, env.wsSlug)

	// Create an item as owner, grab its id for a direct store insert.
	rr := doRequestWithCookie(env.srv, "POST",
		"/api/v1/workspaces/"+env.wsSlug+"/collections/docs/items",
		map[string]any{"title": "Legacy comments"}, env.ownerToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item map[string]any
	parseJSON(t, rr, &item)
	itemID, _ := item["id"].(string)

	// Insert a comment with NO user_id, mirroring a pre-TASK-1663 / agent row.
	comment, err := env.srv.store.CreateComment(wsID, itemID, "", models.CommentCreate{
		Body:      "legacy comment",
		Author:    "Editor",
		CreatedBy: "user",
		Source:    "web",
	})
	if err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	if comment.UserID != "" {
		t.Fatalf("expected empty user_id, got %q", comment.UserID)
	}

	path := "/api/v1/workspaces/" + env.wsSlug + "/comments/" + comment.ID

	// Editor (non-admin) cannot edit a comment with no provable author → 403.
	rr = doRequestWithCookie(env.srv, "PATCH", path,
		map[string]any{"body": "editor tries"}, env.editorToken)
	if rr.Code != http.StatusForbidden {
		t.Errorf("editor on null-user_id comment: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}

	// Admin can.
	rr = doRequestWithCookie(env.srv, "PATCH", path,
		map[string]any{"body": "admin fixes"}, env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Errorf("admin on null-user_id comment: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}
