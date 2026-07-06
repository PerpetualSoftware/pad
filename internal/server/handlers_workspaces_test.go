package server

import (
	"net/http"
	"testing"
	"time"

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

// deletedWorkspaceEntry mirrors the GET /workspaces/deleted response
// shape (workspace fields + purge-window fields) for test assertions.
type deletedWorkspaceEntry struct {
	models.Workspace
	PurgeAt  string `json:"purge_at"`
	DaysLeft int    `json:"days_left"`
}

// setupOwnedDeletedWorkspace registers Alice + Bob, has Alice create and
// then soft-delete a workspace, and returns their tokens plus the
// workspace. Shared setup for the restore/deleted-list handler tests.
func setupOwnedDeletedWorkspace(t *testing.T, srv *Server) (aliceToken, bobToken string, ws models.Workspace) {
	t.Helper()
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	for _, u := range []struct{ email, name string }{
		{"alice@test.com", "Alice"},
		{"bob@test.com", "Bob"},
	} {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/register", map[string]string{
			"email":    u.email,
			"name":     u.name,
			"password": "correct-horse-battery-staple",
		}, adminToken)
		if rr.Code != http.StatusCreated {
			t.Fatalf("register %s: expected 201, got %d: %s", u.email, rr.Code, rr.Body.String())
		}
	}

	aliceToken = loginUser(t, srv, "alice@test.com", "correct-horse-battery-staple")
	bobToken = loginUser(t, srv, "bob@test.com", "correct-horse-battery-staple")

	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{
		"name": "Alice Recoverable",
	}, aliceToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	parseJSON(t, rr, &ws)

	// Alice (owner) soft-deletes it.
	rr = doRequestWithCookie(srv, "DELETE", "/api/v1/workspaces/"+ws.Slug, nil, aliceToken)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete workspace: expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
	return aliceToken, bobToken, ws
}

