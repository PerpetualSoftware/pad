package watchevents

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/PerpetualSoftware/pad/internal/redisns"
)

// TestRedisBusHonoursTheNamespace asserts BOTH directions, because only
// the second one is the bug (BUG-2724): the namespaced bus must write its
// keys under the namespace AND must not touch the historical ones. A test
// that only checked the first would pass against an implementation that
// wrote both, which still cross-feeds a second installation.
func TestRedisBusHonoursTheNamespace(t *testing.T) {
	t.Parallel()

	keys, err := redisns.Parse("inst-a")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBusWithKeys(client, 64, keys)
	t.Cleanup(b.Close)

	ch := b.Subscribe()
	if err := b.Publish(Notification{Kind: KindPush, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("premise failed: the namespaced bus never round-tripped its own notification")
	}

	if !mr.Exists("pad:inst-a:watchevents_seq") {
		t.Errorf("namespaced counter pad:inst-a:watchevents_seq does not exist; keys present: %v", mr.Keys())
	}
	if mr.Exists("pad:watchevents_seq") {
		t.Errorf("the namespaced bus also wrote the DEFAULT counter pad:watchevents_seq — a second installation would still cross-feed")
	}
	if !mr.Exists("pad:inst-a:watchevents_epoch") {
		t.Errorf("namespaced epoch key missing; keys present: %v", mr.Keys())
	}

	// The CHANNEL is the half a key listing cannot see, and it is the one
	// pub/sub cross-feed actually travels on. A publish to the default
	// channel must reach nobody.
	//
	// Checked directly rather than polled, unlike the equivalent in
	// internal/events, because THIS bus's constructor waits for its
	// SUBSCRIBE to be confirmed before returning (see
	// NewRedisBusWithKeys) while the activity bus subscribes
	// asynchronously. That asymmetry is easy to assume away — the
	// activity-bus version of this test flaked on it — so it is named
	// here rather than left to be rediscovered.
	if n := mr.Publish("pad:watchevents", "probe"); n != 0 {
		t.Errorf("the namespaced bus is subscribed to the DEFAULT channel pad:watchevents (%d subscribers)", n)
	}
	if n := mr.Publish("pad:inst-a:watchevents", "probe"); n == 0 {
		t.Error("the namespaced bus is not subscribed to pad:inst-a:watchevents")
	}
}

// TestRedisBusDefaultKeepsHistoricalKeys is the upgrade promise: a
// deployment that sets no namespace must keep addressing exactly the keys
// it already has, or it silently loses its counter and epoch on restart.
func TestRedisBusDefaultKeepsHistoricalKeys(t *testing.T) {
	t.Parallel()

	b, mr := newMiniredisBus(t, 64)

	ch := b.Subscribe()
	if err := b.Publish(Notification{Kind: KindPush, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("premise failed: the default bus never round-tripped its own notification")
	}

	if !mr.Exists("pad:watchevents_seq") {
		t.Errorf("default bus did not write pad:watchevents_seq; keys present: %v", mr.Keys())
	}
	if n := mr.Publish("pad:watchevents", "probe"); n == 0 {
		t.Error("default bus is not subscribed to pad:watchevents")
	}
}
