package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// BUG-2698 codex round 1 P1 — an unreadable presence registry.
//
// THE SPLIT RULE these pin: presence GATES a targeted push but only
// COUNTS a broadcast, so an unreadable registry gets two different
// answers rather than one uniform refusal.
//
//   - targeted: the gate cannot be evaluated → refuse, publish NOTHING.
//   - broadcast: the publish was never gated → publish, and report the
//     count as null (unknown), never as 0.
//
// Both assert what the WRONG behaviour would DO, per CONVE-12. For the
// targeted leg that is a publish that should not exist; for the broadcast
// leg it is a `0` that would tell the caller nobody received a message
// that in fact went out.

// unreadablePresence is a SessionPresence whose reads fail. Add/Remove
// still work, because the failure being modelled is a Redis read outage,
// not a broken registry object.
type unreadablePresence struct {
	MemorySessionPresence
}

func (p *unreadablePresence) ListForUser(string) ([]LiveSession, error) {
	return nil, errors.New("redis: connection refused")
}

func newUnreadablePresence() *unreadablePresence {
	return &unreadablePresence{MemorySessionPresence: *NewMemorySessionPresence()}
}

// TestPushToItem_TargetedWithUnreadablePresenceIsRefused: the handler
// cannot tell whether the named session exists, so it must not guess in
// either direction. Refuse, publish nothing, and say so with a code the
// caller can safely resend on.
func TestPushToItem_TargetedWithUnreadablePresenceIsRefused(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	bus := &recordingBus{}
	srv.SetWatchEventsBus(bus)
	srv.SetSessionPresence(newUnreadablePresence())
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "triage this", "target_session_id": "some-session-id"})

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// THE LOAD-BEARING ASSERTION. A 503 alone would also pass for a
	// handler that published first and then errored — which is precisely
	// the outcome the code must not produce, because the caller is told
	// nothing was sent and would resend.
	if got := bus.count(); got != 0 {
		t.Fatalf("a refused targeted push must publish nothing; published %d", got)
	}
	if code := errorCodeOf(t, rr.Body.Bytes()); code != "unavailable" {
		t.Fatalf("expected code %q (the web allow-list entry that re-arms the send), got %q", "unavailable", code)
	}
}

// TestPushToItem_BroadcastWithUnreadablePresenceStillPublishes: refusing
// here would manufacture an outage — presence never gated a broadcast, so
// the delivery would have succeeded. Publish, and report the count as
// unknown.
func TestPushToItem_BroadcastWithUnreadablePresenceStillPublishes(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	bus := &recordingBus{}
	srv.SetWatchEventsBus(bus)
	srv.SetSessionPresence(newUnreadablePresence())
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "triage this"})

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if got := bus.count(); got != 1 {
		t.Fatalf("a broadcast must still publish when presence is unreadable; published %d", got)
	}

	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	// NULL, not 0 — and the two are asserted separately because a `0`
	// would deserialize into a non-nil pointer and quietly satisfy any
	// check written as "not more than zero". A 0 here would tell the
	// caller nobody received a notification that was in fact published.
	if resp.DeliveredSessions != nil {
		t.Fatalf("delivered_sessions must be null when the registry could not be read, got %d", *resp.DeliveredSessions)
	}
	// And the raw wire, because the Go struct could round-trip a null the
	// JSON did not actually carry (an absent key unmarshals to nil too).
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("parse raw: %v", err)
	}
	got, present := raw["delivered_sessions"]
	if !present {
		t.Fatalf("delivered_sessions must be PRESENT and null, not omitted — an absent key is a different signal to the web client (it means a pre-S5 server); body: %s", rr.Body.String())
	}
	if string(got) != "null" {
		t.Fatalf("delivered_sessions = %s, want null", got)
	}
}

// TestPushToItem_ReadablePresenceStillReportsACount is the positive
// control for both. Without it, a handler that reported null for every
// push — or refused every targeted one — would pass the pair above.
func TestPushToItem_ReadablePresenceStillReportsACount(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	bus := &recordingBus{}
	srv.SetWatchEventsBus(bus)
	presence := NewMemorySessionPresence()
	srv.SetSessionPresence(presence)
	slug, item, tok, user := setupWatchTestUser(t, srv)
	sessionID := presence.Add(user.ID, SessionIdentity{Armed: true}, SessionOrigin{})

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "triage this", "target_session_id": sessionID})

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.DeliveredSessions == nil {
		t.Fatal("a readable registry must report a real count, not null")
	}
	if *resp.DeliveredSessions != 1 {
		t.Fatalf("delivered_sessions = %d, want 1", *resp.DeliveredSessions)
	}
	if got := bus.count(); got != 1 {
		t.Fatalf("expected 1 publish, got %d", got)
	}
}

// TestListSessions_UnreadablePresenceIs503: the same "I cannot tell" must
// not reach the picker as an empty list. handleListSessions already 503s
// for a registry that was never wired; a registry that cannot be READ is
// the same answer to the caller and a worse lie if flattened, since the
// dialog renders an empty list as "No agent session is connected".
func TestListSessions_UnreadablePresenceIs503(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	srv.SetSessionPresence(newUnreadablePresence())
	_, _, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerCall(t, srv, "GET", "/api/v1/sessions", tok.Token, nil)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for an unreadable registry, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// The wrong behaviour's fingerprint: a 200 whose body says zero
	// sessions.
	var payload struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err == nil && rr.Code == http.StatusOK && payload.Count == 0 {
		t.Fatal("an unreadable registry must not be reported as zero connected sessions")
	}
}

// errorCodeOf pulls the error code out of whichever envelope shape the
// server used, so a test asserts the CODE (what the web client keys on)
// rather than only the status.
func errorCodeOf(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("parse error envelope: %v (body: %s)", err, body)
	}
	if envelope.Code != "" {
		return envelope.Code
	}
	return envelope.Error.Code
}
