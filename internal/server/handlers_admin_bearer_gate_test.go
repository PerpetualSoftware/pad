package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-1616 extended coverage — admin-via-bearer gate at the SSE and
// WebSocket-collab entry points that sit OUTSIDE RequireWorkspaceAccess.
// These endpoints (handlers_events.go::handleSSE,
// handlers_collab.go::authorizeCollabAccess) maintain their own auth
// logic with their own admin bypass branches; the middleware fix at
// internal/server/middleware_auth.go does not reach them.
//
// Codex review (round 1) caught the gap. These tests pin the four sites
// fixed in the same round:
//
//   1. handleSSE entry — explicit GetWorkspaceMember check after the
//      slug resolve for bearer-borne admin.
//   2. sseSubscriberStillHasAccess — admin-bypass gated on
//      !isBearerAuth(r), plus membership-only stance on the grants
//      fallback.
//   3. computeSSEVisibility — admin-bypass gated on !isBearerAuth(r);
//      bearer-admin gets a real VisibleCollectionIDs filter snapshot.
//   4. authorizeCollabAccess — admin-bypass gated on !isBearerAuth(r),
//      plus membership-only stance on the grants fallback.

// mkAdminBearerReq builds an *http.Request that looks like a bearer-
// authenticated call: ctxCurrentUser set, Authorization: Bearer header
// present. The header content doesn't have to be a real token because
// the helper functions under test read isBearerAuth(r), which only
// checks header *presence*.
func mkAdminBearerReq(t *testing.T, path string, admin *models.User) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer pad_test_fake_token_value_not_validated_here")
	return req.WithContext(context.WithValue(req.Context(), ctxCurrentUser, admin))
}

// mkAdminCookieReq is the same shape minus the Authorization header.
// Simulates the cookie-session path where isBearerAuth must return
// false and the admin bypass must continue to fire.
func mkAdminCookieReq(t *testing.T, path string, admin *models.User) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	return req.WithContext(context.WithValue(req.Context(), ctxCurrentUser, admin))
}

