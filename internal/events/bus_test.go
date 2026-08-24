package events

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSubscribeAndPublish(t *testing.T) {
	bus := New()

	ch, _, _ := bus.Subscribe(context.Background(), "ws-1")
	defer bus.Unsubscribe(ch)

	bus.Publish(Event{
		Type:        DocumentCreated,
		WorkspaceID: "ws-1",
		DocumentID:  "doc-1",
		Title:       "Test Doc",
	})

	select {
	case event := <-ch:
		if event.Type != DocumentCreated {
			t.Errorf("expected type %q, got %q", DocumentCreated, event.Type)
		}
		if event.DocumentID != "doc-1" {
			t.Errorf("expected doc ID %q, got %q", "doc-1", event.DocumentID)
		}
		if event.Timestamp == 0 {
			t.Error("expected timestamp to be set")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestWorkspaceIsolation(t *testing.T) {
	bus := New()

	ch1, _, _ := bus.Subscribe(context.Background(), "ws-1")
	ch2, _, _ := bus.Subscribe(context.Background(), "ws-2")
	defer bus.Unsubscribe(ch1)
	defer bus.Unsubscribe(ch2)

	bus.Publish(Event{
		Type:        DocumentUpdated,
		WorkspaceID: "ws-1",
		DocumentID:  "doc-1",
	})

	// ch1 should receive the event
	select {
	case <-ch1:
		// good
	case <-time.After(time.Second):
		t.Fatal("ch1 should have received event")
	}

	// ch2 should NOT receive it
	select {
	case <-ch2:
		t.Fatal("ch2 should not have received event for ws-1")
	case <-time.After(50 * time.Millisecond):
		// good
	}
}

func TestMultipleSubscribers(t *testing.T) {
	bus := New()

	ch1, _, _ := bus.Subscribe(context.Background(), "ws-1")
	ch2, _, _ := bus.Subscribe(context.Background(), "ws-1")
	ch3, _, _ := bus.Subscribe(context.Background(), "ws-1")
	defer bus.Unsubscribe(ch1)
	defer bus.Unsubscribe(ch2)
	defer bus.Unsubscribe(ch3)

	bus.Publish(Event{
		Type:        DocumentCreated,
		WorkspaceID: "ws-1",
	})

	for i, ch := range []chan Event{ch1, ch2, ch3} {
		select {
		case <-ch:
			// good
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d didn't receive event", i)
		}
	}
}

func TestUnsubscribe(t *testing.T) {
	bus := New()

	ch, _, _ := bus.Subscribe(context.Background(), "ws-1")
	if bus.SubscriberCount() != 1 {
		t.Fatalf("expected 1 subscriber, got %d", bus.SubscriberCount())
	}

	bus.Unsubscribe(ch)
	if bus.SubscriberCount() != 0 {
		t.Fatalf("expected 0 subscribers, got %d", bus.SubscriberCount())
	}

	// Channel should be closed
	_, ok := <-ch
	if ok {
		t.Fatal("expected channel to be closed")
	}
}

func TestUnsubscribeIdempotent(t *testing.T) {
	bus := New()

	ch, _, _ := bus.Subscribe(context.Background(), "ws-1")
	bus.Unsubscribe(ch)
	// Second unsubscribe should not panic
	bus.Unsubscribe(ch)
}

func TestSlowConsumerDropsEvents(t *testing.T) {
	bus := New()

	ch, _, _ := bus.Subscribe(context.Background(), "ws-1")
	defer bus.Unsubscribe(ch)

	// Overfill the channel buffer, so the drop path runs.
	for i := 0; i < subscriberChanDepth+36; i++ {
		bus.Publish(Event{
			Type:        DocumentUpdated,
			WorkspaceID: "ws-1",
		})
	}

	// Should have exactly a buffer's worth; the rest dropped.
	count := 0
	for {
		select {
		case <-ch:
			count++
		default:
			goto done
		}
	}
done:
	if count != subscriberChanDepth {
		t.Fatalf("expected %d buffered events, got %d", subscriberChanDepth, count)
	}
}

func TestTimestampAutoSet(t *testing.T) {
	bus := New()

	ch, _, _ := bus.Subscribe(context.Background(), "ws-1")
	defer bus.Unsubscribe(ch)

	before := time.Now().UnixMilli()
	bus.Publish(Event{
		Type:        DocumentCreated,
		WorkspaceID: "ws-1",
	})

	event := <-ch
	if event.Timestamp < before {
		t.Error("expected timestamp to be >= publish time")
	}
}

func TestTimestampPreserved(t *testing.T) {
	bus := New()

	ch, _, _ := bus.Subscribe(context.Background(), "ws-1")
	defer bus.Unsubscribe(ch)

	ts := int64(1234567890)
	bus.Publish(Event{
		Type:        DocumentCreated,
		WorkspaceID: "ws-1",
		Timestamp:   ts,
	})

	event := <-ch
	if event.Timestamp != ts {
		t.Errorf("expected timestamp %d, got %d", ts, event.Timestamp)
	}
}

func TestConcurrentAccess(t *testing.T) {
	bus := New()
	var wg sync.WaitGroup

	// Spawn subscribers concurrently
	channels := make([]chan Event, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			channels[idx], _, _ = bus.Subscribe(context.Background(), "ws-1")
		}(i)
	}
	wg.Wait()

	// Publish concurrently
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bus.Publish(Event{
				Type:        DocumentUpdated,
				WorkspaceID: "ws-1",
			})
		}()
	}
	wg.Wait()

	// Unsubscribe concurrently
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			bus.Unsubscribe(channels[idx])
		}(i)
	}
	wg.Wait()

	if bus.SubscriberCount() != 0 {
		t.Fatalf("expected 0 subscribers after cleanup, got %d", bus.SubscriberCount())
	}
}

