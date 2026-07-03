package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-1919 — canEditComment's admin bypass previously fired for
// bearer-borne callers (PAT/CLI/MCP) with no isBearerAuth gate, letting a
// bearer-authed platform admin edit any user's comment in a workspace
// where they're a member, contradicting the BUG-1616/1617 bearer-
// suppression intent (the same family as BUG-1917/BUG-1918). The fix
// gates the bypass on cookie-session auth only, mirroring
// handlers_collab.go's authorizeCollabAccess.
//
// commentBearerGateFixture seeds a platform admin and a separate
// non-admin author, both workspace members (editor role), plus one
// commentable item.
type commentBearerGateFixture struct {
	srv          *Server
	ws           *models.Workspace
	admin        *models.User
	itemSlug     string
	bearerToken  string
	sessionToken string
	authorToken  string // cookie session for the non-admin comment author
}

func (f *commentBearerGateFixture) bearerHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + f.bearerToken}
}

func newCommentBearerGateFixture(t *testing.T) *commentBearerGateFixture {
	t.Helper()
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	author, err := srv.store.CreateUser(models.UserCreate{
		Email: "author@example.com", Name: "Author", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create author: %v", err)
	}

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "CommentBearer", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, admin.ID, "editor"); err != nil {
		t.Fatalf("add admin member: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, author.ID, "editor"); err != nil {
		t.Fatalf("add author member: %v", err)
	}

	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK",
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
		Title: "Commentable", CreatedBy: "user", Source: "test",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	tok, err := srv.store.CreateAPIToken(admin.ID, models.APITokenCreate{
		Name: "admin-pat", WorkspaceID: ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	sessTok, err := srv.store.CreateSession(admin.ID, "web-test", "192.0.2.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession (admin): %v", err)
	}
	authorTok, err := srv.store.CreateSession(author.ID, "web-test", "192.0.2.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession (author): %v", err)
	}

	return &commentBearerGateFixture{
		srv: srv, ws: ws, admin: admin, itemSlug: item.Slug,
		bearerToken: tok.Token, sessionToken: sessTok, authorToken: authorTok,
	}
}

// TestUpdateComment_AdminBearer_DeniedOnOthersComment pins the core
// BUG-1919 fix: a bearer-authed platform admin can no longer edit another
// member's comment (403, same shape as any other non-author non-admin),
// while a cookie-session admin retains the existing web-UI affordance.
func TestUpdateComment_AdminBearer_DeniedOnOthersComment(t *testing.T) {
	f := newCommentBearerGateFixture(t)

	rr := doRequestWithCookie(f.srv, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/"+f.itemSlug+"/comments",
		map[string]any{"body": "original"}, f.authorToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("author create comment: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var c map[string]any
	parseJSON(t, rr, &c)
	cid, _ := c["id"].(string)

	path := "/api/v1/workspaces/" + f.ws.Slug + "/comments/" + cid

	// Bearer admin — bypass suppressed, not the author → 403.
	rr = doRequestWithHeaders(f.srv, "PATCH", path, map[string]any{"body": "hacked via bearer"}, f.bearerHeaders())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("bearer admin editing another user's comment: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}

	// Cookie admin — bypass fires, web-UI affordance preserved.
	rr = doRequestWithCookie(f.srv, "PATCH", path, map[string]any{"body": "edited via cookie"}, f.sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("cookie admin editing another user's comment: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestUpdateComment_AdminBearer_CanEditOwnComment confirms the author
// check is independent of the admin branch: a bearer-authed admin can
// still edit a comment they themselves authored.
func TestUpdateComment_AdminBearer_CanEditOwnComment(t *testing.T) {
	f := newCommentBearerGateFixture(t)

	rr := doRequestWithHeaders(f.srv, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/"+f.itemSlug+"/comments",
		map[string]any{"body": "admin's own comment"}, f.bearerHeaders())
	if rr.Code != http.StatusCreated {
		t.Fatalf("bearer admin create comment: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var c map[string]any
	parseJSON(t, rr, &c)
	cid, _ := c["id"].(string)

	rr = doRequestWithHeaders(f.srv, "PATCH",
		"/api/v1/workspaces/"+f.ws.Slug+"/comments/"+cid,
		map[string]any{"body": "edited by its bearer-authed author"}, f.bearerHeaders())
	if rr.Code != http.StatusOK {
		t.Fatalf("bearer admin editing own comment: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestUpdateComment_AdminBearer_NullUserIDDenied pins the intended
// behavior change called out in BUG-1919: an empty-user_id comment (no
// provable author, admin-only per the doc comment on canEditComment) is
// now also out of reach for a bearer-authed admin — only a cookie-session
// admin can still edit it.
func TestUpdateComment_AdminBearer_NullUserIDDenied(t *testing.T) {
	f := newCommentBearerGateFixture(t)

	item, err := f.srv.store.GetItemBySlug(f.ws.ID, f.itemSlug)
	if err != nil || item == nil {
		t.Fatalf("resolve item: %v", err)
	}
	comment, err := f.srv.store.CreateComment(f.ws.ID, item.ID, "", models.CommentCreate{
		Body:      "legacy comment",
		Author:    "Legacy",
		CreatedBy: "user",
		Source:    "web",
	})
	if err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	if comment.UserID != "" {
		t.Fatalf("expected empty user_id, got %q", comment.UserID)
	}

	path := "/api/v1/workspaces/" + f.ws.Slug + "/comments/" + comment.ID

	// Bearer admin — no longer admin-only-eligible over bearer auth → 403.
	rr := doRequestWithHeaders(f.srv, "PATCH", path, map[string]any{"body": "bearer admin tries"}, f.bearerHeaders())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("bearer admin on null-user_id comment: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}

	// Cookie admin — still the only one who can fix an unowned legacy comment.
	rr = doRequestWithCookie(f.srv, "PATCH", path, map[string]any{"body": "cookie admin fixes"}, f.sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("cookie admin on null-user_id comment: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}
