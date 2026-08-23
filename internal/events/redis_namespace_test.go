package events

import (
	"strings"
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

	b := NewRedisBusWithKeys(client, keys, false)
	t.Cleanup(b.Close)

	// A local subscriber is what starts the Redis-side subscription for
	// this workspace, so the channel assertions below have something to
	// observe.
	ch, _ := b.Subscribe("ws-1")
	defer b.Unsubscribe(ch)
	// This test only reads KEYS, which the publish writes whether or not
	// anyone is listening — so it does not NEED the wait. It is here because
	// this is one of the two shortest Redis-bus tests in the package and
	// therefore what the next person copies; leaving Subscribe followed
	// straight by Publish teaches the race (BUG-2742, codex round 10).
	waitForSubscribers(t, mr, "pad:inst-b:events:ws-1", true)

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

	// BUG-2736 added three more key families to this keyspace (the epoch, its
	// generation counter, and the per-publish dedupe tokens), and a key that
	// misses the namespace is exactly the cross-feed BUG-2724 exists to stop —
	// the epoch especially, since two installations sharing one epoch key
	// would each read the other's ID-space changes as their own and drop their
	// buffers.
	// Driven through the flipped publish path, because that is the only path
	// that writes them.
	flipped := NewRedisBusWithKeys(client, keys, true)
	t.Cleanup(flipped.Close)
	flipped.Publish(Event{Type: "item.created", WorkspaceID: "ws-1"})

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !mr.Exists("pad:inst-b:event_epoch") {
		time.Sleep(2 * time.Millisecond)
	}
	if !mr.Exists("pad:inst-b:event_epoch") {
		t.Errorf("namespaced epoch pad:inst-b:event_epoch does not exist; keys present: %v", mr.Keys())
	}
	if mr.Exists("pad:event_epoch") {
		t.Errorf("the namespaced bus also wrote the DEFAULT epoch pad:event_epoch")
	}
	if !mr.Exists("pad:inst-b:event_epoch_gen") {
		t.Errorf("namespaced epoch generation pad:inst-b:event_epoch_gen does not exist; keys present: %v", mr.Keys())
	}
	if mr.Exists("pad:event_epoch_gen") {
		t.Errorf("the namespaced bus also wrote the DEFAULT generation counter pad:event_epoch_gen")
	}
	// The dedupe token carries a random suffix, so it is matched by prefix.
	var namespacedDedupe, defaultDedupe int
	for _, k := range mr.Keys() {
		switch {
		case strings.HasPrefix(k, "pad:inst-b:events:pub:"):
			namespacedDedupe++
		case strings.HasPrefix(k, "pad:events:pub:"):
			defaultDedupe++
		}
	}
	if namespacedDedupe == 0 {
		t.Errorf("no namespaced dedupe token was written; keys present: %v", mr.Keys())
	}
	if defaultDedupe != 0 {
		t.Errorf("the namespaced bus wrote %d dedupe tokens under the DEFAULT prefix", defaultDedupe)
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

	ch, _ := b.Subscribe("ws-1")
	defer b.Unsubscribe(ch)
	// Not needed for the key assertions below; present so the pattern a
	// reader copies from here is the safe one. See the twin above.
	waitForSubscribers(t, mr, "pad:events:ws-1", true)

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
// CALL IT AFTER Subscribe AND BEFORE THE FIRST Publish, on any test that
// expects an event to ARRIVE. A publish issued before the subscription is
// registered is lost outright — not delayed — so a test without this wait
// fails on a loaded runner roughly once in a hundred runs, with a message
// about an event that never came and no bound that can rescue it. That is
// BUG-2742, four CI failures across three pull requests.
//
// Not needed by a test that only asserts on Redis KEYS: the publish writes
// those whether or not anyone is listening.
//
// POLLING, not a single check, because a subscription is not registered by
// the time Subscribe returns. go-redis DOES write the SUBSCRIBE command
// synchronously — Client.Subscribe calls PubSub.Subscribe, which writes it —
// but it does not wait for the server to confirm it, and anything the test
// does next travels on a different connection. So an immediate assertion
// races the server's processing of that command.
//
// That race made TestRedisBusDefaultKeepsHistoricalKeys fail once in a
// full-suite run — a flake in the test, not the code, and the kind that
// would have been blamed on the next unrelated change.
func waitForSubscribers(t *testing.T, mr *miniredis.Miniredis, channel string, want bool) {
	t.Helper()
	n, ok := pollSubscriberCount(mr, channel, func(n int) bool { return (n > 0) == want })
	if ok {
		return
	}
	if want {
		t.Fatalf("nothing subscribed to %s within the deadline", channel)
	}
	t.Fatalf("%s has %d subscribers, want none", channel, n)
}

// waitForSubscriberCount is waitForSubscribers when the number matters —
// two replicas on one Redis, where "someone is listening" is satisfied by the
// first of them and the test needs both.
func waitForSubscriberCount(t *testing.T, mr *miniredis.Miniredis, channel string, want int) {
	t.Helper()
	if n, ok := pollSubscriberCount(mr, channel, func(n int) bool { return n >= want }); !ok {
		t.Fatalf("%s has %d subscribers, want at least %d, within the deadline", channel, n, want)
	}
}

// pollSubscriberCount READS the server's registration state rather than
// publishing a probe to infer it.
//
// The probe version worked and was a trap for the next person to reuse it:
// "probe" is not a decodable payload, so it is an UNDECODABLE MESSAGE — the
// exact condition several tests in this package assert about, injected by the
// helper they would call to set themselves up. An undecodable message ends
// this instance's coverage of the workspace.
//
// It was harmless in every EXISTING caller, but not for the reason I first
// wrote here (codex round 7 corrected it): four of the callers do run after a
// publish, so the old helper really was ending coverage on a live buffer. It
// did not matter because those four go on to assert about Redis KEYS rather
// than about coverage or delivery. That is a property of what each call site
// happens to assert next — the thinnest possible guarantee, and one that stops
// holding the moment somebody adds a coverage assertion after a wait, or
// copies the helper into a test that has one.
//
// The deadline is a LIVENESS bound, not a latency assertion: registration is
// a sub-millisecond-to-milliseconds affair, and this number exists so a test
// that will never see a subscriber fails instead of hanging.
//
// WHAT WAITING HERE DELIBERATELY STOPS TESTING, so that nobody re-discovers
// it as a gap: every caller of this helper is no longer exercising the window
// between subscribing and being registered, and a publish inside that window
// is lost for good. That window is REAL IN PRODUCTION, where nothing waits —
// see BUG-2747, which also records that internal/watchevents closes the same
// window in its constructor and this bus does not. It is tracked there rather
// than by leaving tests that fail on a loaded runner roughly one time in a
// hundred, which is a defect detector nobody can act on.
func pollSubscriberCount(mr *miniredis.Miniredis, channel string, satisfied func(int) bool) (int, bool) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		n := mr.PubSubNumSub(channel)[channel]
		if satisfied(n) {
			return n, true
		}
		if time.Now().After(deadline) {
			return n, false
		}
		time.Sleep(time.Millisecond)
	}
}