func TestWorkspaceSubscriberCount(t *testing.T) {
	bus := New()

	// No subscribers initially
	if got := bus.WorkspaceSubscriberCount("ws-1"); got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}

	// Subscribe to ws-1
	ch1, _, _ := bus.Subscribe(context.Background(), "ws-1")
	ch2, _, _ := bus.Subscribe(context.Background(), "ws-1")
	ch3, _, _ := bus.Subscribe(context.Background(), "ws-2")

	if got := bus.WorkspaceSubscriberCount("ws-1"); got != 2 {
		t.Fatalf("expected 2 for ws-1, got %d", got)
	}
	if got := bus.WorkspaceSubscriberCount("ws-2"); got != 1 {
		t.Fatalf("expected 1 for ws-2, got %d", got)
	}
	if got := bus.WorkspaceSubscriberCount("ws-3"); got != 0 {
		t.Fatalf("expected 0 for ws-3, got %d", got)
	}

	// Unsubscribe one from ws-1
	bus.Unsubscribe(ch1)
	if got := bus.WorkspaceSubscriberCount("ws-1"); got != 1 {
		t.Fatalf("expected 1 for ws-1 after unsubscribe, got %d", got)
	}

	// Unsubscribe remaining
	bus.Unsubscribe(ch2)
	bus.Unsubscribe(ch3)
	if got := bus.WorkspaceSubscriberCount("ws-1"); got != 0 {
		t.Fatalf("expected 0 for ws-1 after all unsubscribed, got %d", got)
	}
}

func TestPublishNoSubscribers(t *testing.T) {
	bus := New()
	// Should not panic
	bus.Publish(Event{
		Type:        DocumentCreated,
		WorkspaceID: "ws-1",
	})
}

func TestEventIDsAreMonotonic(t *testing.T) {
	bus := New()
	ch, _, _ := bus.Subscribe(context.Background(), "ws-1")
	defer bus.Unsubscribe(ch)

	for i := 0; i < 10; i++ {
		bus.Publish(Event{
			Type:        ItemUpdated,
			WorkspaceID: "ws-1",
		})
	}

	var lastID int64
	for i := 0; i < 10; i++ {
		event := <-ch
		if event.ID <= lastID {
			t.Fatalf("event %d: ID %d not greater than previous %d", i, event.ID, lastID)
		}
		lastID = event.ID
	}
}

func TestEventsSinceCaughtUp(t *testing.T) {
	bus := New()

	ids := publishTyped(bus, "ws-1", ItemCreated, ItemUpdated, ItemUpdated)

	// Ask for events since the last one — should get empty slice
	events := bus.EventsSince("ws-1", ids[2])
	if events == nil {
		t.Fatal("expected non-nil slice")
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}
}

