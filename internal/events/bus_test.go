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
