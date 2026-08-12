package watchevents

import (
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
