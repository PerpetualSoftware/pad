package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// BUG-2698 at the layer that OWNS the caller.
//
// session_presence_redis_test.go proves the registry crosses instances.
// That is a different claim from "the push endpoint now delivers
// cross-instance", and conflating the two is the exact mistake the day-49
// review caught one unit ago: a knob tested where it is CONSUMED proves
// the knob works, not that the handler turns it.
//
// So these drive the real failure scenario through HTTP:
//
//	1. A user's agent session connects to replica B and registers there.
//	2. The load balancer sends their push, naming that session id, to
//	   replica A.
//	3. A's registry did not contain the session, so deliveredSessionCount
//	   returned 0 and the handler SKIPPED the publish entirely, answering
//	   200 pushed:true delivered_sessions:0. B's session received nothing.

// recordingBus records what the handler put on the bus. Recording rather
// than delivering is deliberate: the notification's existence and its
// TargetSessionID are what distinguish "published, for whichever instance
// holds the target to pick up" from "skipped", and the shared Redis bus
// (BUG-2651) is what carries it from there. Reproducing that fan-out here
// would test BUG-2651 again, not this fix.
type recordingBus struct {
	stubBus
	mu      sync.Mutex
	targets []string
}

func (b *recordingBus) Publish(n watchevents.Notification) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.targets = append(b.targets, n.TargetSessionID)
	return nil
}

func (b *recordingBus) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.targets)
}

func (b *recordingBus) targetSessionIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.targets...)
}

// TestPushToItem_TargetedAtSessionOnAnotherInstance is the defect: with a
// SHARED registry, replica A must publish a push aimed at a session that
// only replica B holds, and must count it as delivered.
func TestPushToItem_TargetedAtSessionOnAnotherInstance(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)

	// Replica A: the one answering the POST. Its presence registry is
	// Redis-backed and it has never seen the session.
	srvA := testServer(t)
	busA := &recordingBus{}
	srvA.SetWatchEventsBus(busA)
	presenceA := NewRedisSessionPresence(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	t.Cleanup(presenceA.Close)
	srvA.SetSessionPresence(presenceA)

	slug, item, tok, user := setupWatchTestUser(t, srvA)

	// Replica B: a different process, a different registry object, the same
	// Redis. This is where the agent's stream is actually held.
	presenceB := NewRedisSessionPresence(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	t.Cleanup(presenceB.Close)
	sessionOnB := presenceB.Add(user.ID, SessionIdentity{Label: "docapp", Armed: true})

	rr := bearerJSON(t, srvA, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "triage this", "target_session_id": sessionOnB})

	if rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}

	// The WRONG behaviour's fingerprint, asserted directly (CONVE-12): the
	// broken handler answered 200 with delivered_sessions:0 having put
	// nothing on the bus. "The push worked" is not observable here — the
	// receiving stream lives in another process — so what is asserted is
	// what the skip would have left behind.
	if got := busA.count(); got != 1 {
		t.Fatalf("replica A must publish a push aimed at a session held by B; published %d notifications", got)
	}
	if got := busA.targetSessionIDs()[0]; got != sessionOnB {
		t.Fatalf("published notification targeted %q, want %q", got, sessionOnB)
	}
	if deliveredCount(t, resp) != 1 {
		t.Fatalf("delivered_sessions = %d, want 1 — the count is what the CLI and the web dialog read",
			deliveredCount(t, resp))
	}
}

