package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3341: claiming or releasing an item's execution lease is a write and
// takes edit permission. The handlers checked only visibility, so a viewer,
// or a guest holding a view grant, could claim an item (blocking others from
// working it) or release someone else's lease.
func TestBUG3341_LeaseRequiresEditPermission(t *testing.T) {
	srv := testServer(t)
	ownerCookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Leases"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	owner, _ := srv.store.GetUserByEmail("owner@test.com")
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections/tasks/items", map[string]any{"title": "Leased"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	user := func(email, role string) string {
		u, err := srv.store.CreateUser(models.UserCreate{Email: email, Name: email, Password: "correct-horse-battery-staple"})
		if err != nil {
			t.Fatal(err)
		}
		if role != "" {
			if err := srv.store.AddWorkspaceMember(ws.ID, u.ID, role); err != nil {
				t.Fatal(err)
			}
		}
		if role == "" {
			perm := "view"
			if email == "guest-edit@test.com" {
				perm = "edit"
			}
			if _, err := srv.store.CreateItemGrant(ws.ID, item.ID, u.ID, perm, owner.ID); err != nil {
				t.Fatal(err)
			}
		}
		c, err := srv.store.CreateSession(u.ID, "web-test", "192.0.2.1", "", webSessionTTL)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	viewer := user("viewer@test.com", "viewer")
	editor := user("editor@test.com", "editor")
	guestView := user("guest-view@test.com", "")
	guestEdit := user("guest-edit@test.com", "")

	call := func(cookie, action string) int {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/items/"+item.Slug+"/"+action, map[string]any{}, cookie)
		return rr.Code
	}
	for name, c := range map[string]string{"viewer": viewer, "view-grant guest": guestView} {
		if code := call(c, "claim"); code != http.StatusForbidden {
			t.Errorf("%s claim: %d, want 403", name, code)
		}
		if code := call(c, "release"); code != http.StatusForbidden {
			t.Errorf("%s release: %d, want 403", name, code)
		}
	}
	if code := call(editor, "claim"); code != http.StatusOK {
		t.Errorf("control: editor claim: %d, want 200", code)
	}
	if code := call(editor, "release"); code != http.StatusOK {
		t.Errorf("control: editor release: %d, want 200", code)
	}
	if code := call(guestEdit, "claim"); code != http.StatusOK {
		t.Errorf("control: edit-grant guest claim: %d, want 200", code)
	}
}

// BUG-3341: the holder label alone used to authorize refresh and release, so
// another editor could end or extend a lease by naming its label. A lease is
// now bound to the user who claimed it; a lease from before the binding (no
// user recorded) still works on its label until a refresh binds it.
func TestBUG3341_LeaseBoundToItsClaimer(t *testing.T) {
	srv := testServer(t)
	ownerCookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Bound"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	mkItem := func(title string) *models.Item {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections/tasks/items", map[string]any{"title": title}, ownerCookie)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
		}
		var it models.Item
		parseJSON(t, rr, &it)
		return &it
	}
	editor := func(email string) string {
		u, err := srv.store.CreateUser(models.UserCreate{Email: email, Name: email, Password: "correct-horse-battery-staple"})
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.store.AddWorkspaceMember(ws.ID, u.ID, "editor"); err != nil {
			t.Fatal(err)
		}
		c, err := srv.store.CreateSession(u.ID, "web-test", "192.0.2.1", "", webSessionTTL)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	alice, bob := editor("alice@test.com"), editor("bob@test.com")
	call := func(cookie string, it *models.Item, action string) int {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/items/"+it.Slug+"/"+action, map[string]any{"holder": "runner"}, cookie)
		return rr.Code
	}

	bound := mkItem("Bound")
	if code := call(alice, bound, "claim"); code != http.StatusOK {
		t.Fatalf("alice claim: %d", code)
	}
	if code := call(bob, bound, "release"); code != http.StatusConflict {
		t.Errorf("bob releasing alice's lease by its label: %d, want 409", code)
	}
	if code := call(bob, bound, "claim"); code != http.StatusConflict {
		t.Errorf("bob refreshing alice's lease by its label: %d, want 409", code)
	}
	if code := call(alice, bound, "release"); code != http.StatusOK {
		t.Errorf("control: alice releases her own lease: %d, want 200", code)
	}

	// A lease from before the binding: no user recorded. Its label alone
	// still releases it, and a refresh binds it to the refresher.
	legacy := mkItem("Legacy")
	if _, err := srv.store.ClaimItemLease(legacy.ID, "runner", "", 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if code := call(bob, legacy, "claim"); code != http.StatusOK {
		t.Errorf("refreshing a pre-binding lease by its label: %d, want 200", code)
	}
	if code := call(alice, legacy, "release"); code != http.StatusConflict {
		t.Errorf("after bob's refresh bound it, alice releasing it: %d, want 409", code)
	}
	if code := call(bob, legacy, "release"); code != http.StatusOK {
		t.Errorf("bob releases the lease he bound: %d, want 200", code)
	}
}
