package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestSSESubscriberStillHasAccess verifies the per-tick membership
// revalidation helper returns the right answer across all supported
// access paths: admin, direct member, guest-with-grants, removed
// member, and legacy workspace-scoped token. A regression here would
// mean a silently-drifting SSE stream keeps feeding events to a user
// who lost access.
func TestSSESubscriberStillHasAccess(t *testing.T) {
	srv := testServer(t)

	// Setup: one admin, one workspace owned by admin, one regular user
	// added as editor, one outsider.
	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	member, err := srv.store.CreateUser(models.UserCreate{
		Email: "editor@example.com", Name: "Editor", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	outsider, err := srv.store.CreateUser(models.UserCreate{
		Email: "outsider@example.com", Name: "Outsider", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "TestWS"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}

	mkReq := func(u *models.User) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace="+ws.Slug, nil)
		if u != nil {
			req = req.WithContext(context.WithValue(req.Context(), ctxCurrentUser, u))
		}
		return req
	}

	// Admin: always true regardless of membership.
	if !srv.sseSubscriberStillHasAccess(mkReq(admin), ws.ID) {
		t.Fatal("admin should have access")
	}

	// Direct member: true.
	if !srv.sseSubscriberStillHasAccess(mkReq(member), ws.ID) {
		t.Fatal("active member should have access")
	}

	// Outsider (no membership, no grants): false.
	if srv.sseSubscriberStillHasAccess(mkReq(outsider), ws.ID) {
		t.Fatal("outsider should NOT have access")
	}

	// Remove the member. sseSubscriberStillHasAccess should now return
	// false on the same-shaped request — this is the core regression:
	// a live SSE connection must stop streaming when membership is
	// revoked.
	if err := srv.store.RemoveWorkspaceMember(ws.ID, member.ID); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	if srv.sseSubscriberStillHasAccess(mkReq(member), ws.ID) {
		t.Fatal("removed member should NOT have access after revocation")
	}

	// Re-add the outsider with a collection-level guest grant and
	// confirm the has-grants branch flips to true.
	// (Guest grants exercise a different code path than workspace_members.)
	tasksColl, err := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil || tasksColl == nil {
		t.Log("skipping guest-grant subtest: tasks collection not seeded in this test fixture")
	} else {
		_, err = srv.store.CreateCollectionGrant(ws.ID, tasksColl.ID, outsider.ID, "view", admin.ID)
		if err != nil {
			t.Fatalf("create guest grant: %v", err)
		}
		if !srv.sseSubscriberStillHasAccess(mkReq(outsider), ws.ID) {
			t.Fatal("user with active guest grant should have access")
		}
	}

	// Unauthenticated request: false (no user, no legacy token).
	if srv.sseSubscriberStillHasAccess(mkReq(nil), ws.ID) {
		t.Fatal("unauthenticated subscriber should NOT have access")
	}

	// Legacy workspace-scoped token targeting THIS workspace: true.
	reqWithToken := mkReq(nil)
	reqWithToken = reqWithToken.WithContext(context.WithValue(reqWithToken.Context(), ctxTokenWorkspaceID, ws.ID))
	if !srv.sseSubscriberStillHasAccess(reqWithToken, ws.ID) {
		t.Fatal("legacy token scoped to this workspace should have access")
	}

	// Legacy workspace-scoped token targeting a DIFFERENT workspace: false.
	reqWithOtherToken := mkReq(nil)
	reqWithOtherToken = reqWithOtherToken.WithContext(context.WithValue(reqWithOtherToken.Context(), ctxTokenWorkspaceID, "some-other-workspace-id"))
	if srv.sseSubscriberStillHasAccess(reqWithOtherToken, ws.ID) {
		t.Fatal("legacy token scoped to a different workspace should NOT have access")
	}
}

// TestComputeSSEVisibility_ReflectsCurrentGrants verifies the
// visibility-recompute path returns the current permission snapshot on
// every call. A regression would mean a mid-stream permission change
// (e.g. role downgrade from editor to guest, grants revoked) is
// ignored until the next reconnect — the exact leak Codex P1 flagged.
func TestComputeSSEVisibility_ReflectsCurrentGrants(t *testing.T) {
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "user@example.com", Name: "User", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "VisTest"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, user.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}

	mkReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace="+ws.Slug, nil)
		return req.WithContext(context.WithValue(req.Context(), ctxCurrentUser, user))
	}

	// Initial: full editor access → visibleSlugSet == nil (all access).
	v1 := srv.computeSSEVisibility(mkReq(), ws.ID)
	if v1.visibleSlugSet != nil {
		t.Fatalf("editor should have unrestricted access (nil visibleSlugSet); got %v", v1.visibleSlugSet)
	}
	if v1.isGuest {
		t.Fatal("editor member must not be marked as guest")
	}

	// Revoke membership → recompute must flip the snapshot. Without the
	// periodic recompute, the stream would keep using v1 forever.
	if err := srv.store.RemoveWorkspaceMember(ws.ID, user.ID); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	v2 := srv.computeSSEVisibility(mkReq(), ws.ID)
	// After membership loss, a user with no grants should land on the
	// isGuest=true branch and whatever visibleSlugSet the store returns
	// for a fully-unauthorized resolver — the important invariant is
	// that the *second* call reflects the post-revocation state, not a
	// stale copy of the first.
	if !v2.isGuest {
		t.Fatal("after removal the recomputed visibility should mark user as guest")
	}
	_ = admin // silence unused on branches that don't exercise admin path
}

