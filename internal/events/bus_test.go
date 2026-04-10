package events

import (
	"sync"
	"testing"
	"time"
)

func TestSubscribeAndPublish(t *testing.T) {
	bus := New()

	ch := bus.Subscribe("ws-1")
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

	ch1 := bus.Subscribe("ws-1")
	ch2 := bus.Subscribe("ws-2")
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

	ch1 := bus.Subscribe("ws-1")
	ch2 := bus.Subscribe("ws-1")
	ch3 := bus.Subscribe("ws-1")
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

	ch := bus.Subscribe("ws-1")
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

	ch := bus.Subscribe("ws-1")
	bus.Unsubscribe(ch)
	// Second unsubscribe should not panic
	bus.Unsubscribe(ch)
}

func TestSlowConsumerDropsEvents(t *testing.T) {
	bus := New()

	ch := bus.Subscribe("ws-1")
	defer bus.Unsubscribe(ch)

	// Fill the channel buffer (64 events)
	for i := 0; i < 100; i++ {
		bus.Publish(Event{
			Type:        DocumentUpdated,
			WorkspaceID: "ws-1",
		})
	}

	// Should have 64 events (buffer size), rest dropped
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
	if count != 64 {
		t.Fatalf("expected 64 buffered events, got %d", count)
	}
}

func TestTimestampAutoSet(t *testing.T) {
	bus := New()

	ch := bus.Subscribe("ws-1")
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

	ch := bus.Subscribe("ws-1")
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
			channels[idx] = bus.Subscribe("ws-1")
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
	ch1 := bus.Subscribe("ws-1")
	ch2 := bus.Subscribe("ws-1")
	ch3 := bus.Subscribe("ws-2")

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
	ch := bus.Subscribe("ws-1")
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

	bus.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	bus.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	bus.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})

	// Ask for events since the last one — should get empty slice
	events := bus.EventsSince("ws-1", 3)
	if events == nil {
		t.Fatal("expected non-nil slice")
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}
}

func TestEventsSinceReplay(t *testing.T) {
	bus := New()

	bus.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	bus.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	bus.Publish(Event{Type: ItemArchived, WorkspaceID: "ws-1"})

	// Ask for events since ID 1 — should get events 2 and 3
	events := bus.EventsSince("ws-1", 1)
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
	for i := 0; i < 5; i++ {
		bus.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	}

	// Oldest buffered event should be ID 3 (events 1 and 2 are evicted)
	// Asking for events since ID 1 should return nil (gap too large)
	events := bus.EventsSince("ws-1", 1)
	if events != nil {
		t.Fatalf("expected nil (gap too large), got %d events", len(events))
	}

	// Asking for events since ID 3 should work
	events = bus.EventsSince("ws-1", 3)
	if events == nil {
		t.Fatal("expected non-nil slice")
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events (IDs 4,5), got %d", len(events))
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

	bus.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	bus.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})

	// sinceID=500 is way beyond our newest (ID 2) — foreign sequence
	events := bus.EventsSince("ws-1", 500)
	if events != nil {
		t.Fatalf("expected nil (foreign ID), got %d events", len(events))
	}
}

func TestReplayBufferWrapAround(t *testing.T) {
	bus := NewWithReplay(4, 5*time.Minute)

	// Fill buffer exactly
	for i := 0; i < 4; i++ {
		bus.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	}

	// Overflow by 2
	bus.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	bus.Publish(Event{Type: ItemArchived, WorkspaceID: "ws-1"})

	// Buffer should hold events 3,4,5,6 (1,2 evicted)
	// sinceID=3 should work: events 4,5,6
	events := bus.EventsSince("ws-1", 3)
	if events == nil {
		t.Fatal("expected non-nil slice for sinceID=3")
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events (IDs 4,5,6), got %d", len(events))
	}

	// sinceID=2 should be a gap (event 2 is evicted, oldest in buffer is 3)
	events = bus.EventsSince("ws-1", 2)
	if events != nil {
		t.Fatalf("expected nil (gap) for sinceID=2, got %d events", len(events))
	}

	// sinceID=1 should also be a gap
	events = bus.EventsSince("ws-1", 1)
	if events != nil {
		t.Fatalf("expected nil (gap) for sinceID=1, got %d events", len(events))
	}
}
