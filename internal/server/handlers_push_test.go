package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// TestPushToItem_RequiresMessage covers the 400-on-empty(-after-trim)
// validation: an absent message and a whitespace-only one are both
// rejected, so a blank push instruction can never reach the bus.
func TestPushToItem_RequiresMessage(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	for _, body := range [][]byte{
		nil,
		[]byte(`{}`),
		[]byte(`{"message":""}`),
		[]byte(`{"message":"   "}`),
		[]byte(`{"message":"\n\t "}`),
	} {
		rr := bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token, body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("body %q: expected 400, got %d (body: %s)", body, rr.Code, rr.Body.String())
		}
	}
}

// TestPushToItem_ResponseCarriesWorkspaceSlug covers the P2 finding
// (dispatcher review round 2, codex): a successful push's JSON response
// must carry ref/workspace/pushed/message so `pad push --format json`
// has something real to print, and workspace must be the CANONICAL
// slug (not merely whatever the URL happened to contain) — same
// disambiguation rationale as the monitor line's workspace prefix.
func TestPushToItem_ResponseCarriesWorkspaceSlug(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "triage this"})
	if rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}

	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Ref != item.Ref {
		t.Fatalf("expected ref %q, got %q", item.Ref, resp.Ref)
	}
	if resp.Workspace != slug {
		t.Fatalf("expected workspace %q, got %q", slug, resp.Workspace)
	}
	if !resp.Pushed {
		t.Fatal("expected pushed: true")
	}
	if resp.Message != "triage this" {
		t.Fatalf("expected message %q, got %q", "triage this", resp.Message)
	}
}

// TestPushToItem_RejectsOverLongMessage covers the 400-over-cap
// validation (dispatcher review round 1): a message whose COLLAPSED
// length exceeds maxPushMessageLen is rejected rather than silently
// truncated — the message is the payload, not a preview, so truncation
// would corrupt what the user asked for. Built with extra internal
// whitespace so this also pins that the cap is measured AFTER
// whitespace collapse, not before (a message that's only over-length
// due to now-collapsed whitespace must NOT be rejected).
func TestPushToItem_RejectsOverLongMessage(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	overLong := strings.Repeat("a  \n  ", maxPushMessageLen) // collapses to > maxPushMessageLen chars
	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": overLong})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an over-cap message, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestPushToItem_MessageAtCapSurvivesIntact proves a message whose
// COLLAPSED length is exactly at maxPushMessageLen is accepted and
// delivered byte-for-byte (not silently truncated at the boundary) —
// the counterpart to the over-cap rejection test above.
func TestPushToItem_MessageAtCapSurvivesIntact(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectArmedWatchStream(ctx, t, ts.URL, tok.Token) // armed: this test asserts actual delivery (PLAN-2613 S1)
	waitForWatchEvent(t, ch, 3*time.Second)                  // connected

	atCap := strings.Repeat("a", maxPushMessageLen) // already whitespace-free: collapse is a no-op
	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": atCap})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for a message exactly at the cap, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	ev := waitForWatchEvent(t, ch, 3*time.Second)
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.Summary != atCap {
		t.Fatalf("expected the at-cap message to survive collapse+publish intact (len %d), got len %d", len(atCap), len(payload.Summary))
	}
}

// TestPushToItem_UnavailableWithoutBus covers the 503-on-nil-bus guard:
// unlike a durable watch, a push has no persistence to fall back on, so
// a missing bus must fail loudly rather than silently losing the
// message.
func TestPushToItem_UnavailableWithoutBus(t *testing.T) {
	t.Parallel()
	srv := testServer(t) // NOT testServerWithWatchEvents — bus is nil
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "triage this"})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestPushToItem_DeliversSelfAddressed mirrors
// TestWatchEventsStream_AddressedToYou_Assignment: a push must reach the
// SAME user's own connected stream with no explicit watch required, and
// carry the collapsed message as Summary.
func TestPushToItem_DeliversSelfAddressed(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectArmedWatchStream(ctx, t, ts.URL, tok.Token) // armed: this test asserts actual delivery (PLAN-2613 S1)
	waitForWatchEvent(t, ch, 3*time.Second)                  // connected

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "triage this\nwith the triage playbook"})
	if rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}

	ev := waitForWatchEvent(t, ch, 3*time.Second)
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.Kind != watchevents.KindPush {
		t.Fatalf("expected kind %q, got %q", watchevents.KindPush, payload.Kind)
	}
	if payload.ItemRef != item.Ref {
		t.Fatalf("expected item_ref %q, got %q", item.Ref, payload.ItemRef)
	}
	if payload.Summary != "triage this with the triage playbook" {
		t.Fatalf("expected newline-collapsed summary, got %q", payload.Summary)
	}
}

