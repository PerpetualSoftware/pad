package events

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// codex round 9 #1, ARRANGEMENT CORRECTED after the split. Publish lost its
// local-counter fallback when the ID assignment fails, and the removal is the
// fix — the fallback minted an ID from a process-local space and published it,
// which every receiving instance reads as the counter having been reset.
//
// The first version of this test killed Redis outright, and after the split it
// no longer DISCRIMINATED: with Redis down the fallback's publish fails too, so
// "returns early" and "continues and fails at PUBLISH" look identical from the
// outside. Found by re-running the mutation battery on the split tree, which is
// the point of re-running it — a subtraction hides its defects as absences.
//
// The arrangement now reproduces a PARTIAL failure of the kind the branch
// exists for: the sequence key holds a non-integer, so INCR fails while PUBLISH
// keeps working. A key-type collision with another tenant of the same Redis is
// one realistic route to that state and the cheapest to arrange here; an ACL
// permitting PUBLISH while denying INCR is another. What matters is the shape —
// ID assignment failing while delivery still works — because that is the only
// shape in which a fallback ID would actually reach subscribers.
func TestAFailedIDAssignmentPublishesNothing(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	b := NewRedisBus(client)
	t.Cleanup(b.Close)

	ch := b.Subscribe("ws-1")
	defer b.Unsubscribe(ch)
	waitForSubscribers(t, mr, "pad:events:ws-1", true)

	// Positive control: publishing works before the key is poisoned, so a
	// silent test below cannot be the bus never publishing at all.
	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	drain(t, ch, 1)

	// INCR now fails; PUBLISH still works.
	mr.Set("pad:event_seq", "not-a-number")

	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})

	select {
	case e := <-ch:
		t.Fatalf("a publish whose ID assignment failed must deliver nothing; got an event with id %d "+
			"(an ID from a process-local counter is exactly the defect the fallback's removal fixed)", e.ID)
	case <-time.After(300 * time.Millisecond):
	}

	// The replay buffer must not have grown from it either — here the buffer
	// IS valid evidence, because the subscription is healthy throughout.
	held := b.EventsSince("ws-1", 0)
	if len(held) != 1 {
		t.Fatalf("the failed publish must not reach the replay buffer; buffer holds %d events", len(held))
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
