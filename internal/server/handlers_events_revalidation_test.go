package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xarmian/pad/internal/models"
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