// TestPushToItem_InvisibleItemDenied covers the same resolve/visibility
// gate every other item-scoped handler enforces: pushing a nonexistent
// ref 404s.
func TestPushToItem_InvisibleItemDenied(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, _, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/NOPE-999/push", tok.Token,
		map[string]interface{}{"message": "triage this"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestPushToItem_TargetedSessionReceivesOnly is PLAN-2558 S5's (TASK-2588)
// core positive case: with TWO of the caller's own sessions connected, a
// push naming one of their ids reaches exactly that one, not the other,
// and delivered_sessions reports 1 — not the registry size of 2.
func TestPushToItem_TargetedSessionReceivesOnly(t *testing.T) {
	t.Parallel()
	srv := testServerWithPresence(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	chA := connectArmedWatchStream(ctxA, t, ts.URL, tok.Token) // armed: this is the session the test expects to actually receive the push
	waitForWatchEvent(t, chA, 3*time.Second)                   // connected — A's Add() has happened

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	chB := connectArmedWatchStream(ctxB, t, ts.URL, tok.Token) // armed too — B's exclusion from delivery here is about SESSION TARGETING, not arming
	waitForWatchEvent(t, chB, 3*time.Second)                   // connected — B's Add() has happened, strictly after A's

	status, sessions := getSessions(t, ts.URL, tok.Token)
	if status != http.StatusOK || len(sessions.Sessions) != 2 {
		t.Fatalf("expected 2 live sessions, got status=%d sessions=%+v", status, sessions)
	}
	// ListForUser orders oldest-connection-first (session_presence.go), and
	// A connected strictly before B, so sessions[0] is A's own id.
	targetID := sessions.Sessions[0].ID

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "for A only", "target_session_id": targetID})
	if rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.DeliveredSessions != 1 {
		t.Fatalf("expected delivered_sessions=1 for a session-targeted push, got %d", resp.DeliveredSessions)
	}

	ev := waitForWatchEvent(t, chA, 3*time.Second)
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.Summary != "for A only" {
		t.Fatalf("expected the targeted session to receive the push, got summary %q", payload.Summary)
	}

	assertNoWatchEventForRef(t, chB, item.Ref, 300*time.Millisecond)
}

// TestPushToItem_TargetedVanishedSessionMisses is S5's honest-miss case:
// a target_session_id that names no live session (mistyped, or expired)
// gets a 200 with delivered_sessions=0, never mis-delivered to a session
// that IS connected but wasn't the one addressed.
func TestPushToItem_TargetedVanishedSessionMisses(t *testing.T) {
	t.Parallel()
	srv := testServerWithPresence(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	waitForWatchEvent(t, ch, 3*time.Second) // connected

	// Bus growth, not just "this OTHER session didn't get it" (dispatcher
	// review round 1, codex): a targeted miss must skip the publish
	// entirely, not merely fail to match anyone downstream — see
	// pushResponse.DeliveredSessions' doc comment on why. This is the
	// seam that fails if the pre-publish snapshot-and-skip ever gets
	// reordered back to publish-then-count.
	before := len(srv.watchEvents.EventsSince(0))

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "nobody home", "target_session_id": "sess-does-not-exist"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for a vanished target (honest miss, not an error), got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.DeliveredSessions != 0 {
		t.Fatalf("expected delivered_sessions=0 for a vanished target, got %d", resp.DeliveredSessions)
	}
	if !resp.Pushed {
		t.Fatal("expected pushed=true — a targeted miss is still a successfully PROCESSED push (matching broadcast's existing no-listeners semantics), even though nothing was published to the bus")
	}
	if after := len(srv.watchEvents.EventsSince(0)); after != before {
		t.Fatalf("expected a targeted miss to skip the publish entirely (guaranteed no-op), but the bus grew from %d to %d entries", before, after)
	}

	assertNoWatchEventForRef(t, ch, item.Ref, 300*time.Millisecond)
}

// TestPushToItem_TargetedUnarmedSessionTreatedAsMiss covers PLAN-2613
// S1's honesty requirement for a REAL, currently-connected target that
// simply never declared armed=true: the caller's own dispatcher-approved
// call — "I'd take 'known but deaf' (0 delivered, success) over an
// error, since the session IS present" — matching this handler's
// existing shape for a vanished or cross-user target_session_id exactly
// (TestPushToItem_TargetedVanishedSessionMisses /
// TestPushToItem_TargetedSessionOfAnotherUserTreatedAsVanished): 200,
// delivered_sessions=0, and the publish is skipped entirely (the bus
// must not grow), not merely fail to reach the target downstream. An
// unarmed session is not distinguished from an absent one anywhere in
// this response — see deliveredSessionCount's doc comment.
func TestPushToItem_TargetedUnarmedSessionTreatedAsMiss(t *testing.T) {
	t.Parallel()
	srv := testServerWithPresence(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStream(ctx, t, ts.URL, tok.Token) // UNARMED, but genuinely connected
	waitForWatchEvent(t, ch, 3*time.Second)             // connected

	status, sessions := getSessions(t, ts.URL, tok.Token)
	if status != http.StatusOK || len(sessions.Sessions) != 1 {
		t.Fatalf("expected 1 live session, got status=%d sessions=%+v", status, sessions)
	}
	if sessions.Sessions[0].Armed {
		t.Fatalf("expected the unarmed connection's registry entry to report armed=false, got %+v", sessions.Sessions[0])
	}
	targetID := sessions.Sessions[0].ID

	before := len(srv.watchEvents.EventsSince(0))

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "should not reach an unarmed target", "target_session_id": targetID})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for an unarmed target (honest miss, not an error), got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.DeliveredSessions != 0 {
		t.Fatalf("expected delivered_sessions=0 for a real but unarmed target, got %d", resp.DeliveredSessions)
	}
	if !resp.Pushed {
		t.Fatal("expected pushed=true — an unarmed-target miss is still a successfully PROCESSED push, same shape as a vanished target")
	}
	if after := len(srv.watchEvents.EventsSince(0)); after != before {
		t.Fatalf("expected an unarmed target to skip the publish entirely, but the bus grew from %d to %d entries", before, after)
	}

	assertNoWatchEventForRef(t, ch, item.Ref, 300*time.Millisecond)
}

// TestPushToItem_TargetedSessionOfAnotherUserTreatedAsVanished pins the
// self-addressed boundary (dispatcher constraint, TASK-2588): a
// target_session_id that names a REAL, currently-connected session
// belonging to a DIFFERENT user must behave exactly like a vanished id —
// 200, delivered_sessions=0 — never leak to that other user's session and
// never let the pusher probe whether a given id exists on the server.
func TestPushToItem_TargetedSessionOfAnotherUserTreatedAsVanished(t *testing.T) {
	t.Parallel()
	srv := testServerWithPresence(t)
	slug, item, tokA, _ := setupWatchTestUser(t, srv)
	userB, err := srv.store.CreateUser(models.UserCreate{
		Email:    "push-target-test-b@example.com",
		Name:     "Push Target Tester B",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	tokB, err := srv.store.CreateAPIToken(userB.ID, models.APITokenCreate{Name: "push-target-test-b"}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	chA := connectWatchStream(ctxA, t, ts.URL, tokA.Token)
	waitForWatchEvent(t, chA, 3*time.Second)

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	chB := connectWatchStream(ctxB, t, ts.URL, tokB.Token)
	waitForWatchEvent(t, chB, 3*time.Second)

	_, bSessions := getSessions(t, ts.URL, tokB.Token)
	if len(bSessions.Sessions) != 1 {
		t.Fatalf("expected user B to see exactly 1 live session, got %+v", bSessions)
	}
	bSessionID := bSessions.Sessions[0].ID

	// See TestPushToItem_TargetedVanishedSessionMisses: a cross-user id
	// must skip the publish exactly like a genuinely vanished one — the
	// bus must not grow, not merely fail to reach B downstream.
	before := len(srv.watchEvents.EventsSince(0))

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tokA.Token,
		map[string]interface{}{"message": "should reach nobody", "target_session_id": bSessionID})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.DeliveredSessions != 0 {
		t.Fatalf("expected delivered_sessions=0 for another user's session id — a cross-user id must be indistinguishable from a vanished one, got %d", resp.DeliveredSessions)
	}
	if after := len(srv.watchEvents.EventsSince(0)); after != before {
		t.Fatalf("expected a cross-user target to skip the publish entirely, but the bus grew from %d to %d entries", before, after)
	}

	assertNoWatchEventForRef(t, chA, item.Ref, 300*time.Millisecond)
	assertNoWatchEventForRef(t, chB, item.Ref, 300*time.Millisecond)
}

// TestPushToItem_TargetSessionIDOverLengthRejected covers the 400-over-cap
// guard (dispatcher review round 1, codex): target_session_id is opaque
// and unvalidated by format, but not unbounded — decodeJSON allows request
// bodies up to 2 MiB, so without a length cap an authenticated caller
// could park arbitrarily large garbage strings in the bus's replay
// buffer on every push. The bus must not grow, matching the "reject
// before publish" posture the length check shares with the message cap.
func TestPushToItem_TargetSessionIDOverLengthRejected(t *testing.T) {
	t.Parallel()
	srv := testServerWithPresence(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	before := len(srv.watchEvents.EventsSince(0))
	overLong := strings.Repeat("a", maxPushTargetSessionIDLen+1)
	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "triage this", "target_session_id": overLong})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an over-cap target_session_id, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if after := len(srv.watchEvents.EventsSince(0)); after != before {
		t.Fatalf("expected an over-cap target_session_id to be rejected before publish, but the bus grew from %d to %d entries", before, after)
	}
}

// TestPushToItem_TargetSessionIDAtCapAccepted is the at-boundary
// counterpart: a target_session_id exactly at the cap must NOT be
// rejected (it is still a miss — nothing that long is a real registry
// id — but the length check itself must not be off-by-one).
func TestPushToItem_TargetSessionIDAtCapAccepted(t *testing.T) {
	t.Parallel()
	srv := testServerWithPresence(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	atCap := strings.Repeat("a", maxPushTargetSessionIDLen)
	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "triage this", "target_session_id": atCap})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for a target_session_id exactly at the cap, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.DeliveredSessions != 0 {
		t.Fatalf("expected delivered_sessions=0 — an at-cap id still names no live session, got %d", resp.DeliveredSessions)
	}
}

// TestPushToItem_BroadcastDeliveredSessionsCountsLiveSessions covers the
// broadcast (omitted target_session_id) mode with delivered_sessions
// wired up: it reaches every one of the caller's own connected sessions,
// and the count is the actual number that matched — 2, from
// ListForUser(userID) — not any GLOBAL registry size.
func TestPushToItem_BroadcastDeliveredSessionsCountsLiveSessions(t *testing.T) {
	t.Parallel()
	srv := testServerWithPresence(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	chA := connectArmedWatchStream(ctxA, t, ts.URL, tok.Token) // armed: this test asserts both sessions actually receive the broadcast
	waitForWatchEvent(t, chA, 3*time.Second)

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	chB := connectArmedWatchStream(ctxB, t, ts.URL, tok.Token)
	waitForWatchEvent(t, chB, 3*time.Second)

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "for everyone"})
	if rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.DeliveredSessions != 2 {
		t.Fatalf("expected delivered_sessions=2 for a broadcast with 2 live sessions, got %d", resp.DeliveredSessions)
	}

	for _, ch := range []<-chan watchSSEEvent{chA, chB} {
		ev := waitForWatchEvent(t, ch, 3*time.Second)
		var payload watchEventPayload
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			t.Fatalf("parse payload: %v", err)
		}
		if payload.Summary != "for everyone" {
			t.Fatalf("expected both sessions to receive the broadcast push, got summary %q", payload.Summary)
		}
	}
}

