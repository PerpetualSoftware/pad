package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// BUG-2699 — the push endpoint answering honestly for a publish that did
// not happen, or that it cannot confirm happened.
//
// WHY THESE ASSERT WHAT THEY ASSERT (CONVE-12). The end state a naive
// test would reach for is "the notification did not arrive", and that is
// worthless here: it is equally true of the BROKEN behaviour, which
// published nothing and returned 200 pushed:true. The observable
// difference between fixed and unfixed lives entirely in the RESPONSE —
// the wrong behaviour's fingerprint is a 200 carrying pushed:true — so
// that is what these drive, with the status code and the body checked
// separately rather than one standing in for the other.

// stubBus is a watchevents.Bus whose Publish returns a caller-chosen
// error. Everything else is inert: these tests never subscribe, and a
// stub that silently satisfied the read side would invite a future test
// to believe it had exercised delivery.
type stubBus struct {
	mu       sync.Mutex
	err      error
	attempts int
}

func (b *stubBus) Publish(n watchevents.Notification) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attempts++
	return b.err
}

func (b *stubBus) publishAttempts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts
}

func (b *stubBus) Subscribe() chan watchevents.Notification { return nil }
func (b *stubBus) SubscribeAndReplaySince(int64) (chan watchevents.Notification, []watchevents.Notification) {
	return nil, nil
}
func (b *stubBus) Unsubscribe(chan watchevents.Notification) {}
func (b *stubBus) EventsSince(int64) []watchevents.Notification {
	return nil
}
func (b *stubBus) Close() {}

// TestPushToItem_ClosedBusIsRefused: ErrBusClosed proves nothing was
// published, so the caller gets the same 503 `unavailable` the nil-bus
// branch already returns — never a 200 with pushed:true.
//
// The `unavailable` CODE is asserted, not just the status: the web
// client's PUSH_PRE_PUBLISH_ERROR_CODES allow-list keys off the code, and
// it is what tells PushToAgentDialog this send is safe to re-offer. A 503
// under some other code would silently move the UI onto its
// outcome-unknown branch.
func TestPushToItem_ClosedBusIsRefused(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	bus := &stubBus{err: watchevents.ErrBusClosed}
	srv.SetWatchEventsBus(bus)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "triage this"})

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for a closed bus, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// The wrong behaviour's fingerprint, asserted directly.
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err == nil && resp.Pushed {
		t.Fatalf("response claimed pushed:true for a refused publish: %s", rr.Body.String())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("parse error envelope: %v (body: %s)", err, rr.Body.String())
	}
	code := envelope.Code
	if code == "" {
		code = envelope.Error.Code
	}
	if code != "unavailable" {
		t.Fatalf("expected code %q (the web allow-list entry that re-arms the send), got %q (body: %s)",
			"unavailable", code, rr.Body.String())
	}
	// Premise of this test, asserted rather than assumed: the handler did
	// reach the publish. Without this the 503 above would also pass if
	// some earlier validation had rejected the request, and the test would
	// be named for a path it never took.
	if got := bus.publishAttempts(); got != 1 {
		t.Fatalf("expected exactly 1 publish attempt, got %d", got)
	}
}

// TestPushToItem_UnconfirmedPublishIsNotReportedAsSent: any NON-closed
// publish error means the notification may or may not have gone out (a
// go-redis retry can publish and still return an error), so the response
// must not claim success — and must NOT use a code on the web's
// pre-publish allow-list, since re-offering the send could duplicate a
// dispatch.
func TestPushToItem_UnconfirmedPublishIsNotReportedAsSent(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	bus := &stubBus{err: errors.New("redis publish: connection reset")}
	srv.SetWatchEventsBus(bus)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "triage this"})

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for an unconfirmed publish, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err == nil && resp.Pushed {
		t.Fatalf("response claimed pushed:true for an unconfirmed publish: %s", rr.Body.String())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("parse error envelope: %v (body: %s)", err, rr.Body.String())
	}
	code := envelope.Code
	if code == "" {
		code = envelope.Error.Code
	}
	if code != pushPublishUnconfirmedCode {
		t.Fatalf("expected code %q, got %q (body: %s)", pushPublishUnconfirmedCode, code, rr.Body.String())
	}
	// The load-bearing half of this assertion, and the reason the code is
	// checked at all: `unavailable` would send the web dialog down its
	// re-arm path, offering a resend for a message that may already have
	// been delivered.
	if code == "unavailable" {
		t.Fatalf("unconfirmed publish must not reuse the pre-publish-refusal code")
	}
	if got := bus.publishAttempts(); got != 1 {
		t.Fatalf("expected exactly 1 publish attempt, got %d", got)
	}
	// NEVER retried by the server, for the same reason the CLI and the web
	// client never retry a push: no idempotency key.
}

// TestPushToItem_SucceedsWhenPublishAccepted is the positive control for
// both tests above. Without it, a handler that refused every push would
// pass them, and they would be evidence of nothing.
func TestPushToItem_SucceedsWhenPublishAccepted(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	bus := &stubBus{err: nil}
	srv.SetWatchEventsBus(bus)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "triage this"})

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for an accepted publish, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if !resp.Pushed {
		t.Fatalf("expected pushed:true on an accepted publish, got %s", rr.Body.String())
	}
	if got := bus.publishAttempts(); got != 1 {
		t.Fatalf("expected exactly 1 publish attempt, got %d", got)
	}
}

// TestBestEffortProducerSucceedsWhenThePublishFails — codex round 30
// (coverage-gap sweep).
//
// publishWatchNotification's whole ruling is that a producer layered on a
// committed store write DISCARDS a publish failure: the item exists, the
// comment exists, and failing the caller's request over a lost
// notification would tell the client its write failed when it did not.
// Every test covered the SUCCESS path, so an implementation that
// propagated the error and 500'd a perfectly good comment would have
// passed.
//
// Asserts what the wrong behaviour would DO — a 5xx on the write, and a
// comment missing afterwards — rather than that a notification was
// absent, which is equally true of both behaviours.
func TestBestEffortProducerSucceedsWhenThePublishFails(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	bus := &stubBus{err: errors.New("redis publish: connection reset")}
	srv.SetWatchEventsBus(bus)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/comments", tok.Token,
		map[string]interface{}{"body": "this comment must survive a failed notification"})

	if rr.Code != http.StatusCreated {
		t.Fatalf("a failed watch notification must not fail the write it rides on; got %d (body: %s)",
			rr.Code, rr.Body.String())
	}

	// And the write really committed — a 201 with nothing behind it would
	// satisfy the check above.
	list := bearerCall(t, srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/comments", tok.Token, nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list comments: %d %s", list.Code, list.Body.String())
	}
	if !strings.Contains(list.Body.String(), "must survive a failed notification") {
		t.Fatalf("the comment was not persisted: %s", list.Body.String())
	}

	// THE PREMISE, asserted rather than assumed (codex round 31, and my own
	// rule about tests asserting their own premise). Without this, the two
	// checks above pass for a handler that never publishes at all — no bus,
	// the producer removed, the notification hook deleted — so the test
	// would be named for a failed publish it never performed.
	if got := bus.publishAttempts(); got != 1 {
		t.Fatalf("expected the producer to attempt exactly 1 publish, got %d — this test proves nothing about a FAILED publish if none was attempted", got)
	}
}
