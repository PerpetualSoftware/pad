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

	if n := mr.Publish("pad:events:ws-1", "probe"); n != 0 {
		t.Errorf("the namespaced bus is subscribed to the DEFAULT channel pad:events:ws-1 (%d subscribers)", n)
	}
	if n := mr.Publish("pad:inst-b:events:ws-1", "probe"); n == 0 {
		t.Error("the namespaced bus is not subscribed to pad:inst-b:events:ws-1")
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
	if n := mr.Publish("pad:events:ws-1", "probe"); n == 0 {
		t.Error("default bus is not subscribed to pad:events:ws-1")
	}
}
