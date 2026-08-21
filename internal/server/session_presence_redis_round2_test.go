package server

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// BUG-2698, codex round 2. Three findings, three instruments.

// stalledRedis returns the address of a listener that ACCEPTS connections
// and then never answers — the shape a hung Redis presents, which a closed
// port does not: a closed port fails fast, and failing fast was never the
// problem.
func stalledRedis(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Held open, never read, never written.
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return ln.Addr().String()
}

// TestRedisSessionPresence_StalledRedisDoesNotHangShutdown — codex round
// 2, P1, with the claim narrowed to what is actually true.
//
// The first version of this test asserted that Add returns within the
// op timeout when Redis stalls. IT FAILED, and it was right to: go-redis
// does not apply a command context to connection establishment, so a
// 150ms context still took 5.0s (the client's DialTimeout). The bound I
// had just written into a code comment did not exist. Measured, not
// reasoned about — which is the only reason the comment now says
// something true.
//
// What IS assertable, and what the finding was actually about: Close must
// not hang. Its WaitGroup counter includes a goroutine that has not
// started yet (incremented in Add before the registration write, so a
// Close racing an Add cannot return early), so a stalled Redis parks
// Add's write between Close and its own completion. Close therefore
// drains with a deadline.
//
// The assertion is on TIME because that is what the wrong behaviour
// violates: an unbounded Close does not return a wrong value, it simply
// never returns.
//
// HONEST SCOPE, because the mutation matrix says so and a test that
// oversells itself is worse than none: replacing closeDrainTimeout with 24
// hours leaves this test GREEN. go-redis's own dial/read timeouts already
// bound Add's write, so Close is finite here with or without the deadline.
// What this pins is the end-to-end property — a stalled Redis blocks
// neither the connecting handler nor shutdown — not the deadline itself,
// which is a backstop for a client someone reconfigures without timeouts.
func TestRedisSessionPresence_StalledRedisDoesNotHangShutdown(t *testing.T) {
	t.Parallel()
	client := redis.NewClient(&redis.Options{Addr: stalledRedis(t), MaxRetries: -1})
	p := &RedisSessionPresence{
		client:        client,
		sessionKeyTTL: time.Minute,
		renewInterval: time.Hour,
		opTimeout:     150 * time.Millisecond,
		renewals:      make(map[string]*renewal),
	}

	added := make(chan string, 1)
	go func() { added <- p.Add("user-1", SessionIdentity{Armed: true}) }()

	// Close races the IN-FLIGHT Add rather than waiting for it, because
	// that is the scenario the finding described: the WaitGroup counter is
	// already incremented while the write is parked, so Close is queued
	// behind a Redis call that is going nowhere.
	time.Sleep(100 * time.Millisecond)

	closed := make(chan struct{})
	start := time.Now()
	go func() { p.Close(); close(closed) }()
	select {
	case <-closed:
		if elapsed := time.Since(start); elapsed > closeDrainTimeout+5*time.Second {
			t.Fatalf("Close took %v, past its own drain bound", elapsed)
		}
	case <-time.After(closeDrainTimeout + 10*time.Second):
		t.Fatal("Close did not return while Redis was stalled — graceful shutdown would hang behind a renewal goroutine")
	}

	// And Add itself must have returned, with an id: a registry write that
	// failed is under-reporting, not a reason to refuse the connection.
	select {
	case id := <-added:
		if id == "" {
			t.Fatal("Add must return a session id even when the registry write fails")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Add never returned against a stalled Redis — the registration write has no bound at all, not even the client's")
	}
}

// TestPruneIndexScript_LeavesLiveMembers — codex round 2, P2. The prune
// re-checks existence INSIDE Redis, because between ListForUser's MGet and
// its prune a renewal can restore the key it observed missing. That
// happens exactly when it hurts most: an outage long enough to expire
// entries, then recovery, has every instance rewriting at once. An
// unconditional SREM would evict a LIVE session from the index, hiding it
// from the picker and making targeted pushes skip.
//
// Drives the script directly, which is the only way to observe the
// re-check: through ListForUser the two orderings are indistinguishable
// from outside.
func TestPruneIndexScript_LeavesLiveMembers(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := t.Context()

	const user = "user-1"
	live, dead := "live-session", "dead-session"
	if err := client.SAdd(ctx, sessionIndexKey(user), live, dead).Err(); err != nil {
		t.Fatalf("seed index: %v", err)
	}
	// Only the live one has a session key — the dead one's expired, which
	// is exactly the state ListForUser observes before pruning.
	if err := client.Set(ctx, sessionKey(user, live), "{}", time.Minute).Err(); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	if err := pruneIndexScript.Run(ctx, client,
		[]string{sessionIndexKey(user)},
		userIDKeyPrefix(user), live, dead,
	).Err(); err != nil && !errors.Is(err, redis.Nil) {
		t.Fatalf("prune: %v", err)
	}

	members, err := client.SMembers(ctx, sessionIndexKey(user)).Result()
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	// The wrong behaviour's fingerprint: the live member gone because the
	// prune trusted a stale observation instead of re-checking.
	if len(members) != 1 || members[0] != live {
		t.Fatalf("prune must remove only members whose key is gone; index = %v", members)
	}
}

// TestRedisSessionPresence_UndecodableEntryIsAnError — codex round 2, P2.
// A row we cannot interpret is an UNKNOWN registry state, not an absent
// session. Skipping it silently made deliveredSessionCount report a number
// that looked complete, so a targeted push at the omitted session returned
// delivered_sessions:0 and was SKIPPED — the instruction dropped while the
// caller was told nothing was listening.
func TestRedisSessionPresence_UndecodableEntryIsAnError(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	p := NewRedisSessionPresence(client)
	t.Cleanup(p.Close)

	id := p.Add("user-1", SessionIdentity{Armed: true})

	// CONTROL FIRST: a well-formed registry lists cleanly. Without this the
	// assertion below would also pass for an implementation that errored on
	// everything.
	sessions, err := p.ListForUser("user-1")
	if err != nil || len(sessions) != 1 {
		t.Fatalf("precondition: expected 1 session and no error, got %d / %v", len(sessions), err)
	}

	if err := client.Set(t.Context(), sessionKey("user-1", id), "not json", time.Minute).Err(); err != nil {
		t.Fatalf("corrupt entry: %v", err)
	}

	sessions, err = p.ListForUser("user-1")
	if err == nil {
		t.Fatalf("an undecodable entry must be reported, not skipped; got %d sessions and no error", len(sessions))
	}
	if sessions != nil {
		t.Fatalf("a failed read must not return a partial list that looks complete; got %d sessions", len(sessions))
	}
}

// TestRedisSessionPresence_CloseDoesNotWaitForeverOnAParkedRenewal —
// codex round 4, P2, and the instrument the round-2 version of this file
// admitted it did not have.
//
// closeDrainTimeout was untestable while it was a constant: under the
// production client config go-redis's own timeouts always release the
// write first, so removing the deadline changed nothing observable. That
// admission was honest but it left a behaviour with no coverage. Parking a
// renewal INSIDE its write through the onRenewWrite seam produces the one
// state the deadline exists for — a goroutine that nothing else will
// release — and makes the difference between bounded and unbounded
// observable.
//
// Fails by HANGING when the deadline is removed, which is what an
// unbounded Close does; the timeout below is what turns that into a test
// failure rather than a stuck suite.
func TestRedisSessionPresence_CloseDoesNotWaitForeverOnAParkedRenewal(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	p := &RedisSessionPresence{
		client:        client,
		sessionKeyTTL: time.Minute,
		renewInterval: 5 * time.Millisecond,
		drainTimeout:  200 * time.Millisecond,
		renewals:      make(map[string]*renewal),
		onRenewWrite: func() {
			once.Do(func() {
				close(entered)
				<-release
			})
		},
	}
	t.Cleanup(func() { close(release) })

	p.Add("user-1", SessionIdentity{Armed: true})
	<-entered // a renewal is parked and nothing will release it

	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()

	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned with a renewal parked inside its write — the drain has no deadline, so shutdown waits forever")
	}
}