// TestPushToItem_TargetedAtSessionOnAnotherInstance_MemoryRegistryDrops is
// the NEGATIVE CONTROL, and it is what makes the test above mean
// something: the identical scenario against per-process presence must
// still drop, because that IS the bug. It also documents the behaviour a
// self-hosted single-process deployment keeps.
func TestPushToItem_TargetedAtSessionOnAnotherInstance_MemoryRegistryDrops(t *testing.T) {
	t.Parallel()
	srvA := testServer(t)
	busA := &recordingBus{}
	srvA.SetWatchEventsBus(busA)
	srvA.SetSessionPresence(NewMemorySessionPresence())

	slug, item, tok, user := setupWatchTestUser(t, srvA)

	// "Replica B" — an entirely separate in-memory registry, which is what
	// a second process has.
	presenceB := NewMemorySessionPresence()
	sessionOnB := presenceB.Add(user.ID, SessionIdentity{Label: "docapp", Armed: true})

	rr := bearerJSON(t, srvA, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "triage this", "target_session_id": sessionOnB})

	if rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}
	if got := busA.count(); got != 0 {
		t.Fatalf("per-process presence cannot see B's session, so the publish must still be skipped; got %d", got)
	}
}

// TestPushToItem_BroadcastCountsSessionsOnEveryInstance covers the OTHER
// half BUG-2698 filed: delivered_sessions was a local count describing a
// global delivery. With a shared bus, a broadcast published on A reaches
// B's sessions — so a count that only saw A's under-reported.
func TestPushToItem_BroadcastCountsSessionsOnEveryInstance(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)

	srvA := testServer(t)
	busA := &recordingBus{}
	srvA.SetWatchEventsBus(busA)
	presenceA := NewRedisSessionPresence(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	t.Cleanup(presenceA.Close)
	srvA.SetSessionPresence(presenceA)

	slug, item, tok, user := setupWatchTestUser(t, srvA)

	// One session on the answering replica, two on another.
	presenceA.Add(user.ID, SessionIdentity{Label: "local", Armed: true})
	presenceB := NewRedisSessionPresence(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	t.Cleanup(presenceB.Close)
	presenceB.Add(user.ID, SessionIdentity{Label: "remote-1", Armed: true})
	presenceB.Add(user.ID, SessionIdentity{Label: "remote-2", Armed: true})

	// An UNARMED session on B, which must NOT be counted: it cannot receive
	// a push at all (watchNotificationVisible denies it), and counting it
	// would trade one honesty gap for another.
	presenceB.Add(user.ID, SessionIdentity{Label: "remote-unarmed", Armed: false})

	rr := bearerJSON(t, srvA, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "triage this"})

	if rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}
	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if deliveredCount(t, resp) != 3 {
		t.Fatalf("delivered_sessions = %d, want 3 (1 local + 2 remote armed, unarmed excluded)", deliveredCount(t, resp))
	}
}

// TestListSessions_ShowsSessionsFromEveryInstance covers the third
// consumer BUG-2698 names, and the one a user meets first: the web push
// dialog's target picker reads GET /api/v1/sessions, so a session on
// another replica was not merely mis-counted — it could not be SELECTED.
func TestListSessions_ShowsSessionsFromEveryInstance(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)

	srvA := testServer(t)
	presenceA := NewRedisSessionPresence(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	t.Cleanup(presenceA.Close)
	srvA.SetSessionPresence(presenceA)

	_, _, tok, user := setupWatchTestUser(t, srvA)

	presenceB := NewRedisSessionPresence(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	t.Cleanup(presenceB.Close)
	remoteID := presenceB.Add(user.ID, SessionIdentity{Label: "remote", Armed: true})

	rr := bearerCall(t, srvA, "GET", "/api/v1/sessions", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list sessions: %d %s", rr.Code, rr.Body.String())
	}
	if !containsSessionID(t, rr.Body.Bytes(), remoteID) {
		t.Fatalf("the picker must offer a session held by another replica; body: %s", rr.Body.String())
	}
}

func containsSessionID(t *testing.T, body []byte, id string) bool {
	t.Helper()
	var payload struct {
		Sessions []LiveSession `json:"sessions"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("parse sessions payload: %v (body: %s)", err, body)
	}
	for _, s := range payload.Sessions {
		if s.ID == id {
			return true
		}
	}
	return false
}
