package metrics

import (
	"context"
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

	ch, gaps, outcome := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)
	if outcome != events.SubscribeOK {
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
	m := New()
	b := NewInstrumentedBus(inner, m)

	b.Publish(events.Event{Type: events.ItemUpdated, WorkspaceID: "ws-1", ItemID: "one"})
	b.Publish(events.Event{Type: events.ItemUpdated, WorkspaceID: "ws-1", ItemID: "two"})
	cursor := b.EventsSince("ws-1", 0)[0].ID

	ch, missed, gaps, outcome := b.SubscribeAndReplaySince(context.Background(), "ws-1", cursor, 0)
	if outcome != events.SubscribeOK {
		t.Fatal("subscribe refused")
	}
	defer b.Unsubscribe(ch)
	if gaps == nil {
		t.Error("SubscribeAndReplaySince returned a nil gap channel")
	}
	if len(missed) != 1 || missed[0].ItemID != "two" {
		t.Fatalf("the replay was not forwarded intact: %+v", missed)
	}
	// The gauge must have been SET, not merely be non-nil — the wrapper's
	// whole job on this path is the bookkeeping, and a delegation that
	// forwarded the data and skipped trackSubscription would look identical
	// to a nil check.
	assertGauge(t, m, "pad_sse_connections_active", 1)
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

// assertGauge mirrors assertCounter for gauge families.
func assertGauge(t *testing.T, m *Metrics, name string, want float64) {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, metric := range f.GetMetric() {
			if got := metric.GetGauge().GetValue(); got != want {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
			return
		}
	}
	if want != 0 {
		t.Errorf("%s is not exported", name)
	}
}
