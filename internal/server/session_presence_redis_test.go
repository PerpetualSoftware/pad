package server

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// BUG-2698 — the registry half.
//
// These drive TWO RedisSessionPresence values against ONE miniredis,
// because that is what the bug is: two padd processes behind a load
// balancer, each with its own registry object, disagreeing about who is
// connected. A single-instance test would pass on the broken code.

func newRedisPresencePair(t *testing.T) (*RedisSessionPresence, *RedisSessionPresence, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	newOne := func() *RedisSessionPresence {
		client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		p := NewRedisSessionPresence(client)
		t.Cleanup(p.Close)
		return p
	}
	return newOne(), newOne(), mr
}

// TestRedisSessionPresence_VisibleAcrossInstances is the defect, stated
// directly: a session registered on instance B must appear in instance A's
// listing. Before this type, A's ListForUser returned nothing for it, and
// handlers_push.go skipped the publish on that basis.
func TestRedisSessionPresence_VisibleAcrossInstances(t *testing.T) {
	t.Parallel()
	instanceB, instanceA, _ := newRedisPresencePair(t)

	id := instanceB.Add("user-1", SessionIdentity{Label: "docapp", PID: 4242, Armed: true})

	sessions := instanceA.ListForUser("user-1")
	if len(sessions) != 1 {
		t.Fatalf("instance A must see the session registered on B; got %d sessions", len(sessions))
	}
	if sessions[0].ID != id {
		t.Fatalf("expected session id %q, got %q", id, sessions[0].ID)
	}
	// The fields the target picker and the push gate actually read. Armed
	// especially: deliveredSessionCount drops unarmed sessions, so an entry
	// that crossed the wire with Armed lost would be counted as absent and
	// the push skipped — the original bug wearing a different hat.
	if !sessions[0].Armed {
		t.Fatal("Armed must survive the round trip: an unarmed-looking session is skipped by the push gate")
	}
	if sessions[0].Label != "docapp" || sessions[0].PID != 4242 {
		t.Fatalf("identity lost in transit: %+v", sessions[0])
	}
	if sessions[0].ConnectedAt.IsZero() {
		t.Fatal("ConnectedAt must survive the round trip — it is the picker's sort key")
	}
}

// TestMemorySessionPresence_NotVisibleAcrossInstances is the NEGATIVE
// CONTROL for the test above, and it is what makes that test evidence
// rather than a tautology: the same two-registry shape against the
// in-memory implementation must NOT see across, because that is precisely
// the behaviour BUG-2698 reports. If this ever passes, the test above has
// stopped discriminating.
func TestMemorySessionPresence_NotVisibleAcrossInstances(t *testing.T) {
	t.Parallel()
	instanceB := NewMemorySessionPresence()
	instanceA := NewMemorySessionPresence()

	instanceB.Add("user-1", SessionIdentity{Label: "docapp", Armed: true})

	if got := len(instanceA.ListForUser("user-1")); got != 0 {
		t.Fatalf("in-memory presence is per-process by definition; instance A saw %d sessions", got)
	}
}

// TestRedisSessionPresence_RemoveIsImmediateAndCrossInstance: a clean
// disconnect must deregister NOW, not on the TTL. If Remove only stopped
// the renewal, a closed session would linger in every other instance's
// picker for the full TTL — and a targeted push at it would be published
// and delivered to nobody.
func TestRedisSessionPresence_RemoveIsImmediateAndCrossInstance(t *testing.T) {
	t.Parallel()
	instanceB, instanceA, _ := newRedisPresencePair(t)

	id := instanceB.Add("user-1", SessionIdentity{Armed: true})
	if got := len(instanceA.ListForUser("user-1")); got != 1 {
		t.Fatalf("precondition: A should see 1 session, got %d", got)
	}

	instanceB.Remove("user-1", id)

	if got := len(instanceA.ListForUser("user-1")); got != 0 {
		t.Fatalf("Remove must be visible immediately on other instances; A still sees %d", got)
	}
}

// TestRedisSessionPresence_RemoveIsIdempotentAndSafeForUnknownIDs pins the
// interface contract the stream handler depends on: Remove runs from a
// defer that also fires on paths where Add was never reached.
func TestRedisSessionPresence_RemoveIsIdempotentAndSafeForUnknownIDs(t *testing.T) {
	t.Parallel()
	p, _, _ := newRedisPresencePair(t)

	p.Remove("user-1", "")
	p.Remove("user-1", "never-added")
	id := p.Add("user-1", SessionIdentity{Armed: true})
	p.Remove("user-1", id)
	p.Remove("user-1", id)

	if got := len(p.ListForUser("user-1")); got != 0 {
		t.Fatalf("expected no sessions, got %d", got)
	}
}

