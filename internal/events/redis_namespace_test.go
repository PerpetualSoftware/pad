package events

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/PerpetualSoftware/pad/internal/redisns"
)

// TestRedisBusHonoursTheNamespace covers the activity-event keyspace's
// half of BUG-2724. Both directions asserted: under the namespace, and
// NOT under the historical names — an implementation that wrote both
// would still cross-feed a second installation sharing the endpoint.
func TestRedisBusHonoursTheNamespace(t *testing.T) {
	t.Parallel()

	keys, err := redisns.Parse("inst-b")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBusWithKeys(client, keys)
	t.Cleanup(b.Close)

	// A local subscriber is what starts the Redis-side subscription for
	// this workspace, so the channel assertions below have something to
	// observe.
	ch := b.Subscribe("ws-1")
	defer b.Unsubscribe(ch)

	b.Publish(Event{Type: "item.created", WorkspaceID: "ws-1"})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !mr.Exists("pad:inst-b:event_seq") {
		time.Sleep(2 * time.Millisecond)
	}

	if !mr.Exists("pad:inst-b:event_seq") {
		t.Errorf("namespaced counter pad:inst-b:event_seq does not exist; keys present: %v", mr.Keys())
	}
	if mr.Exists("pad:event_seq") {
		t.Errorf("the namespaced bus also wrote the DEFAULT counter pad:event_seq")
	}

	waitForSubscribers(t, mr, "pad:inst-b:events:ws-1", true)
	waitForSubscribers(t, mr, "pad:events:ws-1", false)

	// WHERE THE BUS PUBLISHES, which is a separate claim from where it
	// subscribes and the one cross-feed actually travels on. Caught by
	// mutation testing: pointing Publish at the default channel while
	// leaving the subscribe namespaced left every assertion above green,
	// because a local subscriber is served by this bus's own fan-out and
	// never notices which channel the message went out on.
	//
	// Observed with RAW clients rather than through the bus, so the answer
	// cannot come from the local fan-out path.
	defaultSub := redis.NewClient(&redis.Options{Addr: mr.Addr()}).Subscribe(t.Context(), "pad:events:ws-1")
	t.Cleanup(func() { _ = defaultSub.Close() })
	nsSub := redis.NewClient(&redis.Options{Addr: mr.Addr()}).Subscribe(t.Context(), "pad:inst-b:events:ws-1")
	t.Cleanup(func() { _ = nsSub.Close() })
	// Wait for both subscriptions to be live before publishing — pub/sub
	// is at-most-once, so a message sent before they establish proves
	// nothing either way.
	if _, err := defaultSub.Receive(t.Context()); err != nil {
		t.Fatalf("premise failed: default-channel subscription not confirmed: %v", err)
	}
	if _, err := nsSub.Receive(t.Context()); err != nil {
		t.Fatalf("premise failed: namespaced-channel subscription not confirmed: %v", err)
	}

	b.Publish(Event{Type: "item.updated", WorkspaceID: "ws-1"})

	select {
	case msg := <-nsSub.Channel():
		if msg.Channel != "pad:inst-b:events:ws-1" {
			t.Errorf("received on %q, want pad:inst-b:events:ws-1", msg.Channel)
		}
	case <-time.After(3 * time.Second):
		t.Error("the namespaced bus published nothing to pad:inst-b:events:ws-1")
	}

	select {
	case msg := <-defaultSub.Channel():
		t.Errorf("the namespaced bus published to the DEFAULT channel %q — a second installation would receive it", msg.Channel)
	case <-time.After(300 * time.Millisecond):
		// Correct: nothing on the historical channel.
	}
}

// TestRedisBusDefaultKeepsHistoricalKeys is the upgrade promise for this
// keyspace: no namespace configured means byte-identical names.
func TestRedisBusDefaultKeepsHistoricalKeys(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)

	ch := b.Subscribe("ws-1")
	defer b.Unsubscribe(ch)

	b.Publish(Event{Type: "item.created", WorkspaceID: "ws-1"})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !mr.Exists("pad:event_seq") {
		time.Sleep(2 * time.Millisecond)
	}
	if !mr.Exists("pad:event_seq") {
		t.Errorf("default bus did not write pad:event_seq; keys present: %v", mr.Keys())
	}
	waitForSubscribers(t, mr, "pad:events:ws-1", true)
}

// waitForSubscribers polls until the channel has (or provably lacks)
// subscribers.
//
// POLLING, not a single check, because go-redis establishes a
// subscription ASYNCHRONOUSLY: Subscribe returns before the SUBSCRIBE
// reaches the server, so an immediate assertion races the connection.
// That race made TestRedisBusDefaultKeepsHistoricalKeys fail once in a
// full-suite run — a flake in the test, not the code, and the kind that
// would have been blamed on the next unrelated change.
//
// The "probe" payload is not valid JSON, so a bus that IS subscribed logs
// an unmarshal error when it arrives. That noise is the confirmation the
// assertion wants; it is deliberately not an event any consumer sees.
func waitForSubscribers(t *testing.T, mr *miniredis.Miniredis, channel string, want bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		n := mr.Publish(channel, "probe")
		if (n > 0) == want {
			return
		}
		if time.Now().After(deadline) {
			if want {
				t.Fatalf("nothing subscribed to %s within the deadline", channel)
			}
			t.Fatalf("%s has %d subscribers, want none", channel, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
