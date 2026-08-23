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

	ch, _ := b.Subscribe()
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

// TestMemoryBus_PublishDeliversPushWithTargetUserID covers IDEA-2544
// Phase 1: KindPush and TargetUserID must round-trip through Publish
// like any other kind/field — the bus itself is kind-agnostic, but this
// pins that a new kind + a new addressed-to field don't need any bus
// changes to work, matching the plan's "zero formatter/bus changes"
// claim for a new kind.
func TestMemoryBus_PublishDeliversPushWithTargetUserID(t *testing.T) {
	t.Parallel()
	b := New()
	defer b.Close()

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	b.Publish(Notification{
		WorkspaceID:  "ws1",
		ItemID:       "item1",
		Kind:         KindPush,
		Summary:      "triage this",
		TargetUserID: "user-1",
	})

	select {
	case n := <-ch:
		if n.Kind != KindPush {
			t.Fatalf("expected kind %q, got %q", KindPush, n.Kind)
		}
		if n.TargetUserID != "user-1" {
			t.Fatalf("expected TargetUserID %q, got %q", "user-1", n.TargetUserID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for notification")
	}
}

func TestMemoryBus_UnsubscribeClosesChannel(t *testing.T) {
	t.Parallel()
	b := New()
	defer b.Close()

	ch, _ := b.Subscribe()
	b.Unsubscribe(ch)

	if _, ok := <-ch; ok {
		t.Fatal("expected channel to be closed after Unsubscribe")
	}
}

func TestMemoryBus_MultipleSubscribersAllReceive(t *testing.T) {
	t.Parallel()
	b := New()
	defer b.Close()

	ch1, _ := b.Subscribe()
	ch2, _ := b.Subscribe()
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

	// TASK-1's id, read back rather than spelled out. A literal 1 would be
	// refused by the incarnation guard (BUG-2736) before eviction was ever
	// consulted, leaving this test green with the branch it names deleted.
	evicted := b.base + 1
	if survivors := b.EventsSince(0); len(survivors) != 2 || survivors[0].ID != evicted+1 {
		t.Fatalf("fixture: expected TASK-2 and TASK-3 to survive with TASK-1 (%d) evicted, got %+v", evicted, survivors)
	}

	got := b.EventsSince(evicted)
	if got != nil {
		t.Fatalf("expected nil (gap too large), got %+v", got)
	}
}

func TestMemoryBus_SlowSubscriberDoesNotBlockPublish(t *testing.T) {
	t.Parallel()
	b := New()
	defer b.Close()

	ch, _ := b.Subscribe() // never drained
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
	// IDs must be a dense, unique run of n — every concurrent Publish got its
	// own sequence number with no duplicates and no gaps. Anchored on the
	// first ID the bus actually issued rather than on 1: since BUG-2736 the
	// counter starts from this incarnation's base (see internal/idspace), so
	// a literal 1 here would be asserting the process start time.
	first := all[0].ID
	seenIDs := make(map[int64]bool, n)
	for _, e := range all {
		seenIDs[e.ID] = true
	}
	for id := first; id < first+n; id++ {
		if !seenIDs[id] {
			t.Fatalf("missing sequence ID %d (run of %d from %d) after %d concurrent publishes", id, n, first, n)
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

	ch, missed, _ := b.SubscribeAndReplaySince(sinceID)
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

// TestMemoryBus_ConcurrentPublishUnsubscribeClose_NoPanic covers codex
// round 2 finding 3: Publish used to snapshot subscriber channels under
// the lock, release it, and send afterward — a window where a
// concurrent Unsubscribe or Close could close a channel Publish was
// about to send to. A send on a closed channel PANICS in Go, which
// would have crashed the whole padd process (not just dropped one
// subscriber's message), and the timing-dependent nature of the race
// means -race alone does not reliably surface it — this test hammers
// concurrent publish / subscribe / unsubscribe / close across many
// iterations and short-lived channels specifically to provoke it, with
// a recover() so a regression fails this test cleanly instead of
// crashing the whole `go test` run.
func TestMemoryBus_ConcurrentPublishUnsubscribeClose_NoPanic(t *testing.T) {
	t.Parallel()

	for iter := 0; iter < 30; iter++ {
		b := New()

		const numChannels = 20
		chs := make([]chan Notification, numChannels)
		for i := range chs {
			chs[i], _ = b.Subscribe()
		}

		var wg sync.WaitGroup
		wg.Add(3)

		// Publisher: hammers Publish continuously while channels are
		// being torn down concurrently below.
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Publish panicked (iter %d): %v", iter, r)
				}
			}()
			for i := 0; i < 200; i++ {
				b.Publish(Notification{Kind: KindComment})
			}
		}()

		// Unsubscriber: closes every channel while Publish is mid-flight.
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Unsubscribe panicked (iter %d): %v", iter, r)
				}
			}()
			for _, ch := range chs {
				b.Unsubscribe(ch)
			}
		}()

		// Churner: subscribes and immediately unsubscribes fresh
		// channels throughout, maximizing the window a stale Publish
		// snapshot (under the old code) could race against.
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("churn panicked (iter %d): %v", iter, r)
				}
			}()
			for i := 0; i < 200; i++ {
				ch, _ := b.Subscribe()
				b.Unsubscribe(ch)
			}
		}()

		// Drain the original channels concurrently so Publish's
		// non-blocking sends don't just fall through to "channel full"
		// for the whole run (irrelevant to the race being tested, but
		// keeps the scenario realistic).
		var drainWg sync.WaitGroup
		for _, ch := range chs {
			drainWg.Add(1)
			go func(ch chan Notification) {
				defer drainWg.Done()
				for range ch {
				}
			}(ch)
		}

		wg.Wait()
		b.Close()
		drainWg.Wait()
	}
}