// TestRedisSessionPresence_EmptyListIsNotNil — codex round 5, P2.
//
// handleListSessions marshals whatever ListForUser returns straight into
// the `sessions` field, so a nil slice serialises as `"sessions": null`
// where MemorySessionPresence produces `"sessions": []`. A consumer that
// maps over the array breaks against one implementation and not the other.
//
// Asserts the WIRE, not just the Go value: a non-nil check would pass for
// a value that still marshalled to null through some future indirection.
func TestRedisSessionPresence_EmptyListIsNotNil(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	p := NewRedisSessionPresence(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	t.Cleanup(p.Close)

	sessions, err := p.ListForUser("nobody-here")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if sessions == nil {
		t.Fatal("an empty listing must be an empty slice, not nil — it serialises as null and breaks array consumers")
	}

	encoded, err := json.Marshal(sessionsResponse{Sessions: sessions, Count: len(sessions)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"sessions":[]`) {
		t.Fatalf("expected an empty array on the wire, got %s", encoded)
	}

	// CONTROL: the in-memory implementation, whose shape this is matching.
	memSessions, err := NewMemorySessionPresence().ListForUser("nobody-here")
	if err != nil {
		t.Fatalf("memory list: %v", err)
	}
	if memSessions == nil {
		t.Fatal("precondition: MemorySessionPresence was expected to return a non-nil empty slice")
	}
}

// TestRedisSessionPresence_RemoveAfterCloseStillWaits — codex round 6, P2.
//
// Round 1 made Remove wait for its session's renewal so an in-flight write
// could not re-create the key after the DEL. Close then removed the
// entries from the map before draining, which deleted the very lookup
// Remove uses to find that renewal — so a handler disconnecting
// concurrently with Close found nothing, skipped the wait, and reopened
// the same resurrection window by the other door.
//
// Drives that exact interleaving: a renewal parked inside its write, Close
// running (and hitting its drain bound), then a Remove arriving after.
// Remove must not return while that renewal can still write.
func TestRedisSessionPresence_RemoveAfterCloseStillWaits(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	p := &RedisSessionPresence{
		client:        client,
		sessionKeyTTL: time.Minute,
		renewInterval: 5 * time.Millisecond,
		// Long enough that the wait below is observable. The BOUND on that
		// wait is asserted separately, by the test after this one — the two
		// properties pull against each other (wait, but not forever) and
		// conflating them into one deadline would let either failure hide
		// behind the other.
		drainTimeout: 5 * time.Second,
		renewals:     make(map[string]*renewal),
		onRenewWrite: func() {
			once.Do(func() {
				close(entered)
				<-release
			})
		},
	}

	id := p.Add("user-1", SessionIdentity{Armed: true})
	<-entered

	closeDone := make(chan struct{})
	go func() { p.Close(); close(closeDone) }()

	removed := make(chan struct{})
	go func() {
		p.Remove("user-1", id)
		close(removed)
	}()

	// THE ASSERTION: Remove must still be waiting. The pre-fix version
	// found an empty map and went straight to its DEL, which the parked
	// write would then undo.
	select {
	case <-removed:
		t.Fatal("Remove returned after Close while a renewal was still in flight; that renewal can re-create the entry after the delete")
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	<-closeDone

	select {
	case <-removed:
	case <-time.After(5 * time.Second):
		t.Fatal("Remove never returned after the renewal was released")
	}

	reader := NewRedisSessionPresence(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	t.Cleanup(reader.Close)
	sessions, err := reader.ListForUser("user-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("a ghost session survived Remove-after-Close (%d listed)", len(sessions))
	}
}

// TestRedisSessionPresence_RemoveIsBoundedWhenARenewalWillNotStop —
// codex round 11, and the counterweight to the test above.
//
// Those two properties pull against each other: Remove must WAIT for an
// in-flight renewal (or a ghost survives the delete), and must NOT wait
// forever (or a parked renewal holds http.Server.Shutdown). Round 10
// bounded the wrong branch — the post-Close fallback rather than the
// `<-rn.done` path a shutdown actually takes, because Close RETAINS its
// entries so the handler finds one. The hang survived its own fix.
//
// Asserting the bound needs its own test rather than a shorter deadline on
// the one above, which would make each failure indistinguishable from the
// other.
func TestRedisSessionPresence_RemoveIsBoundedWhenARenewalWillNotStop(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	p := &RedisSessionPresence{
		client:        client,
		sessionKeyTTL: time.Minute,
		renewInterval: 5 * time.Millisecond,
		drainTimeout:  200 * time.Millisecond,
		renewals:      make(map[string]*renewal),
		onRenewWrite: func() {
			once.Do(func() {
				close(entered)
				<-release
			})
		},
	}
	t.Cleanup(func() { close(release) })

	id := p.Add("user-1", SessionIdentity{Armed: true})
	<-entered // parked, and nothing will release it

	removed := make(chan struct{})
	go func() {
		p.Remove("user-1", id)
		close(removed)
	}()

	select {
	case <-removed:
	case <-time.After(10 * time.Second):
		t.Fatal("Remove never returned against a renewal that will not stop — a disconnecting handler holds http.Server.Shutdown forever")
	}
}
