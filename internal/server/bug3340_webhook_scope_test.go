package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/webhooks"
)

// BUG-3340: a webhook receives full item snapshots from every collection, so
// configuring one is a whole-workspace capability. A workspace owner whose
// access is restricted to specific collections must not be able to create or
// fire one (the same rule as full export, BUG-1922); deleting one stays open.
// And a soft-deleted workspace's webhooks must receive nothing.

func bug3340Setup(t *testing.T) (*Server, *models.Workspace, string, string) {
	t.Helper()
	srv := testServer(t)
	ownerCookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Hooks"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	tasks, _ := srv.store.GetCollectionBySlug(ws.ID, "tasks")

	co, err := srv.store.CreateUser(models.UserCreate{Email: "coowner@test.com", Name: "CoOwner", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, co.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, co.ID, "specific", []string{tasks.ID}); err != nil {
		t.Fatal(err)
	}
	coCookie, err := srv.store.CreateSession(co.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	return srv, &ws, ownerCookie, coCookie
}

func TestBUG3340_RestrictedOwnerCannotManageWebhooks(t *testing.T) {
	srv, ws, ownerCookie, coCookie := bug3340Setup(t)
	base := "/api/v1/workspaces/" + ws.Slug + "/webhooks"
	body := map[string]string{"url": "https://8.8.8.8/hook"}

	if rr := doRequestWithCookie(srv, "POST", base, body, coCookie); rr.Code != http.StatusForbidden {
		t.Errorf("restricted owner create: %d %s, want 403", rr.Code, rr.Body.String())
	}
	rr := doRequestWithCookie(srv, "POST", base, body, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("control: unrestricted owner create: %d %s, want 201", rr.Code, rr.Body.String())
	}
	var hook models.Webhook
	parseJSON(t, rr, &hook)
	if rr := doRequestWithCookie(srv, "POST", base+"/"+hook.ID+"/test", nil, coCookie); rr.Code != http.StatusForbidden {
		t.Errorf("restricted owner test-fire: %d %s, want 403", rr.Code, rr.Body.String())
	}
	// Removing a webhook is revocation and stays open to any owner.
	if rr := doRequestWithCookie(srv, "DELETE", base+"/"+hook.ID, nil, coCookie); rr.Code != http.StatusNoContent && rr.Code != http.StatusOK {
		t.Errorf("restricted owner delete: %d %s, want success", rr.Code, rr.Body.String())
	}
}

func TestBUG3340_DeletedWorkspaceWebhooksReceiveNothing(t *testing.T) {
	srv, ws, _, _ := bug3340Setup(t)
	if _, err := srv.store.CreateWebhook(ws.ID, models.WebhookCreate{URL: "https://8.8.8.8/hook"}); err != nil {
		t.Fatal(err)
	}
	d := webhooks.NewDispatcher(srv.store)
	matched := func() int {
		t.Helper()
		out, err := d.DeliverEvent(webhooks.Delivery{WorkspaceID: ws.ID, EventID: "e1", Event: "item.created", Payload: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		return out.Matched
	}
	if n := matched(); n != 1 {
		t.Fatalf("control: a live workspace's webhook should match, got %d", n)
	}
	if err := srv.store.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatal(err)
	}
	if n := matched(); n != 0 {
		t.Errorf("after the workspace was deleted, %d webhook(s) still matched an event", n)
	}
	if err := srv.store.RestoreWorkspace(ws.Slug); err != nil {
		t.Fatal(err)
	}
	if n := matched(); n != 1 {
		t.Errorf("after the workspace was restored, %d webhook(s) matched, want 1", n)
	}
}
