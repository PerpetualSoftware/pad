package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3254: GET /workspaces/{ws} never set is_guest, so a guest (grants, no
// membership) who reached the workspace by URL (a fresh load or a deep link,
// which resolves through this endpoint rather than the list) got the member
// sidebar. The list endpoint has always set it; this is the other door.
func bug3254Guest(t *testing.T, srv *Server) (slug, token, memberToken string) {
	t.Helper()
	slug = createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	task := createItem(t, srv, slug, "tasks", map[string]any{"title": "Granted task"})
	granter, err := srv.store.CreateUser(models.UserCreate{Email: "granter-3254@example.com", Name: "Granter", Username: "granter-3254", Password: "pw-test-12345"})
	if err != nil {
		t.Fatalf("CreateUser granter: %v", err)
	}
	guest, err := srv.store.CreateUser(models.UserCreate{Email: "guest-3254@example.com", Name: "Guest", Username: "guest-3254", Password: "pw-test-12345"})
	if err != nil {
		t.Fatalf("CreateUser guest: %v", err)
	}
	if _, err := srv.store.CreateItemGrant(ws.ID, task.ID, guest.ID, "edit", granter.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	token, err = srv.store.CreateSession(guest.ID, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, granter.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	memberToken, err = srv.store.CreateSession(granter.ID, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession member: %v", err)
	}
	return slug, token, memberToken
}

func TestBUG3254_GetWorkspaceMarksAGuest(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, token, memberToken := bug3254Guest(t, srv)

		rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+slug, nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("guest GET workspace: %d %s", rr.Code, rr.Body.String())
		}
		var got models.Workspace
		parseJSON(t, rr, &got)
		if !got.IsGuest {
			t.Fatalf("guest GET /workspaces/%s: is_guest absent or false (body %s)", slug, rr.Body.String())
		}

		// A member reading the same workspace is not marked a guest.
		rr = doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+slug, nil, memberToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("member GET workspace: %d %s", rr.Code, rr.Body.String())
		}
		var member models.Workspace
		parseJSON(t, rr, &member)
		if member.IsGuest {
			t.Fatalf("member GET /workspaces/%s: is_guest true", slug)
		}
	})
}
