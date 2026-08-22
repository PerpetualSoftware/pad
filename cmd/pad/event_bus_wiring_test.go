package main

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/metrics"
	"github.com/PerpetualSoftware/pad/internal/redisns"
)

// CONVE-19: wiring is a claim. internal/events proves the bus calls its
// observer and internal/metrics proves the adapter maps it to a counter —
// and BOTH pass with the SetObserver call in this package deleted, because
// each of those tests attaches the observer itself. This is the only test
// that fails when the production wiring goes missing.
//
// Both deployment shapes, because they are two separate call sites and only
// one of them is exercised by any given deployment.
func TestBothEventBusShapesReportToMetrics(t *testing.T) {
	t.Run("in-process", func(t *testing.T) {
		m := metrics.New()
		bus := newObservedEventBus(nil, redisns.Default, m)
		t.Cleanup(bus.Close)

		// Negative control first: a served resume must not move the counter,
		// so a non-zero reading cannot be mistaken for this test's work.
		bus.Publish(events.Event{Type: events.ItemCreated, WorkspaceID: "ws-1"})
		if got := bus.EventsSince("ws-1", 1); got == nil {
			t.Fatal("a caught-up cursor must be served")
		}
		assertResumeGaps(t, m, 0)

		if got := bus.EventsSince("ws-never-seen", 4200); got != nil {
			t.Fatalf("expected a gap, got %d events", len(got))
		}
		assertResumeGaps(t, m, 1)
	})

	t.Run("redis", func(t *testing.T) {
		mr := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = client.Close() })

		m := metrics.New()
		bus := newObservedEventBus(client, redisns.Default, m)
		t.Cleanup(bus.Close)

		assertResumeGaps(t, m, 0)

		if got := bus.EventsSince("ws-never-seen", 4200); got != nil {
			t.Fatalf("expected a gap, got %d events", len(got))
		}
		assertResumeGaps(t, m, 1)
	})
}

// Reads the counter straight off the registry rather than through a helper
// exported from internal/metrics: this is the only consumer outside that
// package's own tests, and widening a production API to serve one test is a
// worse trade than eight lines here.
func assertResumeGaps(t *testing.T, m *metrics.Metrics, want float64) {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var got float64
	for _, f := range families {
		if f.GetName() != "pad_event_resume_gaps_total" {
			continue
		}
		for _, metric := range f.GetMetric() {
			got += metric.GetCounter().GetValue()
		}
	}
	if got != want {
		t.Fatalf("pad_event_resume_gaps_total = %v, want %v", got, want)
	}
}