// TestRestoreWorkspace_OwnerFlow: the owner can restore their
// soft-deleted workspace (200 + restored, no deleted_at), and it
// re-appears in their workspace list.
func TestRestoreWorkspace_OwnerFlow(t *testing.T) {
	srv := testServer(t)
	aliceToken, _, ws := setupOwnedDeletedWorkspace(t, srv)

	// While deleted, it should not appear in the normal list.
	rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces", nil, aliceToken)
	var live []models.Workspace
	parseJSON(t, rr, &live)
	if containsWorkspace(live, ws.ID) {
		t.Fatal("soft-deleted workspace should not appear in the normal workspace list")
	}

	// Restore.
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/restore", nil, aliceToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("restore: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var restored models.Workspace
	parseJSON(t, rr, &restored)
	if restored.ID != ws.ID {
		t.Errorf("restored workspace ID mismatch: got %s want %s", restored.ID, ws.ID)
	}
	if restored.DeletedAt != nil {
		t.Errorf("restored workspace should have nil deleted_at, got %v", restored.DeletedAt)
	}

	// It's back in the list.
	rr = doRequestWithCookie(srv, "GET", "/api/v1/workspaces", nil, aliceToken)
	parseJSON(t, rr, &live)
	if !containsWorkspace(live, ws.ID) {
		t.Fatal("restored workspace should re-appear in the workspace list")
	}

	// Restoring again (now live) → 404.
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/restore", nil, aliceToken)
	if rr.Code != http.StatusNotFound {
		t.Errorf("double restore: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestRestoreWorkspace_NonOwnerForbidden: a non-owner who can see the
// endpoint gets 403, not a restore — owner-only authz.
func TestRestoreWorkspace_NonOwnerForbidden(t *testing.T) {
	srv := testServer(t)
	aliceToken, bobToken, ws := setupOwnedDeletedWorkspace(t, srv)

	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/restore", nil, bobToken)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-owner restore: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}

	// The workspace is still soft-deleted (Bob's forbidden call didn't
	// restore it): Alice can still restore it herself.
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/restore", nil, aliceToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner restore after forbidden attempt: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestRestoreWorkspace_LiveOrUnknown404: restoring a live or unknown
// workspace returns 404.
func TestRestoreWorkspace_LiveOrUnknown404(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{
		"name": "Still Live",
	}, adminToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)

	// Live workspace → nothing to restore → 404.
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/restore", nil, adminToken)
	if rr.Code != http.StatusNotFound {
		t.Errorf("restore live workspace: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	// Unknown slug → 404.
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/nope-nope-nope/restore", nil, adminToken)
	if rr.Code != http.StatusNotFound {
		t.Errorf("restore unknown workspace: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestRestoreWorkspace_NoZeroUsersBypass is the security regression for
// the Codex P1: when account deletion leaves zero live users, a
// soft-deleted (account-deleted) workspace must NOT be restorable by an
// unauthenticated caller who guesses its slug. There is no fresh-install
// UserCount==0 bypass on restore.
func TestRestoreWorkspace_NoZeroUsersBypass(t *testing.T) {
	srv := testServer(t)

	// Simulate the post-account-deletion state: a workspace whose owner is
	// gone (owner_id points at no live user) and zero users on the
	// instance. CreateWorkspace with a ghost owner_id + no registered users
	// reproduces it without driving the whole DeleteAccountAtomic path.
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{
		Name:    "Orphaned",
		OwnerID: "ghost-owner-id",
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}

	// Unauthenticated restore on a zero-user instance (RequireAuth is
	// bypassed on fresh install) must NOT be honored. The owner user is
	// gone, so it takes the owner-gone path → 404 (not restorable), which
	// also avoids leaking that the row exists.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/restore", nil)
	if rr.Code == http.StatusOK {
		t.Fatalf("zero-user unauthenticated restore must be refused, got 200: %s", rr.Body.String())
	}
	if rr.Code != http.StatusNotFound {
		t.Fatalf("zero-user unauthenticated restore: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	// And it stays soft-deleted.
	if got, err := srv.store.GetDeletedWorkspaceBySlug(ws.Slug); err != nil {
		t.Fatalf("GetDeletedWorkspaceBySlug: %v", err)
	} else if got == nil {
		t.Fatal("workspace must remain soft-deleted after a forbidden restore")
	}
}

// TestRestoreWorkspace_OwnerGone404NotProbeable is the Codex P2 fix for
// slug probing: when the owning user is gone (account deletion), a
// soft-deleted workspace must return 404 to a non-owner — not 403 —
// so a guessed slug can't confirm the account-deleted row still exists.
// A live-owned workspace still returns 403 to non-owners (see
// TestRestoreWorkspace_NonOwnerForbidden), preserving the owner-only
// contract.
func TestRestoreWorkspace_OwnerGone404NotProbeable(t *testing.T) {
	srv := testServer(t)
	// A real, authenticated caller exists (so RequireAuth is active and
	// this isn't the zero-user path).
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	// Account-deletion shape: a soft-deleted workspace whose owner_id
	// points at a user row that no longer exists.
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{
		Name:    "Account Deleted",
		OwnerID: "gone-owner-id",
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}

	// The admin (authenticated non-owner) probing the slug gets 404, not
	// 403 — no existence leak.
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/restore", nil, adminToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("owner-gone restore probe: expected 404 (no leak), got %d: %s", rr.Code, rr.Body.String())
	}

	// Still soft-deleted.
	if got, err := srv.store.GetDeletedWorkspaceBySlug(ws.Slug); err != nil {
		t.Fatalf("GetDeletedWorkspaceBySlug: %v", err)
	} else if got == nil {
		t.Fatal("workspace must remain soft-deleted")
	}
}

// TestRestoreWorkspace_PastHorizon404 pins the Codex P2 fix: a workspace
// soft-deleted longer ago than the purge retention window is eligible for
// hard-purge and hidden from the deleted-list, so restore must 404 on it
// too (even if the sweeper hasn't physically purged it yet) — restore and
// the list share one horizon.
func TestRestoreWorkspace_PastHorizon404(t *testing.T) {
	srv := testServer(t)
	aliceToken, _, ws := setupOwnedDeletedWorkspace(t, srv)

	// Backdate deleted_at to 31 days ago (past the 30-day horizon),
	// dialect-safely so this also holds under Postgres (make test-pg).
	past := time.Now().UTC().Add(-31 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(
		srv.store.D().Rebind("UPDATE workspaces SET deleted_at = ? WHERE id = ?"),
		past, ws.ID,
	); err != nil {
		t.Fatalf("backdate deleted_at: %v", err)
	}

	// It's hidden from the owner's deleted-list...
	rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/deleted", nil, aliceToken)
	var deleted []deletedWorkspaceEntry
	parseJSON(t, rr, &deleted)
	for _, e := range deleted {
		if e.ID == ws.ID {
			t.Fatal("past-horizon workspace must not appear in the deleted-list")
		}
	}

	// ...and restore 404s rather than resurrecting a purge-eligible row.
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/restore", nil, aliceToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("past-horizon restore: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestListDeletedWorkspaces_OwnerScoped: the deleted-list is scoped to
// the caller's owned workspaces and carries the purge-window fields.
func TestListDeletedWorkspaces_OwnerScoped(t *testing.T) {
	srv := testServer(t)
	aliceToken, bobToken, ws := setupOwnedDeletedWorkspace(t, srv)

	// Alice (owner) sees her deleted workspace with purge-window fields.
	rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/deleted", nil, aliceToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("alice deleted list: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var aliceDeleted []deletedWorkspaceEntry
	parseJSON(t, rr, &aliceDeleted)
	var found *deletedWorkspaceEntry
	for i := range aliceDeleted {
		if aliceDeleted[i].ID == ws.ID {
			found = &aliceDeleted[i]
		}
	}
	if found == nil {
		t.Fatalf("owner should see their deleted workspace, got %+v", aliceDeleted)
	}
	if found.DeletedAt == nil {
		t.Error("deleted-list entry should carry deleted_at")
	}
	if found.PurgeAt == "" {
		t.Error("deleted-list entry should carry purge_at")
	}
	// Just-deleted → close to the full 30-day window remaining.
	if found.DaysLeft < 29 || found.DaysLeft > 30 {
		t.Errorf("days_left for a just-deleted workspace should be ~30, got %d", found.DaysLeft)
	}

	// Bob (not owner) sees nothing.
	rr = doRequestWithCookie(srv, "GET", "/api/v1/workspaces/deleted", nil, bobToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("bob deleted list: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var bobDeleted []deletedWorkspaceEntry
	parseJSON(t, rr, &bobDeleted)
	for _, e := range bobDeleted {
		if e.ID == ws.ID {
			t.Fatal("non-owner must not see another user's deleted workspace")
		}
	}
}
