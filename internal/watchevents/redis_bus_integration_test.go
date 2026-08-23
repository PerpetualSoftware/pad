package watchevents

// BUG-2651 — the half the hermetic tests structurally cannot reach: the actual
// Redis round trip.
//
// Codex raised the gap three times, and it was right to: the publish path is a
// Lua script with KEYS/ARGV indexing, a wire format with an id prefix, a
// channel name, and a subscription lifecycle — none of which a test that calls
// fanOutLocally directly can exercise. The argument became concrete when the
// idempotency script shipped with `ARGV[3]` while Publish passed two arguments.
// That bug was caught by re-reading, which is not a control I want to rely on
// for the next one.
//
// miniredis is a test-only dependency (an in-process Redis with a Lua
// interpreter). It is not a substitute for a real server — it is an
// implementation of the same protocol — so a failure here is meaningful and a
// pass is good evidence rather than proof.

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/redisns"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newMiniredisBus(t *testing.T, size int) (*RedisBus, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBusWithReplaySize(client, size)
	t.Cleanup(b.Close)
	return b, mr
}

// waitFor polls until cond holds or the deadline passes. Delivery crosses a
// goroutine and a socket, so a bare read after Publish is a race.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestRedisBusRoundTripDeliversToTheSameInstance is the end-to-end path: a
// notification published here goes out to Redis, comes back through the
// subscription, and lands on a local subscriber with an id assigned by the
// script.
//
// It is also the only test that would catch a wrong channel name, a broken
// KEYS/ARGV mapping, or a receive goroutine that never started.
func TestRedisBusRoundTripDeliversToTheSameInstance(t *testing.T) {
	b, _ := newMiniredisBus(t, 64)

	ch, _ := b.Subscribe()
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1", Summary: "hello"})

	select {
	case n := <-ch:
		if n.ItemRef != "TASK-1" || n.Summary != "hello" {
			t.Errorf("payload did not survive the round trip: %+v", n)
		}
		if n.ID != 1 {
			t.Errorf("id: got %d, want 1 — the script assigns it from the shared counter", n.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("notification never came back through Redis; check the channel name and the receive goroutine")
	}
}

// TestRedisBusRoundTripCrossInstance is the actual bug being fixed: two buses
// on one Redis, one publishes, the OTHER receives. This is the case MemoryBus
// cannot do at all.
func TestRedisBusRoundTripCrossInstance(t *testing.T) {
	a, mr := newMiniredisBus(t, 64)

	clientB := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = clientB.Close() })
	busB := NewRedisBusWithReplaySize(clientB, 64)
	t.Cleanup(busB.Close)

	chB, _ := busB.Subscribe()

	// Published on A.
	a.Publish(Notification{Kind: KindPush, ItemRef: "TASK-9", Summary: "from A"})

	select {
	case n := <-chB:
		if n.ItemRef != "TASK-9" || n.Summary != "from A" {
			t.Errorf("instance B received the wrong payload: %+v", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a notification published on instance A never reached instance B — " +
			"this is the entire point of BUG-2651")
	}
}

// TestRedisBusIDsAreSharedAcrossInstances pins the shared counter: ids come
// from one Redis key, so two instances publishing interleaved do not both
// start at 1.
func TestRedisBusIDsAreSharedAcrossInstances(t *testing.T) {
	a, mr := newMiniredisBus(t, 64)

	clientB := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = clientB.Close() })
	busB := NewRedisBusWithReplaySize(clientB, 64)
	t.Cleanup(busB.Close)

	ch, _ := a.Subscribe()

	a.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	busB.Publish(Notification{Kind: KindComment, ItemRef: "TASK-2"})
	a.Publish(Notification{Kind: KindComment, ItemRef: "TASK-3"})

	var ids []int64
	for i := 0; i < 3; i++ {
		select {
		case n := <-ch:
			ids = append(ids, n.ID)
		case <-time.After(3 * time.Second):
			t.Fatalf("only received %d of 3 notifications: %v", len(ids), ids)
		}
	}
	for i, want := range []int64{1, 2, 3} {
		if ids[i] != want {
			t.Fatalf("ids %v — want 1,2,3 in order; a per-instance counter would repeat, "+
				"and a non-atomic assign could reorder", ids)
		}
	}
}

