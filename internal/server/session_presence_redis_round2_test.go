package server

import (
	"errors"
	"net"
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