// TestSSESubscriberStillHasAccess_AdminBearer_DeniedOnNonMember pins
// the bearer-aware revalidation path. Cookie admin → still has access;
// bearer admin (Authorization: Bearer header) → denied.
func TestSSESubscriberStillHasAccess_AdminBearer_DeniedOnNonMember(t *testing.T) {
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	// Need a second admin so we don't accidentally hit the
	// last-admin guard in any future SetUserRole assertion.
	if _, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin2@example.com", Name: "Admin2", Password: "correct-horse-battery-staple", Role: "admin",
	}); err != nil {
		t.Fatalf("create second admin: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "BearerSSE"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	// Cookie path — admin bypass fires, access granted.
	if !srv.sseSubscriberStillHasAccess(mkAdminCookieReq(t, "/api/v1/events?workspace="+ws.Slug, admin), ws.ID) {
		t.Fatal("cookie-session admin should still have access (web UI / SPA path)")
	}

	// Bearer path — admin bypass suppressed, no membership row →
	// denied. This is the BUG-1616 extension Codex called out.
	if srv.sseSubscriberStillHasAccess(mkAdminBearerReq(t, "/api/v1/events?workspace="+ws.Slug, admin), ws.ID) {
		t.Fatal("bearer-borne admin without workspace membership must NOT pass SSE revalidation (BUG-1616)")
	}
}

// TestSSESubscriberStillHasAccess_AdminBearer_DeniedEvenWithGrants
// pins the membership-only stance: a bearer-admin with a guest grant
// on a workspace they didn't join still loses access on the next
// revalidation tick. Mirrors the same stance enforced by
// RequireWorkspaceAccess.
func TestSSESubscriberStillHasAccess_AdminBearer_DeniedEvenWithGrants(t *testing.T) {
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "owner@example.com", Name: "Owner", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "BearerSSEGrant", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	// Seed a collection + grant the admin view access. Admin is NOT
	// in workspace_members.
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK",
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	if _, err := srv.store.CreateCollectionGrant(ws.ID, coll.ID, admin.ID, "view", owner.ID); err != nil {
		t.Fatalf("create grant: %v", err)
	}

	if srv.sseSubscriberStillHasAccess(mkAdminBearerReq(t, "/api/v1/events?workspace="+ws.Slug, admin), ws.ID) {
		t.Fatal("bearer-admin with only a guest grant must NOT pass SSE revalidation (membership-only stance)")
	}
}

// TestComputeSSEVisibility_AdminBearer_GetsFilter verifies the
// per-tick visibility recompute applies the bearer-admin gate too.
// Without the gate, an admin who's a workspace member with
// CollectionAccess="specific" would still see events from every
// collection because the recompute fell through the admin bypass
// before resolving VisibleCollectionIDs.
func TestComputeSSEVisibility_AdminBearer_GetsFilter(t *testing.T) {
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "VisBearer", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, admin.ID, "editor"); err != nil {
		t.Fatalf("add admin as member: %v", err)
	}
	// Two collections; restrict admin to one.
	visible, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Visible", Slug: "visible", Prefix: "VIS",
	})
	if err != nil {
		t.Fatalf("create visible collection: %v", err)
	}
	if _, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Hidden", Slug: "hidden", Prefix: "HID",
	}); err != nil {
		t.Fatalf("create hidden collection: %v", err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, admin.ID, "specific", []string{visible.ID}); err != nil {
		t.Fatalf("set member collection access: %v", err)
	}

	// Cookie path — admin bypass fires, no collection filter.
	vCookie := srv.computeSSEVisibility(mkAdminCookieReq(t, "/api/v1/events?workspace="+ws.Slug, admin), ws.ID)
	if vCookie.visibleSlugSet != nil {
		t.Errorf("cookie admin should see all events (nil filter); got %v", vCookie.visibleSlugSet)
	}

	// Bearer path — bypass suppressed, real filter applied.
	vBearer := srv.computeSSEVisibility(mkAdminBearerReq(t, "/api/v1/events?workspace="+ws.Slug, admin), ws.ID)
	if vBearer.visibleSlugSet == nil {
		t.Fatalf("bearer admin should get a filtered visibility set; got nil (no filter)")
	}
	if !vBearer.visibleSlugSet["visible"] {
		t.Errorf("bearer admin should see the granted 'visible' collection; got %v", vBearer.visibleSlugSet)
	}
	if vBearer.visibleSlugSet["hidden"] {
		t.Errorf("bearer admin should NOT see the unrestricted 'hidden' collection; got %v", vBearer.visibleSlugSet)
	}
}

// TestAuthorizeCollabAccess_AdminBearer_DeniedOnNonMember pins the
// WebSocket-collab entry gate. Bearer-admin on a non-member workspace's
// item → forbidden. Cookie-admin → allowed (web UI preserves the
// global-admin affordance).
func TestAuthorizeCollabAccess_AdminBearer_DeniedOnNonMember(t *testing.T) {
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "owner@example.com", Name: "Owner", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "CollabBearer", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK",
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
		Title: "Secret", CreatedBy: "user", Source: "test",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	// Cookie-session admin — bypass fires, access allowed.
	if err := srv.authorizeCollabAccess(
		mkAdminCookieReq(t, "/api/v1/collab/"+item.ID, admin), item,
	); err != nil {
		t.Fatalf("cookie admin should be allowed on collab WS; got error %v", err)
	}

	// Bearer admin — bypass suppressed, no membership → 403.
	err = srv.authorizeCollabAccess(
		mkAdminBearerReq(t, "/api/v1/collab/"+item.ID, admin), item,
	)
	if err == nil {
		t.Fatal("bearer admin without workspace membership must NOT pass authorizeCollabAccess (BUG-1616)")
	}
	if se, ok := err.(*statusError); !ok {
		t.Errorf("expected *statusError, got %T: %v", err, err)
	} else if se.code != http.StatusForbidden {
		t.Errorf("expected 403, got %d (msg=%s)", se.code, se.Error())
	}
}
