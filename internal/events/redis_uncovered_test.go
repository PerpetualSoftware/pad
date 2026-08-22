package events

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// codex round 9 #1. Publish lost its local-counter fallback when the script
// fails, and the removal is the fix — the fallback minted an ID from a
// different space and published it. Nothing tested the resulting branch, so
// nothing would notice a future "helpful" restoration of it.
func TestAFailedPublishDeliversNothing(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	b := NewRedisBus(client)
	t.Cleanup(b.Close)

	ch := b.Subscribe("ws-1")
	defer b.Unsubscribe(ch)
	waitForSubscribers(t, mr, "pad:events:ws-1", true)

	// Positive control: the same call succeeds while Redis is reachable, so a
	// failure below cannot be confused with the bus never publishing at all.
	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	drain(t, ch, 1)

	// Redis goes away. The script cannot run.
	mr.Close()

	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})

	select {
	case e := <-ch:
		t.Fatalf("a failed publish must deliver nothing; got an event with id %d "+
			"(a fallback id from a local counter is exactly the defect the script removed)", e.ID)
	case <-time.After(300 * time.Millisecond):
	}

	// NOT asserting anything about the replay buffer here, and the reason is
	// worth writing down: killing Redis also kills the subscription, so the
	// receive loop's error path legitimately drops this workspace's coverage
	// (BUG-2731's reconnect handling). An empty buffer at this point is that
	// mechanism working, not evidence about the publish — an assertion on it
	// would pass or fail for the wrong reason.
	//
	// A resume is a gap now, for that same reason, which is correct: we
	// genuinely cannot vouch for anything across a dead connection.
	if got := b.EventsSince("ws-1", 1); got != nil {
		t.Fatalf("with the connection dead, a resume must be a gap, got %d events", len(got))
	}
}

// codex round 9 #4. The subscriber map was re-indexed by workspace, so these
// counts and the admission check run through new code on RedisBus while every
// existing test for them uses MemoryBus.
func TestRedisBusSubscriptionAccounting(t *testing.T) {
	b := newTestRedisBus(t)

	first, ok := b.SubscribeIfAllowed("ws-1", 2)
	if !ok {
		t.Fatal("the first subscriber must be admitted")
	}
	second, ok := b.SubscribeIfAllowed("ws-1", 2)
	if !ok {
		t.Fatal("the second subscriber must be admitted")
	}
	if _, ok := b.SubscribeIfAllowed("ws-1", 2); ok {
		t.Fatal("the third subscriber must be refused at a limit of 2")
	}

	// A different workspace is accounted separately.
	other, ok := b.SubscribeIfAllowed("ws-2", 2)
	if !ok {
		t.Fatal("a different workspace must have its own budget")
	}
	defer b.Unsubscribe(other)

	if got := b.WorkspaceSubscriberCount("ws-1"); got != 2 {
		t.Fatalf("WorkspaceSubscriberCount(ws-1) = %d, want 2", got)
	}
	if got := b.SubscriberCount(); got != 3 {
		t.Fatalf("SubscriberCount() = %d, want 3", got)
	}

	// A slot is RELEASED on unsubscribe — the case a per-workspace index can
	// get wrong by deleting the wrong map entry.
	b.Unsubscribe(first)
	if got := b.WorkspaceSubscriberCount("ws-1"); got != 1 {
		t.Fatalf("after one unsubscribe, WorkspaceSubscriberCount(ws-1) = %d, want 1", got)
	}
	third, ok := b.SubscribeIfAllowed("ws-1", 2)
	if !ok {
		t.Fatal("the freed slot must be reusable")
	}
	defer b.Unsubscribe(third)
	defer b.Unsubscribe(second)

	if got := b.SubscriberCount(); got != 3 {
		t.Fatalf("SubscriberCount() = %d, want 3 after the swap", got)
	}
}

// codex round 9 #6. Close on a LIVE Redis bus: every subscriber channel closes
// so handlers exit, and the receive loops stop. Existing coverage only
// unsubscribes the last subscriber, which is a different path.
func TestClosingALiveRedisBusReleasesEverything(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	b := NewRedisBus(client)

	obs := &recordingObserver{}
	b.SetObserver(obs)

	one := b.Subscribe("ws-1")
	two := b.Subscribe("ws-2")
	waitForSubscribers(t, mr, "pad:events:ws-1", true)
	waitForSubscribers(t, mr, "pad:events:ws-2", true)

	b.Close()

	for name, ch := range map[string]chan Event{"ws-1": one, "ws-2": two} {
		select {
		case _, open := <-ch:
			if open {
				t.Fatalf("%s: expected a closed channel, got an event", name)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: Close must close subscriber channels so SSE handlers exit", name)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if obs.exits() >= 2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("both receive loops must report their exit at shutdown, got %d", obs.exits())
}

func drain(t *testing.T, ch chan Event, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for event %d of %d", i+1, n)
		}
	}
}
