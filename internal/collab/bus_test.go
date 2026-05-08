package collab

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

// TestMemoryOpBusSubscribeAndPublish verifies basic fan-out: a
// subscriber receives every event for its itemID and ignores events
// for other items.
func TestMemoryOpBusSubscribeAndPublish(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()

	chA := bus.Subscribe("item-a")
	chB := bus.Subscribe("item-b")

	bus.Publish(OpEvent{ItemID: "item-a", Type: OpTypeSync, Data: []byte{1}})
	bus.Publish(OpEvent{ItemID: "item-b", Type: OpTypeSync, Data: []byte{2}})
	bus.Publish(OpEvent{ItemID: "item-a", Type: OpTypeAwareness, Data: []byte{3}})

	got := drain(chA, 2)
	if len(got) != 2 {
		t.Fatalf("subA: want 2 events, got %d", len(got))
	}
	if got[0].Type != OpTypeSync || !bytes.Equal(got[0].Data, []byte{1}) {
		t.Errorf("subA[0] mismatch: %+v", got[0])
	}
	if got[1].Type != OpTypeAwareness || !bytes.Equal(got[1].Data, []byte{3}) {
		t.Errorf("subA[1] mismatch: %+v", got[1])
	}
	if got[0].Timestamp == 0 {
		t.Errorf("Timestamp should be auto-stamped on Publish")
	}

	gotB := drain(chB, 1)
	if len(gotB) != 1 || !bytes.Equal(gotB[0].Data, []byte{2}) {
		t.Fatalf("subB: want 1 event with data [2], got %+v", gotB)
	}
}

// TestMemoryOpBusUnsubscribeClosesChannel confirms Unsubscribe removes
// the subscriber AND closes its channel, so handler `for ev := range
// ch { ... }` loops exit cleanly.
func TestMemoryOpBusUnsubscribeClosesChannel(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()

	ch := bus.Subscribe("item-a")
	if bus.SubscriberCount("item-a") != 1 {
		t.Fatalf("want 1 subscriber, got %d", bus.SubscriberCount("item-a"))
	}

	bus.Unsubscribe(ch)
	if bus.SubscriberCount("item-a") != 0 {
		t.Fatalf("want 0 subscribers after Unsubscribe, got %d", bus.SubscriberCount("item-a"))
	}

	// Reading from a closed channel returns the zero value with ok=false.
	if _, ok := <-ch; ok {
		t.Errorf("channel should be closed after Unsubscribe")
	}

	// Idempotent: a second Unsubscribe is a no-op (no panic).
	bus.Unsubscribe(ch)
}