// TestRedisBusPublishIsIdempotentUnderRetry drives the dedupe token directly:
// running the SAME script arguments twice is what go-redis does when a reply is
// lost, and the second run must publish nothing.
//
// Calling the script twice by hand is the honest way to test this — provoking a
// real lost-reply retry is not something miniredis can be asked for, and a test
// that pretended to would be testing its own mock.
func TestRedisBusPublishIsIdempotentUnderRetry(t *testing.T) {
	b, _ := newMiniredisBus(t, 64)
	ch, _ := b.Subscribe()

	token := redisns.Default.Name(redisWatchDedupeSuffix) + "fixed-token-for-this-test"
	keys := []string{redisns.Default.Name(redisWatchSeqSuffix), redisns.Default.Name(redisWatchChannelSuffix), token, redisns.Default.Name(redisWatchEpochSuffix)}
	payload := `{"ItemRef":"TASK-1","Kind":"comment"}`
	const epochCandidate = "epoch-for-this-test"

	first, err := publishScript.Run(b.ctx, b.client, keys, payload, redisWatchDedupeTTLSeconds, epochCandidate).Int64()
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if first != 1 {
		t.Fatalf("first run returned id %d, want 1", first)
	}

	second, err := publishScript.Run(b.ctx, b.client, keys, payload, redisWatchDedupeTTLSeconds, epochCandidate).Int64()
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second != 0 {
		t.Fatalf("second run with the same token returned id %d, want 0 — a retried script must not "+
			"publish the notification a second time under a new id", second)
	}

	// Exactly one delivery, and the counter moved exactly once.
	waitFor(t, "the first notification", func() bool { return len(ch) == 1 })
	<-ch
	time.Sleep(50 * time.Millisecond)
	if got := len(ch); got != 0 {
		t.Fatalf("a second notification arrived (%d queued); the dedupe token did not hold", got)
	}
}

// TestRedisBusCloseStopsReceiving — Close must actually tear the subscription
// down, not merely close the local channels. A leaked subscription keeps a
// goroutine and a connection alive for the process's life.
func TestRedisBusCloseStopsReceiving(t *testing.T) {
	b, mr := newMiniredisBus(t, 64)

	ch, _ := b.Subscribe()
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("precondition failed: the bus never received its first notification")
	}

	b.Close()

	// The SERVER's view of who is subscribed is the half Close is responsible
	// for — asserting only that the local channel closed would pass on a
	// build that leaked the subscription and the goroutine behind it.
	waitFor(t, "Redis to report no subscribers on the channel", func() bool {
		for _, c := range mr.PubSubChannels("") {
			if c == redisns.Default.Name(redisWatchChannelSuffix) {
				return false
			}
		}
		return true
	})
	if n := mr.Publish(redisns.Default.Name(redisWatchChannelSuffix), "probe"); n != 0 {
		t.Fatalf("Redis still reports %d subscriber(s) after Close", n)
	}
}

// TestRedisBusPublishReportsAcceptanceOnASuccessfulRoundTrip — codex round
// 30 (coverage-gap sweep).
//
// publish_result_test.go covers the FAILURE directions — ErrBusClosed, and
// a transport error that must not claim the bus was closed — against a
// dead client. Nothing asserted the success direction against a real
// round trip: every integration test here ignored Publish's return value,
// so an implementation that published correctly and then returned a
// non-nil error would have made every push answer 502 push_unconfirmed
// while the suite stayed green.
//
// Asserts BOTH that the call reports acceptance and that the notification
// actually arrived, because either alone is compatible with the bug: a nil
// error proves nothing if nothing was published, and an arrival proves
// nothing about what the caller was told.
func TestRedisBusPublishReportsAcceptanceOnASuccessfulRoundTrip(t *testing.T) {
	bus, _ := newMiniredisBus(t, 16)
	ch, _ := bus.Subscribe()

	if err := bus.Publish(Notification{Kind: KindPush, ItemRef: "TASK-1", Summary: "triage this"}); err != nil {
		t.Fatalf("a successful publish must report acceptance, got %v", err)
	}

	select {
	case got := <-ch:
		if got.ItemRef != "TASK-1" || got.Summary != "triage this" {
			t.Fatalf("unexpected notification: %+v", got)
		}
		if got.ID == 0 {
			t.Fatal("a published notification must carry the id the script assigned")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("publish reported acceptance but nothing arrived — the nil error was not evidence of a publish")
	}
}
