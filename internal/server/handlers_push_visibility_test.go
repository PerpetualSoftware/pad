package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// pushCSRF is the double-submit token these cookie-authed pushes carry.
// Any fixed 64-char hex string works; the same constant several other
// test files use.
const pushCSRF = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// cookiePush issues POST .../push as a COOKIE-authenticated caller.
//
// The transport is the whole point of this helper, not an incidental
// detail: RequireWorkspaceAccess's platform-admin bypass is
// cookie-only (isBearerAuth gates it), so an admin who is not a member
// of the target workspace can push ONLY this way. Sending the same
// request with a bearer token would 403 and the case under test would
// never be reached.
func cookiePush(t *testing.T, srv *Server, slug, itemSlug, sessionToken string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal push body: %v", err)
	}
	req := httptest.NewRequest("POST", "/api/v1/workspaces/"+slug+"/items/"+itemSlug+"/push", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: sessionToken})
	req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: pushCSRF})
	req.Header.Set("X-CSRF-Token", pushCSRF)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// adminOutsideWorkspace builds the one configuration in which
// delivered_sessions can over-report, and the reasoning for why it is
// THIS configuration is worth keeping next to the fixture.
//
// Over-counting needs the pusher to be allowed to push while one of
// their OWN sessions cannot see the item. Per-user access narrowing (a
// "specific"-access member, a guest's grants) cannot produce that: it
// resolves identically for every stream that user holds, so it makes
// the count uniformly wrong rather than making two of their streams
// disagree — and it would also stop them pushing in the first place.
//
// The only genuinely per-CONNECTION input to the delivery predicate is
// auth transport (computeWatchAccessVisibility consults bearerAuth
// exactly once, inside the admin bypass). So the reproducible case is a
// platform ADMIN who is NOT a member of the target workspace:
//
//   - pushing over a COOKIE, where the admin bypass grants full access;
//   - holding an armed stream opened over a BEARER token, where the
//     bypass does not apply, membership is absent, and there are no
//     grants — so vis.allows is false and delivery drops the push.
//
// Returns the workspace slug, the item, the admin's cookie session
// token, and the admin's user ID.
func adminOutsideWorkspace(t *testing.T, srv *Server) (slug string, item models.Item, adminSessionToken, adminUserID string) {
	t.Helper()

	// ORDER MATTERS HERE. The workspace and its item are created FIRST,
	// during the fresh-install window where no users exist and the
	// unauthenticated test helpers work. Bootstrapping the admin first
	// would put the instance behind auth + CSRF and make
	// createWSWithCollections fail with a csrf_error that has nothing to
	// do with what is under test.
	slug = createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items",
		map[string]interface{}{"title": "Push target", "fields": `{"status":"open"}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed item: %d %s", rr.Code, rr.Body.String())
	}
	parseJSON(t, rr, &item)

	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}

	// Now the first user — the platform admin, holding a cookie session.
	adminSessionToken = bootstrapFirstUser(t, srv, "push-vis-admin@example.com", "Admin")
	admin, err := srv.store.GetUserByEmail("push-vis-admin@example.com")
	if err != nil || admin == nil {
		t.Fatalf("resolve admin: %v", err)
	}
	if admin.Role != "admin" {
		t.Fatalf("fixture premise broken: first user must be a platform admin, got role %q", admin.Role)
	}

	// The workspace belongs to somebody else entirely.
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "push-vis-owner@example.com", Name: "Owner", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser (owner): %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember (owner): %v", err)
	}

	// THE FIXTURE'S LOAD-BEARING PRECONDITION, asserted rather than
	// assumed: the admin must NOT be a member. If a future change to
	// createWSWithCollections or to bootstrap starts enrolling the first
	// user, the admin bypass and the membership path would agree, both
	// transports would resolve full access, and every assertion below
	// would pass for a reason that has nothing to do with the fix.
	member, err := srv.store.GetWorkspaceMember(ws.ID, admin.ID)
	if err != nil {
		t.Fatalf("GetWorkspaceMember: %v", err)
	}
	if member != nil {
		t.Fatalf("fixture premise broken: admin must not be a member of %s, but is (%q)", slug, member.Role)
	}

	return slug, item, adminSessionToken, admin.ID
}

// TestPushToItem_BroadcastDoesNotCountSessionsDeliveryWillDrop is the
// BUG-2725 over-count regression. It FAILS against the unfixed code,
// which reported 1.
//
// The admin's only armed session was opened over a bearer token. The
// push itself goes over a cookie, so the admin bypass lets it through
// to an item in a workspace they do not belong to — but that same
// bypass does not apply to the bearer-opened stream, whose visibility
// resolves to nothing at all. Delivery drops the notification; the
// count must say so.
func TestPushToItem_BroadcastDoesNotCountSessionsDeliveryWillDrop(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	presence := NewMemorySessionPresence()
	srv.SetSessionPresence(presence)

	slug, item, adminSessionToken, adminUserID := adminOutsideWorkspace(t, srv)

	// Armed, and opened over a BEARER token — the transport is the
	// variable under test.
	presence.Add(adminUserID, SessionIdentity{Label: "cli", Armed: true},
		SessionOrigin{BearerAuth: true})

	rr := cookiePush(t, srv, slug, item.Slug, adminSessionToken,
		map[string]interface{}{"message": "look at this"})
	if rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.DeliveredSessions == nil {
		t.Fatal("expected a delivered_sessions count, got the unknown-count shape")
	}
	if *resp.DeliveredSessions != 0 {
		t.Fatalf("delivered_sessions = %d, want 0: the only armed session is bearer-opened and "+
			"cannot see this item, so counting it is the over-report BUG-2725 filed",
			*resp.DeliveredSessions)
	}
}

// TestPushToItem_BroadcastStillCountsVisibleSessions is the control
// leg, and the reason the test above means anything.
//
// Same fixture, same admin, same workspace, ONE difference: the armed
// session was opened over a cookie, so the admin bypass applies to it
// and delivery genuinely will deliver. Without this leg, a fix that
// simply counted zero always — a broken resolver, a fail-closed path
// taken unconditionally, allows() hardwired to false — would pass the
// test above and read as correct.
func TestPushToItem_BroadcastStillCountsVisibleSessions(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	presence := NewMemorySessionPresence()
	srv.SetSessionPresence(presence)

	slug, item, adminSessionToken, adminUserID := adminOutsideWorkspace(t, srv)

	presence.Add(adminUserID, SessionIdentity{Label: "browser", Armed: true},
		SessionOrigin{BearerAuth: false})

	rr := cookiePush(t, srv, slug, item.Slug, adminSessionToken,
		map[string]interface{}{"message": "look at this"})
	if rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.DeliveredSessions == nil {
		t.Fatal("expected a delivered_sessions count, got the unknown-count shape")
	}
	if *resp.DeliveredSessions != 1 {
		t.Fatalf("delivered_sessions = %d, want 1: a cookie-opened session takes the admin "+
			"bypass and WILL receive this push — counting 0 here means the fix denies everything",
			*resp.DeliveredSessions)
	}
}

// TestPushToItem_TargetedAtInvisibleSessionSkipsPublish is the worse
// half of BUG-2725 and the one that lost an instruction.
//
// Pre-fix, the targeted gate passed (registry hit, armed, id matched),
// so the push WAS published, the stream dropped it on visibility, and
// the response said delivered_sessions: 1.
//
// CONVE-12: "nothing was delivered" is the weakest possible claim, and
// a response body alone cannot make it — handlePushToItem sets
// pushed:true on both branches. So this asserts the thing a wrongful
// publish would LEAVE BEHIND: a notification on the bus. A subscriber
// is attached before the push and must see nothing.
func TestPushToItem_TargetedAtInvisibleSessionSkipsPublish(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	presence := NewMemorySessionPresence()
	srv.SetSessionPresence(presence)

	slug, item, adminSessionToken, adminUserID := adminOutsideWorkspace(t, srv)

	sessionID := presence.Add(adminUserID, SessionIdentity{Label: "cli", Armed: true},
		SessionOrigin{BearerAuth: true})

	ch, _ := srv.watchEvents.Subscribe()

	rr := cookiePush(t, srv, slug, item.Slug, adminSessionToken, map[string]interface{}{
		"message":           "run the migration",
		"target_session_id": sessionID,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.DeliveredSessions == nil || *resp.DeliveredSessions != 0 {
		t.Fatalf("delivered_sessions = %v, want 0", resp.DeliveredSessions)
	}

	select {
	case n := <-ch:
		t.Fatalf("a notification reached the bus (%s/%s) — the publish should have been SKIPPED, "+
			"because the only matching session cannot see the item; publishing it is how the "+
			"pre-fix code lost an instruction behind a success", n.Kind, n.ItemRef)
	case <-time.After(250 * time.Millisecond):
		// Nothing published, which is the fix.
	}
}

// TestPushToItem_TargetedAtVisibleSessionPublishes is the control for
// the test above: the identical flow with a cookie-opened target must
// actually publish. Without it, a fix that skipped EVERY publish would
// satisfy the skip assertion and silently break push altogether.
func TestPushToItem_TargetedAtVisibleSessionPublishes(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	presence := NewMemorySessionPresence()
	srv.SetSessionPresence(presence)

	slug, item, adminSessionToken, adminUserID := adminOutsideWorkspace(t, srv)

	sessionID := presence.Add(adminUserID, SessionIdentity{Label: "browser", Armed: true},
		SessionOrigin{BearerAuth: false})

	ch, _ := srv.watchEvents.Subscribe()

	rr := cookiePush(t, srv, slug, item.Slug, adminSessionToken, map[string]interface{}{
		"message":           "run the migration",
		"target_session_id": sessionID,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}

	select {
	case n := <-ch:
		if n.Kind != watchevents.KindPush {
			t.Fatalf("expected a push notification, got %s", n.Kind)
		}
		if n.TargetSessionID != sessionID {
			t.Fatalf("expected target session %q, got %q", sessionID, n.TargetSessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("control failed: a push targeted at a VISIBLE session was never published — " +
			"the skip assertion in the sibling test proves nothing if this path is broken too")
	}
}

// TestPushSessionVisibility_ResolvesAtMostTwice pins the bound the fix
// rests on: however many sessions are counted, visibility is resolved
// at most once per transport.
//
// This is not a performance test. It is what makes "≤2 resolutions, not
// N" a property somebody can VERIFY instead of a claim in a comment,
// and it is the assertion that fails if a later edit moves the allows()
// call out of the memo or adds a second pass over the sessions.
func TestPushSessionVisibility_ResolvesAtMostTwice(t *testing.T) {
	t.Parallel()

	calls := 0
	vis := &sessionVisibility{resolve: func(bearerAuth bool) (bool, error) {
		calls++
		return true, nil
	}}

	presence := NewMemorySessionPresence()
	const perTransport = 25
	for i := 0; i < perTransport; i++ {
		presence.Add("u1", SessionIdentity{Armed: true}, SessionOrigin{BearerAuth: true})
		presence.Add("u1", SessionIdentity{Armed: true}, SessionOrigin{BearerAuth: false})
	}

	count, err := deliveredSessionCount(presence, "u1", "", vis)
	if err != nil {
		t.Fatalf("deliveredSessionCount: %v", err)
	}
	if count != perTransport*2 {
		t.Fatalf("counted %d sessions, want %d — the fixture, not the bound, is wrong",
			count, perTransport*2)
	}
	if calls > 2 {
		t.Fatalf("visibility resolved %d times for %d sessions; the memo bounds it at 2 "+
			"(one per transport) and that bound is what retires the per-push cost objection",
			calls, perTransport*2)
	}
	if got := vis.resolutions(); got != 2 {
		t.Fatalf("resolutions() = %d, want 2: both transports are present in the fixture, so "+
			"both must have been resolved — fewer means a transport was never consulted", got)
	}
}

// TestPushToItem_HandlerPassesRealVisibility is the CONVE-19 binding
// test. Every other test in this file could pass while the push HANDLER
// still called deliveredSessionCount with a nil visibility: the counter
// would be correct and unused, which is precisely the "tested at the
// layer that implements it, wiring left as an untested claim" shape.
//
// nil *sessionVisibility allows everything by design (see its doc
// comment), so a handler that passed nil would over-count exactly as
// the unfixed code did. This drives the real route and asserts the
// wiring by its consequence.
func TestPushToItem_HandlerPassesRealVisibility(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	presence := NewMemorySessionPresence()
	srv.SetSessionPresence(presence)

	slug, item, adminSessionToken, adminUserID := adminOutsideWorkspace(t, srv)

	// Two armed sessions, differing ONLY in transport. A nil (or
	// allow-everything) visibility counts both; the real one counts the
	// cookie session alone. The two outcomes are distinguishable, which
	// is what makes this an assertion about the binding rather than
	// about the counter.
	presence.Add(adminUserID, SessionIdentity{Label: "cli", Armed: true},
		SessionOrigin{BearerAuth: true})
	presence.Add(adminUserID, SessionIdentity{Label: "browser", Armed: true},
		SessionOrigin{BearerAuth: false})

	rr := cookiePush(t, srv, slug, item.Slug, adminSessionToken,
		map[string]interface{}{"message": "check the wiring"})
	if rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.DeliveredSessions == nil {
		t.Fatal("expected a delivered_sessions count")
	}
	if *resp.DeliveredSessions != 1 {
		t.Fatalf("delivered_sessions = %d, want 1 (the cookie session only). 2 means the handler "+
			"passed a visibility that allows everything — the counter is fixed but not wired",
			*resp.DeliveredSessions)
	}
}