// TestPushToItem_BroadcastDeliveredSessionsCountsArmedOnly is PLAN-2613
// S1's honesty requirement for the broadcast (untargeted) count: with
// TWO of the caller's own sessions connected, only ONE armed, a
// broadcast push's delivered_sessions must report 1 — the registry has
// 2 entries, but reporting that number would be dishonest once delivery
// is armed-gated (the whole point deliveredSessionCount's doc comment
// makes). The armed session actually receives the push; the unarmed one
// does not, on the SAME broadcast publish.
func TestPushToItem_BroadcastDeliveredSessionsCountsArmedOnly(t *testing.T) {
	t.Parallel()
	srv := testServerWithPresence(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctxArmed, cancelArmed := context.WithCancel(context.Background())
	defer cancelArmed()
	chArmed := connectArmedWatchStream(ctxArmed, t, ts.URL, tok.Token)
	waitForWatchEvent(t, chArmed, 3*time.Second)

	ctxUnarmed, cancelUnarmed := context.WithCancel(context.Background())
	defer cancelUnarmed()
	chUnarmed := connectWatchStream(ctxUnarmed, t, ts.URL, tok.Token)
	waitForWatchEvent(t, chUnarmed, 3*time.Second)

	status, sessions := getSessions(t, ts.URL, tok.Token)
	if status != http.StatusOK || len(sessions.Sessions) != 2 {
		t.Fatalf("expected 2 live sessions, got status=%d sessions=%+v", status, sessions)
	}

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "for the armed session only"})
	if rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.DeliveredSessions != 1 {
		t.Fatalf("expected delivered_sessions=1 (armed sessions only) out of 2 connected, got %d", resp.DeliveredSessions)
	}

	ev := waitForWatchEvent(t, chArmed, 3*time.Second)
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.Summary != "for the armed session only" {
		t.Fatalf("expected the armed session to receive the broadcast push, got summary %q", payload.Summary)
	}

	assertNoWatchEventForRef(t, chUnarmed, item.Ref, 300*time.Millisecond)
}

// TestPushToItem_OmittedTargetSessionIDMatchesPreS5RequestShape is the
// wire-level compatibility leg (dispatcher plan): a request body in
// EXACTLY the pre-S5 shape — no target_session_id key at all, not even
// an empty string — must still be accepted and behave as pure broadcast,
// against a server with no presence registry at all (mirroring every
// pre-S5 push test's setup via testServerWithWatchEvents). A vanished
// registry answers delivered_sessions=0 honestly rather than erroring —
// there is no new error channel for a caller that never asked to target
// anything.
func TestPushToItem_OmittedTargetSessionIDMatchesPreS5RequestShape(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t) // NOT testServerWithPresence — no registry
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		[]byte(`{"message":"triage this"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Ref != item.Ref || resp.Workspace != slug || !resp.Pushed || resp.Message != "triage this" {
		t.Fatalf("pre-S5 fields must be unchanged for a pre-S5 request body, got %+v", resp)
	}
	if resp.DeliveredSessions != 0 {
		t.Fatalf("expected delivered_sessions=0 with no presence registry wired, got %d", resp.DeliveredSessions)
	}
}