// TestRedisSessionPresence_CrashedInstanceEntriesExpire is the reaping
// story session_presence.go's interface doc REQUIRES of any out-of-process
// implementation — the constraint MemorySessionPresence is exempt from
// because its entries die with its process.
//
// A crash is modelled by the only thing a crash actually is from Redis's
// side: renewals stop. The registry object is abandoned WITHOUT Close and
// WITHOUT Remove, exactly as a killed process leaves it, and time is moved
// past the TTL.
func TestRedisSessionPresence_CrashedInstanceEntriesExpire(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	// Short TTL so the expiry path is driven for real rather than waited
	// out. The renewal interval is longer than the TTL here on purpose:
	// this models the crashed process, which by definition renews nothing.
	crashed := &RedisSessionPresence{
		client:        client,
		sessionKeyTTL: 2 * time.Second,
		renewInterval: time.Hour,
		renewals:      make(map[string]context.CancelFunc),
	}
	crashed.Add("user-1", SessionIdentity{Armed: true})

	survivor := NewRedisSessionPresence(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	t.Cleanup(survivor.Close)
	if got := len(survivor.ListForUser("user-1")); got != 1 {
		t.Fatalf("precondition: the survivor should see the crashed instance's session, got %d", got)
	}

	mr.FastForward(3 * time.Second)

	if got := len(survivor.ListForUser("user-1")); got != 0 {
		t.Fatalf("a crashed instance's entries must expire; still listing %d", got)
	}
	// The index must self-heal too, not just the entry. Without the prune,
	// the SET accumulates a dead member per crashed session forever, and
	// every future ListForUser pays an MGET for keys that will never exist.
	members, err := client.SMembers(t.Context(), sessionIndexKey("user-1")).Result()
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("expired session left %d stale index member(s): %v", len(members), members)
	}
}

// TestRedisSessionPresence_RenewalKeepsALiveSessionListed is the positive
// control for the expiry test above. Without it, an implementation that
// dropped every session after the TTL — renewals broken, entries never
// restored — would pass the crash test and look correct.
func TestRedisSessionPresence_RenewalKeepsALiveSessionListed(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	live := &RedisSessionPresence{
		client:        client,
		sessionKeyTTL: 2 * time.Second,
		renewInterval: 20 * time.Millisecond,
		renewals:      make(map[string]context.CancelFunc),
	}
	t.Cleanup(live.Close)
	live.Add("user-1", SessionIdentity{Armed: true})

	// Let several renewals run, moving miniredis's clock less far than the
	// TTL between them, so the session survives only if renewal works.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		mr.FastForward(time.Second)
	}

	if got := len(live.ListForUser("user-1")); got != 1 {
		t.Fatalf("a renewed session must stay listed; got %d", got)
	}
}

// TestRedisSessionPresence_OrderIsDeterministic pins the same ordering
// MemorySessionPresence guarantees — oldest first, id as tiebreaker. Redis
// SETs are unordered, so without the explicit sort the web target picker
// would reorder under the user's cursor between polls.
func TestRedisSessionPresence_OrderIsDeterministic(t *testing.T) {
	t.Parallel()
	p, reader, _ := newRedisPresencePair(t)

	for i := 0; i < 5; i++ {
		p.Add("user-1", SessionIdentity{Armed: true})
	}

	first := reader.ListForUser("user-1")
	if len(first) != 5 {
		t.Fatalf("expected 5 sessions, got %d", len(first))
	}
	for i := 1; i < len(first); i++ {
		if first[i].ConnectedAt.Before(first[i-1].ConnectedAt) {
			t.Fatalf("not oldest-first at %d: %v before %v", i, first[i].ConnectedAt, first[i-1].ConnectedAt)
		}
	}
	for attempt := 0; attempt < 5; attempt++ {
		again := reader.ListForUser("user-1")
		for i := range again {
			if again[i].ID != first[i].ID {
				t.Fatalf("listing order changed between reads at index %d", i)
			}
		}
	}
}

// TestRedisSessionPresence_ScopedPerUser: the cross-user boundary that
// makes a foreign target_session_id structurally indistinguishable from a
// vanished one (deliveredSessionCount's doc comment) has to hold for the
// shared registry too — sharing state across INSTANCES must not leak state
// across USERS.
func TestRedisSessionPresence_ScopedPerUser(t *testing.T) {
	t.Parallel()
	instanceB, instanceA, _ := newRedisPresencePair(t)

	instanceB.Add("user-1", SessionIdentity{Armed: true})

	if got := len(instanceA.ListForUser("user-2")); got != 0 {
		t.Fatalf("user-2 must not see user-1's sessions; got %d", got)
	}
}
