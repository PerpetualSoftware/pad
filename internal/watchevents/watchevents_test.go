package watchevents

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestMemoryBus_PublishDeliversToSubscriber(t *testing.T) {
	t.Parallel()
	b := New()
	defer b.Close()

	ch := b.Subscribe()
	defer b.Unsubscribe(ch)

	b.Publish(Notification{WorkspaceID: "ws1", ItemID: "item1", Kind: KindStatusChange})

	select {
	case n := <-ch:
		if n.WorkspaceID != "ws1" || n.Kind != KindStatusChange {
			t.Fatalf("unexpected notification: %+v", n)
		}
		if n.ID == 0 {
			t.Fatalf("expected a non-zero assigned ID")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for notification")
	}
}

func TestMemoryBus_UnsubscribeClosesChannel(t *testing.T) {
	t.Parallel()
	b := New()
	defer b.Close()

	ch := b.Subscribe()
	b.Unsubscribe(ch)

	if _, ok := <-ch; ok {
		t.Fatal("expected channel to be closed after Unsubscribe")
	}
}

func TestMemoryBus_MultipleSubscribersAllReceive(t *testing.T) {
	t.Parallel()
	b := New()
	defer b.Close()

	ch1 := b.Subscribe()
	ch2 := b.Subscribe()
	defer b.Unsubscribe(ch1)
	defer b.Unsubscribe(ch2)

	b.Publish(Notification{Kind: KindComment})

	for _, ch := range []chan Notification{ch1, ch2} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for notification on one of the subscribers")
		}
	}
}

func TestMemoryBus_EventsSince_ReplaysNewerOnly(t *testing.T) {
	t.Parallel()
	b := New()
	defer b.Close()

	b.Publish(Notification{Kind: KindStatusChange, ItemRef: "TASK-1"})
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-2"})
	b.Publish(Notification{Kind: KindAssignment, ItemRef: "TASK-3"})

	all := b.EventsSince(0)
	if len(all) != 3 {
		t.Fatalf("expected 3 events since 0, got %d", len(all))
	}

	sinceFirst := b.EventsSince(all[0].ID)
	if len(sinceFirst) != 2 {
		t.Fatalf("expected 2 events since the first, got %d", len(sinceFirst))
	}
	if sinceFirst[0].ItemRef != "TASK-2" || sinceFirst[1].ItemRef != "TASK-3" {
		t.Fatalf("unexpected replay order: %+v", sinceFirst)
	}
}

func TestMemoryBus_EventsSince_CaughtUpReturnsEmptyNotNil(t *testing.T) {
	t.Parallel()
	b := New()
	defer b.Close()

	b.Publish(Notification{Kind: KindComment})
	all := b.EventsSince(0)
	latest := all[len(all)-1].ID

	got := b.EventsSince(latest)
	if got == nil {
		t.Fatal("expected an empty (non-nil) slice, got nil (nil means 'gap, resync')")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 events, got %d", len(got))
	}
}

func TestMemoryBus_EventsSince_EvictedGapReturnsNil(t *testing.T) {
	t.Parallel()
	b := NewWithReplaySize(2)
	defer b.Close()

	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-2"})
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-3"}) // evicts TASK-1's slot

	got := b.EventsSince(1) // TASK-1's ID, now evicted
	if got != nil {
		t.Fatalf("expected nil (gap too large), got %+v", got)
	}
}

func TestMemoryBus_SlowSubscriberDoesNotBlockPublish(t *testing.T) {
	t.Parallel()
	b := New()
	defer b.Close()

	ch := b.Subscribe() // never drained
	defer b.Unsubscribe(ch)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			b.Publish(Notification{Kind: KindComment})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full subscriber channel")
	}
}

// TestMemoryBus_ConcurrentPublish_MaintainsIDOrderInReplayBuffer covers
// codex round 1 finding 4: sequence assignment and replay-buffer
// insertion used to happen under SEPARATE locks, so two concurrent
// Publish calls could append to the ring buffer out of ID order —
// corrupting since()'s ordering assumptions (it walks the ring
// oldest→newest and compares IDs, assuming monotonic order). Run with
// -race; this also catches any residual lock-ordering issue in the
// unified-lock fix.
func TestMemoryBus_ConcurrentPublish_MaintainsIDOrderInReplayBuffer(t *testing.T) {
	t.Parallel()
	b := New()
	defer b.Close()

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b.Publish(Notification{Kind: KindComment, ItemRef: fmt.Sprintf("TASK-%d", i)})
		}(i)
	}
	wg.Wait()

	all := b.EventsSince(0)
	if len(all) != n {
		t.Fatalf("expected %d buffered events, got %d", n, len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatalf("replay buffer out of ID order at index %d: %d then %d", i, all[i-1].ID, all[i].ID)
		}
	}
	// IDs must be a dense, unique 1..n set — every concurrent Publish
	// got its own sequence number with no duplicates and no gaps.
	seenIDs := make(map[int64]bool, n)
	for _, e := range all {
		seenIDs[e.ID] = true
	}
	for id := int64(1); id <= n; id++ {
		if !seenIDs[id] {
			t.Fatalf("missing sequence ID %d after %d concurrent publishes", id, n)
		}
	}
}

// TestMemoryBus_SubscribeAndReplaySince_NoDuplicateUnderConcurrentPublish
// covers codex round 1 finding 3: Subscribe() and EventsSince() used to
// be two separate calls, leaving a window where a Notification published
// in between would be delivered TWICE (once via replay, once via the
// live channel). Publishes a batch of notifications immediately after
// the atomic subscribe-and-replay call — the exact timing that was
// racy under the old two-step sequence — and asserts every notification
// ID is observed EXACTLY once across {replay result, live channel}.
func TestMemoryBus_SubscribeAndReplaySince_NoDuplicateUnderConcurrentPublish(t *testing.T) {
	t.Parallel()
	b := New()
	defer b.Close()

	// Seed some history so there's a real replay window to resume into.
	for i := 0; i < 5; i++ {
		b.Publish(Notification{Kind: KindComment})
	}
	seeded := b.EventsSince(0)
	sinceID := seeded[2].ID // resume from partway through history

	ch, missed := b.SubscribeAndReplaySince(sinceID)
	defer b.Unsubscribe(ch)

	const concurrentPublishes = 20
	var wg sync.WaitGroup
	for i := 0; i < concurrentPublishes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Publish(Notification{Kind: KindStatusChange})
		}()
	}
	wg.Wait()

	seen := make(map[int64]int)
	for _, n := range missed {
		seen[n.ID]++
	}
	deadline := time.After(500 * time.Millisecond)
drain:
	for {
		select {
		case n := <-ch:
			seen[n.ID]++
		case <-deadline:
			break drain
		}
	}

	for id, count := range seen {
		if count != 1 {
			t.Errorf("notification ID %d delivered %d times, want exactly 1", id, count)
		}
	}
	// None of the 20 concurrently-published notifications should be
	// missing (they must all land on the live channel, since they were
	// published strictly after the atomic subscribe returned).
	concurrentSeen := 0
	for id := range seen {
		if id > seeded[len(seeded)-1].ID {
			concurrentSeen++
		}
	}
	if concurrentSeen != concurrentPublishes {
		t.Fatalf("expected all %d concurrently-published notifications to be observed exactly once, got %d", concurrentPublishes, concurrentSeen)
	}
}