func TestEventsSinceReplay(t *testing.T) {
	bus := New()

	ids := publishTyped(bus, "ws-1", ItemCreated, ItemUpdated, ItemArchived)

	// Ask for events since the first — should get the second and third
	events := bus.EventsSince("ws-1", ids[0])
	if events == nil {
		t.Fatal("expected non-nil slice")
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Type != ItemUpdated {
		t.Errorf("expected ItemUpdated, got %q", events[0].Type)
	}
	if events[1].Type != ItemArchived {
		t.Errorf("expected ItemArchived, got %q", events[1].Type)
	}
}

func TestEventsSinceGapTooLarge(t *testing.T) {
	// Create a tiny buffer so we can overflow it
	bus := NewWithReplay(3, 5*time.Minute)

	// Publish 5 events (buffer only holds 3)
	ids := publishN(bus, "ws-1", 5)

	// The oldest buffered event is now ids[2]; ids[0] and ids[1] were evicted,
	// so a resume from ids[0] cannot be answered completely.
	events := bus.EventsSince("ws-1", ids[0])
	if events != nil {
		t.Fatalf("expected nil (gap too large), got %d events", len(events))
	}

	// Asking from the oldest event still buffered should work
	events = bus.EventsSince("ws-1", ids[2])
	if events == nil {
		t.Fatal("expected non-nil slice")
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events (ids %v), got %d", ids[3:], len(events))
	}
}

func TestEventsSinceWorkspaceIsolation(t *testing.T) {
	bus := New()

	bus.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	bus.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-2"})
	bus.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})

	// ws-2 events since 0 should only return the ws-2 event
	events := bus.EventsSince("ws-2", 0)
	if len(events) != 1 {
		t.Fatalf("expected 1 event for ws-2, got %d", len(events))
	}
	if events[0].WorkspaceID != "ws-2" {
		t.Errorf("expected ws-2, got %s", events[0].WorkspaceID)
	}
}

func TestEventsSinceNoEventsForWorkspace(t *testing.T) {
	bus := New()

	events := bus.EventsSince("ws-nonexistent", 0)
	if events == nil {
		t.Fatal("expected non-nil empty slice")
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}
}

func TestEventsSinceForeignID(t *testing.T) {
	// Simulates the multi-instance Redis scenario: a client sends a
	// Last-Event-ID from a different instance whose IDs are in a different range.
	bus := New()

	ids := publishTyped(bus, "ws-1", ItemCreated, ItemUpdated)

	// A cursor ABOVE our newest belongs to a sequence we are not part of.
	// Stated relative to what we issued: since BUG-2736 an absolute constant
	// like 500 sits BELOW this incarnation's base, so it would be refused by
	// the coverage check instead — the same nil for a different reason, which
	// is a test passing while proving nothing about the branch it names.
	events := bus.EventsSince("ws-1", ids[1]+500)
	if events != nil {
		t.Fatalf("expected nil (foreign ID), got %d events", len(events))
	}
}

func TestReplayBufferWrapAround(t *testing.T) {
	bus := NewWithReplay(4, 5*time.Minute)

	// Fill the buffer exactly, then overflow it by 2.
	ids := publishTyped(bus, "ws-1",
		ItemUpdated, ItemUpdated, ItemUpdated, ItemUpdated,
		ItemCreated, ItemArchived)

	// The buffer now holds ids[2..5]; ids[0] and ids[1] were evicted.
	events := bus.EventsSince("ws-1", ids[2])
	if events == nil {
		t.Fatalf("expected non-nil slice for a cursor at the oldest buffered id %d", ids[2])
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events (ids %v), got %d", ids[3:], len(events))
	}

	// A cursor at an EVICTED id is a gap: its successor is gone.
	events = bus.EventsSince("ws-1", ids[1])
	if events != nil {
		t.Fatalf("expected nil (gap) for evicted id %d, got %d events", ids[1], len(events))
	}

	events = bus.EventsSince("ws-1", ids[0])
	if events != nil {
		t.Fatalf("expected nil (gap) for evicted id %d, got %d events", ids[0], len(events))
	}
}
