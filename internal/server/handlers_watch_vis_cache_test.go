package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestWatchVisCache_DeniesAfterAdminDemotion covers TASK-2533 codex
// round 3: the round-2 watchVisCache captured *models.User ONCE at
// connect time and never re-fetched it — reset() cleared only the
// per-workspace map — so an admin demoted mid-connection kept
// fullAccess (via computeWatchAccessVisibility's admin bypass) until
// reconnect. Mirrors handlers_events_revalidation_test.go's
// TestSSESubscriberStillHasAccess_AdminDemotion / TestComputeSSEVisibility_DemotedAdminGetsFilter
// exactly: the request context holds the STALE pre-demotion snapshot
// (Role="admin"), the store holds the post-demotion truth, and the
// fix under test is what makes the SECOND call see the truth.
func TestWatchVisCache_DeniesAfterAdminDemotion(t *testing.T) {
	t.Parallel()
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "vis-cache-admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	// A second admin so SetUserRole's last-admin guard doesn't block the
	// demotion below.
	if _, err := srv.store.CreateUser(models.UserCreate{
		Email: "vis-cache-admin2@example.com", Name: "Admin2", Password: "correct-horse-battery-staple", Role: "admin",
	}); err != nil {
		t.Fatalf("create second admin: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "VisCacheDemotion"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	// admin is deliberately NOT a member of ws — their only access is
	// the platform-admin bypass.

	// Cookie-session-style request: no Authorization header, so
	// isBearerAuth(r) is false and the admin bypass (cookie-session only,
	// BUG-1616) is reachable — this is the SAME auth-transport
	// precondition the analogous existing SSE tests use.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil)

	cache := newWatchVisCache(srv, req, admin)
	before := cache.forWorkspace(ws.ID)
	if !before.fullAccess {
		t.Fatalf("expected the admin to have full access before demotion, got %+v", before)
	}

	if err := srv.store.SetUserRole(admin.ID, "member"); err != nil {
		t.Fatalf("demote admin: %v", err)
	}

	// SAME cache instance (same request context, same originally-cached
	// user) — reset() is what a reval tick calls.
	cache.reset()
	after := cache.forWorkspace(ws.ID)
	if after.fullAccess {
		t.Fatal("demoted admin without workspace membership must NOT keep full access after reset() — the cached user was not refreshed")
	}
}

// TestWatchVisCache_DeniesAfterUserDisabled covers the same gap for a
// disabled user: refreshUser must deny outright (not just narrow
// collection access) once the user is confirmed disabled.
func TestWatchVisCache_DeniesAfterUserDisabled(t *testing.T) {
	t.Parallel()
	srv := testServer(t)

	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "vis-cache-disabled@example.com", Name: "ToDisable", Password: "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "VisCacheDisable"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatalf("add member: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil)
	cache := newWatchVisCache(srv, req, user)

	before := cache.forWorkspace(ws.ID)
	if !before.fullAccess {
		t.Fatalf("expected the active owner-member to have full access before disable, got %+v", before)
	}

	if err := srv.store.DisableUser(user.ID); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	cache.reset()
	after := cache.forWorkspace(ws.ID)
	if after.fullAccess || len(after.visibleCollIDs) != 0 || len(after.grantedItemIDs) != 0 {
		t.Fatalf("expected a disabled user to be denied outright after reset(), got %+v", after)
	}
}

// TestWatchEventsStream_StopsDeliveringAfterUserDisabled is the
// HTTP/SSE-level regression: a live stream for a user who gets disabled
// mid-connection must stop delivering entirely once the next reval tick
// runs, not just narrow to a smaller visible set.
func TestWatchEventsStream_StopsDeliveringAfterUserDisabled(t *testing.T) {
	// Deliberately NOT t.Parallel(): this test mutates the PACKAGE-LEVEL
	// watchListRevalInterval var below. Every other watch-stream test in
	// this package reads that same var (via time.NewTicker(...) when it
	// opens its own SSE connection) and DOES run t.Parallel() — writing
	// to a shared package var from a parallel test while sibling
	// parallel tests concurrently read it is a genuine data race
	// (caught by go test -race, which attributed the failure to a whole
	// cluster of unrelated concurrently-running tests, not just this
	// one). Running serially means this test's mutate-then-restore
	// window never overlaps with another test's read of the same var.
	srv := testServerWithWatchEvents(t)
	slug, item, tok, user := setupWatchTestUser(t, srv)

	// Shrink the reval interval so the test doesn't wait 60 real
	// seconds — mirrors the package-level var override pattern this
	// codebase already uses for the analogous SSE membership interval.
	origInterval := watchListRevalInterval
	watchListRevalInterval = 50 * time.Millisecond
	t.Cleanup(func() { watchListRevalInterval = origInterval })

	rr := bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/watch", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("create watch: %d %s", rr.Code, rr.Body.String())
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	waitForWatchEvent(t, ch, 3*time.Second) // connected

	// Sanity: the stream delivers before disabling.
	admin2, err := srv.store.CreateUser(models.UserCreate{
		Email: "disable-owner@example.com", Name: "Owner2", Password: "pw-test-12345", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create actor: %v", err)
	}
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, admin2.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	actorTok, err := srv.store.CreateAPIToken(admin2.ID, models.APITokenCreate{Name: "disable-actor"}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	rr = bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, actorTok.Token,
		map[string]interface{}{"fields": `{"status":"done"}`})
	if rr.Code != http.StatusOK {
		t.Fatalf("update item: %d %s", rr.Code, rr.Body.String())
	}
	waitForWatchEvent(t, ch, 3*time.Second) // the pre-disable notification

	// Disable the connected user and wait past a reval tick.
	if err := srv.store.DisableUser(user.ID); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	time.Sleep(150 * time.Millisecond) // several reval ticks

	rr = bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, actorTok.Token,
		map[string]interface{}{"fields": `{"status":"in-progress"}`})
	if rr.Code != http.StatusOK {
		t.Fatalf("update item (post-disable): %d %s", rr.Code, rr.Body.String())
	}

	select {
	case ev, ok := <-ch:
		if ok {
			var payload watchEventPayload
			_ = json.Unmarshal([]byte(ev.Data), &payload)
			t.Fatalf("expected no further notifications after the connected user was disabled, got %+v (data: %s)", ev, ev.Data)
		}
		// Channel closed is also an acceptable outcome (server dropped
		// the connection outright).
	case <-time.After(1 * time.Second):
		// No event arrived — correct.
	}
}
