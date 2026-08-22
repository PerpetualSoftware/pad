package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/PerpetualSoftware/pad/internal/config"
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
		bus := newObservedEventBus(&config.Config{}, nil, redisns.Default, m)
		t.Cleanup(bus.Close)

		// Negative control first: a served resume must not move the counter,
		// so a non-zero reading cannot be mistaken for this test's work.
		bus.Publish(events.Event{Type: events.ItemCreated, WorkspaceID: "ws-1"})
		// The id comes from the bus, not from a literal: since BUG-2736 the
		// in-process counter starts from this incarnation's base, so 1 names
		// a dead space and would be answered as a gap — which would make this
		// negative control assert the opposite of what it is for.
		published := bus.EventsSince("ws-1", 0)
		if len(published) != 1 {
			t.Fatalf("fixture: expected one buffered event, got %d", len(published))
		}
		if got := bus.EventsSince("ws-1", published[0].ID); got == nil {
			t.Fatal("a caught-up cursor must be served")
		}
		assertResumeGaps(t, m, 0)

		// The unservable resume: a cursor from THIS incarnation naming a
		// workspace with no buffer. Base-relative for the same reason as
		// above — a small literal would be refused by the incarnation guard
		// rather than by the no-buffer branch, so the counter would move for
		// a reason this test does not name.
		if got := bus.EventsSince("ws-never-seen", published[0].ID+4200); got != nil {
			t.Fatalf("expected a gap, got %d events", len(got))
		}
		assertResumeGaps(t, m, 1)
	})

	t.Run("redis", func(t *testing.T) {
		mr := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = client.Close() })

		m := metrics.New()
		bus := newObservedEventBus(&config.Config{}, client, redisns.Default, m)
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

// The phase-2 flip has to REACH the bus, which is a separate claim from the
// bus honouring it (BUG-2736). internal/events proves a bus constructed with
// publishEpoch=true emits the prefixed form; this proves newObservedEventBus
// carries the parameter there rather than dropping it, and that the
// in-process shape ignores it instead of panicking or changing behaviour.
//
// The config-to-bus link is covered by the same two cases, because
// newObservedEventBus takes the whole Config and reads the flip itself. That
// link WAS untested when the flip was a hand-picked argument at the RunE call
// site: replacing it with `false` there compiled and passed the whole tree,
// leaving the deployment silently on phase 1. Both directions are asserted
// because a helper that ignores its config and hardcodes EITHER value would
// pass a one-directional test.
//
// WHAT REMAINS UNEXECUTED, stated rather than left to be assumed: the
// `newObservedEventBus(cfg, ...)` calls inside RunE itself, which this package
// cannot invoke without standing up a server. A separate guard
// (redis_keyspace_wiring_test.go) reads those call sites as text and counts
// them, which is what catches a shape being dropped.
func TestThePublishEpochFlipReachesTheRedisBus(t *testing.T) {
	channel := redisns.Default.Name("events:") + "ws-1"

	for _, tc := range []struct {
		name         string
		publishEpoch bool
		wantPrefix   bool
	}{
		{name: "phase 1", publishEpoch: false, wantPrefix: false},
		{name: "phase 2", publishEpoch: true, wantPrefix: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = client.Close() })

			ps := client.Subscribe(context.Background(), channel)
			t.Cleanup(func() { _ = ps.Close() })
			if _, err := ps.Receive(context.Background()); err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			incoming := ps.Channel()

			bus := newObservedEventBus(&config.Config{EventsPublishEpoch: tc.publishEpoch}, client, redisns.Default, metrics.New())
			t.Cleanup(bus.Close)
			bus.Publish(events.Event{Type: events.ItemCreated, WorkspaceID: "ws-1"})

			select {
			case msg := <-incoming:
				// The prefixed form is not valid JSON on its own; the bare
				// form is. That is the difference an older instance sees, so
				// it is the difference this asserts.
				var ev events.Event
				bare := json.Unmarshal([]byte(msg.Payload), &ev) == nil
				if bare == tc.wantPrefix {
					t.Fatalf("publishEpoch=%v: got payload %q, want prefixed=%v", tc.publishEpoch, msg.Payload, tc.wantPrefix)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for the published event")
			}
		})
	}

	t.Run("in-process shape ignores it", func(t *testing.T) {
		bus := newObservedEventBus(&config.Config{EventsPublishEpoch: true}, nil, redisns.Default, metrics.New())
		t.Cleanup(bus.Close)
		bus.Publish(events.Event{Type: events.ItemCreated, WorkspaceID: "ws-1"})
		if got := bus.EventsSince("ws-1", 0); len(got) != 1 {
			t.Fatalf("the in-process bus must publish normally regardless of the flip, got %d events", len(got))
		}
	})
}