// TestMemoryOpBusSlowSubscriberDrops verifies that a subscriber whose
// channel buffer is full has events dropped instead of blocking the
// publisher. Without this, one stuck consumer would back-pressure the
// entire collab room.
func TestMemoryOpBusSlowSubscriberDrops(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()

	ch := bus.Subscribe("item-a")

	// Fill the buffer (default 64) without draining.
	for i := 0; i < 64; i++ {
		bus.Publish(OpEvent{ItemID: "item-a", Type: OpTypeSync, Data: []byte{byte(i)}})
	}

	// One more Publish — this MUST NOT block. We exercise the drop path
	// by giving Publish a hard wall-clock budget; without the
	// non-blocking select, this would hang the test.
	done := make(chan struct{})
	go func() {
		bus.Publish(OpEvent{ItemID: "item-a", Type: OpTypeSync, Data: []byte{0xff}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber instead of dropping")
	}

	// Confirm the buffer still has the original 64 entries (the dropped
	// 65th is ABSENT — drop path is silent at the data level, only logs).
	got := drain(ch, 64)
	if len(got) != 64 {
		t.Fatalf("want 64 events drained, got %d", len(got))
	}
}

// TestMemoryOpBusSubscriberCount tracks the count across subscribe
// and unsubscribe.
func TestMemoryOpBusSubscriberCount(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()

	if c := bus.SubscriberCount("item-a"); c != 0 {
		t.Errorf("empty bus: want 0, got %d", c)
	}

	chA1 := bus.Subscribe("item-a")
	chA2 := bus.Subscribe("item-a")
	chB := bus.Subscribe("item-b")

	if c := bus.SubscriberCount("item-a"); c != 2 {
		t.Errorf("two on item-a: want 2, got %d", c)
	}
	if c := bus.SubscriberCount("item-b"); c != 1 {
		t.Errorf("one on item-b: want 1, got %d", c)
	}
	if c := bus.SubscriberCount("item-c"); c != 0 {
		t.Errorf("none on item-c: want 0, got %d", c)
	}

	bus.Unsubscribe(chA1)
	if c := bus.SubscriberCount("item-a"); c != 1 {
		t.Errorf("after one unsub: want 1, got %d", c)
	}

	bus.Unsubscribe(chA2)
	bus.Unsubscribe(chB)
	if c := bus.SubscriberCount("item-a"); c != 0 {
		t.Errorf("after all unsub item-a: want 0, got %d", c)
	}
	if c := bus.SubscriberCount("item-b"); c != 0 {
		t.Errorf("after all unsub item-b: want 0, got %d", c)
	}
}

// TestMemoryOpBusCloseClosesAllChannels confirms Close cleans up
// every subscriber, regardless of which item they were watching.
func TestMemoryOpBusCloseClosesAllChannels(t *testing.T) {
	bus := NewMemoryOpBus()

	chA := bus.Subscribe("item-a")
	chB := bus.Subscribe("item-b")

	bus.Close()

	if _, ok := <-chA; ok {
		t.Errorf("chA should be closed after bus.Close")
	}
	if _, ok := <-chB; ok {
		t.Errorf("chB should be closed after bus.Close")
	}
	if c := bus.SubscriberCount("item-a"); c != 0 {
		t.Errorf("after Close: want 0 subscribers on item-a, got %d", c)
	}
}

// TestMemoryOpBusConcurrentPublish confirms the bus survives many
// concurrent publishers without panicking or tripping the race
// detector on the subscriber map. Some events legitimately get dropped
// under contention (that's the slow-subscriber-drop contract — a
// dedicated test exercises that path), so we don't assert "all events
// received" here; the goal is race-cleanliness, not zero loss.
func TestMemoryOpBusConcurrentPublish(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()

	ch := bus.Subscribe("item-a")

	const writers = 8
	const each = 50
	const want = writers * each

	// Drain continuously so the buffer doesn't sit full for long and
	// every publisher gets to make progress. Stop when publishers are
	// done AND the channel is empty.
	stopDrain := make(chan struct{})
	receivedCh := make(chan int, 1)
	go func() {
		count := 0
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					receivedCh <- count
					return
				}
				count++
			case <-stopDrain:
				// Drain remaining buffered events without blocking.
				for {
					select {
					case _, ok := <-ch:
						if !ok {
							receivedCh <- count
							return
						}
						count++
					default:
						receivedCh <- count
						return
					}
				}
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				bus.Publish(OpEvent{
					ItemID:   "item-a",
					ClientID: uint64(id),
					Type:     OpTypeSync,
					Data:     []byte{byte(i)},
				})
			}
		}(w)
	}
	wg.Wait()
	// Give the drain goroutine a beat to flush anything still buffered
	// before signalling stop. This isn't strictly required for race
	// cleanliness but keeps the received-count meaningful for the
	// sanity assertion below.
	time.Sleep(50 * time.Millisecond)
	close(stopDrain)

	select {
	case got := <-receivedCh:
		// Sanity: at least some events landed. Exact count varies
		// because of the legitimate slow-consumer drop path.
		if got == 0 {
			t.Fatalf("want at least one event received, got 0 (publish path appears broken)")
		}
		if got > want {
			t.Fatalf("got more events than published: %d > %d", got, want)
		}
		t.Logf("concurrent publish: %d/%d events delivered (drops are expected and OK)", got, want)
	case <-time.After(5 * time.Second):
		t.Fatal("drain goroutine did not return")
	}
}

// drain reads up to n events from ch with a short timeout so a
// missing/slow event doesn't hang the test.
func drain(ch chan OpEvent, n int) []OpEvent {
	out := make([]OpEvent, 0, n)
	for i := 0; i < n; i++ {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(1 * time.Second):
			return out
		}
	}
	return out
}
