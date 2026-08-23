package metrics

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/events"
)

// The wrapper is the only events.EventBus implementation that does not
// originate a gap channel — it passes the inner bus's through. A wrapper that
// returned nil there would silently disable the whole fix in production, where
// the bus IS wrapped, while every test against the inner bus stayed green
// (team CONVE-19: a component tested directly says nothing about its binding).
func TestInstrumentedBusPassesTheGapChannelThrough(t *testing.T) {
	inner := events.New()
	defer inner.Close()
	b := NewInstrumentedBus(inner, New())

	ch, gaps, ok := b.SubscribeIfAllowed("ws-1", 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	defer b.Unsubscribe(ch)
	if gaps == nil {
		t.Fatal("SubscribeIfAllowed returned a nil gap channel; every drop would be silent")
	}

	// Overflow the 64-deep channel without draining it.
	for range 65 {
		b.Publish(events.Event{Type: events.ItemUpdated, WorkspaceID: "ws-1"})
	}

	select {
	case <-gaps:
	default:
		t.Error("the inner bus raised a gap and the wrapper's channel never saw it")
	}
}

// SubscribeAndReplaySince is the wrapper's other new method; the replay it
// forwards is what the SSE handler writes to the client, so dropping it would
// silently lose every resumed event.
func TestInstrumentedBusForwardsTheReplay(t *testing.T) {
	inner := events.New()
	defer inner.Close()
	b := NewInstrumentedBus(inner, New())

	b.Publish(events.Event{Type: events.ItemUpdated, WorkspaceID: "ws-1", ItemID: "one"})
	b.Publish(events.Event{Type: events.ItemUpdated, WorkspaceID: "ws-1", ItemID: "two"})
	cursor := b.EventsSince("ws-1", 0)[0].ID

	ch, missed, gaps, ok := b.SubscribeAndReplaySince("ws-1", cursor, 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	defer b.Unsubscribe(ch)
	if gaps == nil {
		t.Error("SubscribeAndReplaySince returned a nil gap channel")
	}
	if len(missed) != 1 || missed[0].ItemID != "two" {
		t.Fatalf("the replay was not forwarded intact: %+v", missed)
	}
	if got := (*b.metrics.EventBusSubscribers); got == nil {
		t.Error("the subscriber gauge was not wired on this path")
	}
}

// The adapter is the last hop between the bus's report and Prometheus. It is
// one line, which is exactly the kind of line that gets written for the wrong
// counter.
func TestEventsObserverRecordsADrop(t *testing.T) {
	m := New()
	o := NewEventsObserver(m)
	o.EventDropped(events.DropReasonSlowSubscriber)

	assertCounter(t, m, "pad_event_events_dropped_total",
		map[string]string{"reason": events.DropReasonSlowSubscriber}, 1)

	// A mapping that crossed this wire with the resume counter would satisfy
	// the assertion above on a registry where both happened to be zero.
	assertCounter(t, m, "pad_event_resume_gaps_total", nil, 0)
	assertCounter(t, m, "pad_watchevents_notifications_dropped_total",
		map[string]string{"reason": events.DropReasonSlowSubscriber}, 0)
}
