package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestListWorkspaces_AdminScopedToMembership is the regression for BUG-982.
//
// Before the fix, handleListWorkspaces routed admin users through an
// unfiltered store query that returned every non-deleted workspace,
// regardless of membership. The admin's "workspace switcher" therefore
// showed workspaces they had no member row for, labeled as "shared with
// me" by the frontend even though they were not actually shared.
//
// After the fix, admins go through GetUserWorkspaces like every other
// authenticated user. Cross-tenant visibility is available via the
// admin panel routes (/api/v1/admin/...), not the personal switcher.
func TestListWorkspaces_AdminScopedToMembership(t *testing.T) {
	srv := testServer(t)

	// 1. Bootstrap an admin (the first user is auto-admin).
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	// 2. Register a regular user via admin-driven signup.
	rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "alice@test.com",
		"name":     "Alice",
		"password": "correct-horse-battery-staple",
	}, adminToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("register alice: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// 3. Alice logs in and creates her own workspace. The handler
	// auto-adds her as the owner member, so she sees it; nobody else
	// should.
	aliceToken := loginUser(t, srv, "alice@test.com", "correct-horse-battery-staple")
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{
		"name": "Alice's Private Workspace",
	}, aliceToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create alice workspace: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var aliceWS models.Workspace
	parseJSON(t, rr, &aliceWS)

	// 4. Admin lists workspaces. Pre-fix: Alice's workspace appears.
	// Post-fix: it does not, because the admin is not a member.
	rr = doRequestWithCookie(srv, "GET", "/api/v1/workspaces", nil, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin list workspaces: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var adminWorkspaces []models.Workspace
	parseJSON(t, rr, &adminWorkspaces)
	for _, ws := range adminWorkspaces {
		if ws.ID == aliceWS.ID {
			t.Fatalf("BUG-982 regression: admin saw Alice's workspace %q in personal listing without being a member", ws.Slug)
		}
	}

	// 5. Sanity: Alice DOES see her workspace.
	rr = doRequestWithCookie(srv, "GET", "/api/v1/workspaces", nil, aliceToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("alice list workspaces: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var aliceWorkspaces []models.Workspace
	parseJSON(t, rr, &aliceWorkspaces)
	if !containsWorkspace(aliceWorkspaces, aliceWS.ID) {
		t.Fatal("owner should see their own workspace in personal listing")
	}

	// 6. Add the admin as an editor member of Alice's workspace —
	// now the admin SHOULD see it (they are an actual member).
	adminUser, err := srv.store.GetUserByEmail("admin@test.com")
	if err != nil || adminUser == nil {
		t.Fatal("failed to find admin user")
	}
	if err := srv.store.AddWorkspaceMember(aliceWS.ID, adminUser.ID, "editor"); err != nil {
		t.Fatalf("add admin as editor: %v", err)
	}
	rr = doRequestWithCookie(srv, "GET", "/api/v1/workspaces", nil, adminToken)
	parseJSON(t, rr, &adminWorkspaces)
	if !containsWorkspace(adminWorkspaces, aliceWS.ID) {
		t.Fatal("admin should see workspaces they are an explicit member of")
	}
}

// TestListWorkspaces_RegularUserScopedToMembership confirms the
// pre-existing behavior for non-admin users is unchanged: they only see
// workspaces they own or have been added to.
func TestListWorkspaces_RegularUserScopedToMembership(t *testing.T) {
	srv := testServer(t)

	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	// Admin creates their own workspace.
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{
		"name": "Admin's Private Workspace",
	}, adminToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create admin workspace: %d %s", rr.Code, rr.Body.String())
	}
	var adminWS models.Workspace
	parseJSON(t, rr, &adminWS)

	// Register Bob.
	rr = doRequestWithCookie(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "bob@test.com",
		"name":     "Bob",
		"password": "correct-horse-battery-staple",
	}, adminToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("register bob: %d %s", rr.Code, rr.Body.String())
	}

	bobToken := loginUser(t, srv, "bob@test.com", "correct-horse-battery-staple")

	// Bob has no workspaces.
	rr = doRequestWithCookie(srv, "GET", "/api/v1/workspaces", nil, bobToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("bob list: %d %s", rr.Code, rr.Body.String())
	}
	var bobWorkspaces []models.Workspace
	parseJSON(t, rr, &bobWorkspaces)
	if containsWorkspace(bobWorkspaces, adminWS.ID) {
		t.Fatal("bob (no membership) should not see admin's workspace")
	}
}

func containsWorkspace(list []models.Workspace, id string) bool {
	for _, ws := range list {
		if ws.ID == id {
			return true
		}
	}
	return false
}
