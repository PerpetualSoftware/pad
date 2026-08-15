package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
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

// TestWatchEventsStream_RevalStillDeniesWhenWatchListReloadErrors covers
// TASK-2533 codex round 4: on a reval tick, if ListWatchesForUser
// errored, the handler's `continue` used to skip visCache.reset()
// entirely — the STALE identity/visibility stayed live for exactly as
// long as that unrelated query kept failing, so a demoted or disabled
// user kept receiving previously-visible content through the whole
// error window. Reproduces this with a forced, persistent watch-list
// reload fault (via the watchPredicatesLoadFault test seam) active
// across the SAME tick the connected user's access is narrowed, and
// asserts delivery for the now-invisible item is denied anyway —
// proving reset() runs unconditionally rather than being coupled to the
// reload's success.
//
// Two things this test has to keep true at once, and how it does it:
//
//   - The probe must ride WATCH-MATCHED delivery. It used to ride
//     addressed-to-you assignment, which IDEA-2544 Phase 2 (TASK-2551)
//     deleted; push, the surviving addressed path, is self-addressed
//     only, so there is no way for a second actor to publish addressed
//     traffic at a user whose access is being revoked underneath them.
//   - A watch-matched probe brings a SECOND mechanism that produces the
//     same silence: once maxConsecutiveWatchReloadFailures is crossed,
//     the handler clears the watch set wholesale (round 5 finding 1,
//     covered by the test below). A bare "nothing arrived" assertion
//     would pass on that alone (team CONVE-12). The control leg closes
//     it: a watch on an item in a STILL-granted collection must keep
//     delivering in the same window, which is only possible while the
//     watch set is still live — so the revoked item's silence can only
//     be visCache.
//
// Uses collection-access revocation rather than "disabled" (round 4's
// original vehicle) because a disabled user denies everything, control
// leg included. Same reset()-must-run-unconditionally code path:
// visCache.reset() re-derives per-workspace visibility AND re-fetches
// the user. TestWatchEventsStream_StopsDeliveringAfterUserDisabled above
// keeps the disabled-user path covered end to end, and
// TestWatchVisCache_DeniesAfterUserDisabled covers refreshUser directly.
func TestWatchEventsStream_RevalStillDeniesWhenWatchListReloadErrors(t *testing.T) {
	// Deliberately NOT t.Parallel() — mutates watchListRevalInterval,
	// same rationale as TestWatchEventsStream_StopsDeliveringAfterUserDisabled above.
	srv := testServerWithWatchEvents(t)
	slug := createWSWithCollections(t, srv)

	// This test used to run at 200ms to keep wall-clock margin between
	// "one faulting tick has reset visCache" (what the test needs) and
	// "three faulting ticks clear the watch set" (which would silence the
	// control leg for the wrong reason) — but margin is a bet on scheduler
	// latency, and CI lost it twice under load (BUG-2570). The fault
	// closure below now sequences those two mechanisms explicitly, so the
	// GREEN path is timing-independent at any interval.
	//
	// The interval still matters for one thing: REGRESSION DETECTION
	// sharpness (codex round 1 on this fix). After tick 2 lifts the
	// fault, tick 3 reloads successfully — and a successful reload both
	// resets visCache and replaces the watch set with an access-filtered
	// one, either of which would silence the revoked item even under the
	// regression this test guards against (reset skipped on FAULTING
	// ticks). So the revoked-item PATCH below must be published before
	// tick 3 for a hypothetical regression to be caught. 500ms gives
	// that single localhost request ~10x headroom over the ~50ms/request
	// worst case measured on loaded CI runners. Losing that race can
	// only mask a not-yet-written bug in one CI run — it cannot fail a
	// green build, which is the direction BUG-2570 actually hurt.
	origInterval := watchListRevalInterval
	watchListRevalInterval = 500 * time.Millisecond
	t.Cleanup(func() { watchListRevalInterval = origInterval })

	// A second collection, which the member KEEPS access to when
	// "tasks" is revoked below — it hosts the control item.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name": "Still Granted", "icon": "doc",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create second collection: %d %s", rr.Code, rr.Body.String())
	}
	var grantedColl models.Collection
	parseJSON(t, rr, &grantedColl)

	// Both items seeded BEFORE any user exists, while doRequest's
	// fresh-install auth bypass still applies.
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items",
		map[string]interface{}{"title": "Soon invisible", "fields": `{"status":"open"}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed revoked-collection item: %d %s", rr.Code, rr.Body.String())
	}
	var revokedItem models.Item
	parseJSON(t, rr, &revokedItem)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/"+grantedColl.Slug+"/items",
		map[string]interface{}{"title": "Stays visible", "fields": "{}"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed granted-collection item: %d %s", rr.Code, rr.Body.String())
	}
	var controlItem models.Item
	parseJSON(t, rr, &controlItem)

	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	actor, err := srv.store.CreateUser(models.UserCreate{
		Email: "reval-fault-actor@example.com", Name: "Actor", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create actor: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, actor.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember (actor): %v", err)
	}
	actorTok, err := srv.store.CreateAPIToken(actor.ID, models.APITokenCreate{Name: "reval-fault-actor-test"}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken (actor): %v", err)
	}

	// The connected user: an ordinary member (no collection_access row,
	// so full workspace access) until the revocation below.
	member, err := srv.store.CreateUser(models.UserCreate{
		Email: "reval-fault-member@example.com", Name: "Member", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember (member): %v", err)
	}
	memberTok, err := srv.store.CreateAPIToken(member.ID, models.APITokenCreate{Name: "reval-fault-member-test"}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken (member): %v", err)
	}
	// Watches created before connecting — the map is loaded once at
	// connect time, and the forced fault below stops it reloading.
	if _, err := srv.store.CreateWatch(ws.ID, member.ID, revokedItem.ID, ""); err != nil {
		t.Fatalf("CreateWatch (revoked item): %v", err)
	}
	if _, err := srv.store.CreateWatch(ws.ID, member.ID, controlItem.ID, ""); err != nil {
		t.Fatalf("CreateWatch (control item): %v", err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStream(ctx, t, ts.URL, memberTok.Token)
	waitForWatchEvent(t, ch, 3*time.Second) // connected

	// Baseline: watch-matched delivery works before any of this.
	rr = bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+revokedItem.Slug, actorTok.Token,
		map[string]interface{}{"fields": `{"status":"done"}`})
	if rr.Code != http.StatusOK {
		t.Fatalf("update revoked-collection item (baseline): %d %s", rr.Code, rr.Body.String())
	}
	waitForWatchEvent(t, ch, 3*time.Second)

	// Force every subsequent reval tick's watch-list reload to fail —
	// via the atomic seam (TASK-2533 codex round 5 finding 2): this
	// write happens AFTER the stream's background goroutine is already
	// running and reading the seam on its own reval ticks, so a plain
	// field here would race.
	// The closure doubles as the test's clock (BUG-2570). Within one
	// tick the handler runs visCache.reset() FIRST, then the reload —
	// so the closure fires strictly after its own tick's reset, and the
	// handler goroutine runs ticks sequentially. That gives an exact,
	// load-independent sequence with no sleeps:
	//
	//   tick 1: reset (old access) → closure narrows the member's access.
	//   tick 2: reset — guaranteed after the narrowing, on a tick whose
	//           reload ALSO faults (the regression under test) → closure
	//           lifts the fault and signals readiness.
	//   tick 3+: reloads succeed, so consecutive failures stop at exactly
	//           2 — strictly below maxConsecutiveWatchReloadFailures —
	//           and the clear-the-watch-set path can never be the reason
	//           the control leg goes silent.
	//
	// The old shape (install fault + narrow access, sleep 300ms) bet a
	// fixed sleep against both "at least one tick fired" and "fewer than
	// three fired before the control event lands"; loaded CI runners
	// lost the second bet twice (BUG-2570's two instances, both timing
	// out waiting for the control event after the set was cleared).
	faultTicks := 0 // handler-goroutine only; published to the test by close(ready)
	var narrowErr error
	ready := make(chan struct{})
	loadFault := func() error {
		faultTicks++
		switch faultTicks {
		case 1:
			// Narrow the connected member's access to everything except
			// the collection holding the control item. Done here, inside
			// tick 1, so tick 2's reset provably follows it.
			narrowErr = srv.store.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{grantedColl.ID})
			if narrowErr != nil {
				srv.watchPredicatesLoadFault.Store(nil)
				close(ready)
			}
		case 2:
			srv.watchPredicatesLoadFault.Store(nil)
			close(ready)
		}
		return errFakeWatchListReload
	}
	srv.watchPredicatesLoadFault.Store(&loadFault)
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for two faulting reval ticks")
	}
	if narrowErr != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", narrowErr)
	}

	// Must NOT deliver: the collection is no longer visible to the
	// member, and reset() has to have run despite the reload fault for
	// the handler to know that.
	rr = bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+revokedItem.Slug, actorTok.Token,
		map[string]interface{}{"fields": `{"status":"in-progress"}`})
	if rr.Code != http.StatusOK {
		t.Fatalf("update revoked-collection item (post-revocation): %d %s", rr.Code, rr.Body.String())
	}
	// Control: still-granted collection, same stream, same window — must
	// deliver, proving the watch set is still live and the silence above
	// is visCache's doing rather than a cleared watch map.
	rr = bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+controlItem.Slug+"/comments", actorTok.Token,
		map[string]interface{}{"body": "control nudge"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("comment on control item: %d %s", rr.Code, rr.Body.String())
	}

	// 10s, not the 3s the earlier delivery waits use: this is the wait
	// that timed out in both BUG-2570 CI instances, and it asserts
	// delivery-at-all, not latency — when green it returns immediately,
	// so the wider bound costs nothing.
	ev := waitForWatchEvent(t, ch, 10*time.Second)
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.ItemRef != controlItem.Ref {
		t.Fatalf("expected the still-granted item's notification %q, got %q — the revoked collection's item leaked despite the access change (visCache.reset() did not run on the faulting reval tick)",
			controlItem.Ref, payload.ItemRef)
	}
}

// errFakeWatchListReload is the sentinel error the
// watchPredicatesLoadFault test seam returns.
var errFakeWatchListReload = &testWatchListReloadError{}

type testWatchListReloadError struct{}

func (*testWatchListReloadError) Error() string { return "forced watch-list reload failure (test)" }

// TestWatchEventsStream_WatchSetClearedAfterPersistentReloadFailures
// covers TASK-2533 codex round 5 finding 1: `watches = fresh` only ran
// on the reload's success path, so under a PERSISTENT (not single-tick)
// reload failure the watch set stayed live indefinitely — a dead watch
// (removed, item deleted) would keep matching forever, and a watch
// created during the outage would be silently missed forever, since
// visCache gates current ACCESS, not whether a watch still legitimately
// exists. Forces maxConsecutiveWatchReloadFailures+1 consecutive
// failures via the fault seam and asserts: watch-matched delivery stops
// once the bound is crossed, ADDRESSED delivery is unaffected throughout
// (it never depended on the watch list), and watch-matched delivery
// RESUMES once the fault is cleared and a reload succeeds again — this
// is a bounded outage response, not a one-way ratchet.
//
// The addressed leg is a PUSH. It was an assignment-to-self until
// IDEA-2544 Phase 2 (TASK-2551) dropped assignment from the addressed
// stream; KindPush is now the only addressed kind, and it carries the
// same property the leg is here to prove — delivery gated by
// TargetUserID and visCache alone, never by the watch map.
func TestWatchEventsStream_WatchSetClearedAfterPersistentReloadFailures(t *testing.T) {
	// Deliberately NOT t.Parallel() — mutates watchListRevalInterval,
	// same rationale as the other reval-interval tests in this file.
	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	origInterval := watchListRevalInterval
	watchListRevalInterval = 30 * time.Millisecond
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

	// Baseline: watch-matched delivery works before any of this.
	rr = bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, tok.Token,
		map[string]interface{}{"fields": `{"status":"done"}`})
	if rr.Code != http.StatusOK {
		t.Fatalf("update item (baseline): %d %s", rr.Code, rr.Body.String())
	}
	waitForWatchEvent(t, ch, 3*time.Second)

	// Force every reload to fail, for MORE than the bound.
	loadFault := func() error { return errFakeWatchListReload }
	srv.watchPredicatesLoadFault.Store(&loadFault)
	time.Sleep(time.Duration(maxConsecutiveWatchReloadFailures+2) * watchListRevalInterval)

	// Watch-matched: must NOT deliver — the watch set was cleared once
	// the failure bound was crossed.
	rr = bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, tok.Token,
		map[string]interface{}{"fields": `{"status":"in-progress"}`})
	if rr.Code != http.StatusOK {
		t.Fatalf("update item (during outage): %d %s", rr.Code, rr.Body.String())
	}

	// Addressed (push): MUST still deliver — it depends only on
	// TargetUserID and visCache, which round 4 already decoupled from
	// the reload outcome.
	rr = bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "still addressed during the outage"})
	if rr.Code != http.StatusOK {
		t.Fatalf("push item (during outage): %d %s", rr.Code, rr.Body.String())
	}

	ev := waitForWatchEvent(t, ch, 3*time.Second)
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.Kind != watchevents.KindPush {
		t.Fatalf("expected the push notification (watch-matched delivery should have been suppressed by the outage), got kind %q: %+v", payload.Kind, payload)
	}

	// Recovery: clear the fault, let a reload succeed, and confirm
	// watch-matched delivery resumes — this is a bounded response to an
	// outage, not a permanent one-way ratchet.
	srv.watchPredicatesLoadFault.Store(nil)
	time.Sleep(2 * watchListRevalInterval)

	rr = bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, tok.Token,
		map[string]interface{}{"fields": `{"status":"done"}`})
	if rr.Code != http.StatusOK {
		t.Fatalf("update item (post-recovery): %d %s", rr.Code, rr.Body.String())
	}
	ev = waitForWatchEvent(t, ch, 3*time.Second)
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.Kind != watchevents.KindStatusChange {
		t.Fatalf("expected watch-matched delivery to resume after the reload recovers, got kind %q: %+v", payload.Kind, payload)
	}
}