// TestSSESubscriberStillHasAccess_AdminDemotion verifies that an admin
// who is demoted to "member" AFTER the SSE connection was established
// loses access on the next revalidation tick. A regression would mean
// the cached user snapshot from request context is trusted
// indefinitely (admin-forever bug).
func TestSSESubscriberStillHasAccess_AdminDemotion(t *testing.T) {
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	// Need another admin so demoting the first doesn't fail the
	// last-admin guard in SetUserRole.
	if _, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin2@example.com", Name: "Admin2", Password: "correct-horse-battery-staple", Role: "admin",
	}); err != nil {
		t.Fatalf("create second admin: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "AdminDemotionTest"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	// Request context still has the ORIGINAL admin snapshot (Role="admin").
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace="+ws.Slug, nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxCurrentUser, admin))

	// Before demotion: admin should have access.
	if !srv.sseSubscriberStillHasAccess(req, ws.ID) {
		t.Fatal("admin should have access before demotion")
	}

	// Demote the admin to member. They're NOT a member of the workspace,
	// so they shouldn't have access via membership either.
	if err := srv.store.SetUserRole(admin.ID, "member"); err != nil {
		t.Fatalf("demote admin: %v", err)
	}

	// SAME request context (same cached snapshot showing Role="admin"),
	// but the database has the demoted role. Without the fresh GetUser
	// the function would return true from the admin-role branch. With
	// it, the post-demotion role is checked and access is denied.
	if srv.sseSubscriberStillHasAccess(req, ws.ID) {
		t.Fatal("demoted admin without workspace membership should NOT have access; cached snapshot was trusted")
	}
}

// TestComputeSSEVisibility_DemotedAdminGetsFilter verifies that when a
// global admin is demoted to "member" mid-stream BUT remains a member
// of the workspace, their visibility snapshot immediately picks up the
// collection-level filter instead of continuing to use the admin
// short-circuit (nil visibleSlugSet = "all access"). Without the fresh
// GetUser inside computeSSEVisibility, visibleCollectionIDs would read
// the cached Role="admin" from request context and keep returning nil.
func TestComputeSSEVisibility_DemotedAdminGetsFilter(t *testing.T) {
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if _, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin2@example.com", Name: "Admin2", Password: "correct-horse-battery-staple", Role: "admin",
	}); err != nil {
		t.Fatalf("create second admin: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "DemoteVis"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	// Add the admin as a member with "specific" collection access but
	// zero granted collections — after demotion they should see nothing.
	// Keep membership so sseSubscriberStillHasAccess still returns true;
	// we specifically want to exercise the visibility recompute, not
	// the revoke-and-close path.
	if err := srv.store.AddWorkspaceMember(ws.ID, admin.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, admin.ID, "specific", nil); err != nil {
		t.Fatalf("set specific collection access: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace="+ws.Slug, nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxCurrentUser, admin))

	// Before demotion: admin bypass → visibleSlugSet == nil (all access).
	// visibleCollectionIDs / computeSSEVisibility treats the user as
	// admin regardless of their member.CollectionAccess setting.
	v1 := srv.computeSSEVisibility(req, ws.ID)
	if v1.visibleSlugSet != nil {
		t.Fatalf("admin should have unrestricted visibility; got %v", v1.visibleSlugSet)
	}

	// Demote to member. The cached snapshot in the request still says
	// Role="admin". Without the fresh fetch inside computeSSEVisibility,
	// the admin short-circuit would keep returning nil here, ignoring
	// the "specific" + empty-granted-set member config entirely.
	if err := srv.store.SetUserRole(admin.ID, "member"); err != nil {
		t.Fatalf("demote admin: %v", err)
	}

	v2 := srv.computeSSEVisibility(req, ws.ID)
	if v2.visibleSlugSet == nil {
		t.Fatal("demoted admin should NOT have unrestricted visibility; cached Role=admin was trusted")
	}
	// With empty granted-collection set + system collections added
	// automatically, the visibleSlugSet should be a finite set of only
	// the system collection(s) they inherit — NOT nil. That's enough
	// to prove the fresh-fetch path fired.
}

// TestSSEMembershipRevalInterval_DefaultsToSensibleCadence pins the
// default tick interval. Not functional on its own; it's a guard so a
// future cleanup doesn't accidentally set the production default to a
// test-scale value like 100ms.
func TestSSEMembershipRevalInterval_DefaultsToSensibleCadence(t *testing.T) {
	const minReasonable = 30 // seconds
	const maxReasonable = 300
	sec := int(sseMembershipRevalInterval.Seconds())
	if sec < minReasonable || sec > maxReasonable {
		t.Fatalf("sseMembershipRevalInterval = %ds, want between %d and %d",
			sec, minReasonable, maxReasonable)
	}
}
