package metrics

import (
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/events"
)

// failingBus is a MemoryBus whose Publish answers a fixed error, for the
// unconfirmed outcome a MemoryBus cannot produce on its own.
type failingBus struct {
	*events.MemoryBus
	err error
}

func (b failingBus) Publish(events.Event) error { return b.err }

func publishFailures(t *testing.T, m *Metrics, outcome string) (float64, bool) {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != "pad_eventbus_publish_failures_total" {
			continue
		}
		for _, metric := range fam.GetMetric() {
			if labelsMatch(metric.GetLabel(), map[string]string{"outcome": outcome}) {
				return metric.GetCounter().GetValue(), true
			}
		}
	}
	return 0, false
}

// BUG-2732: both series exist before the first failure, so a rate() reads
// zero rather than absent.
func TestPublishFailuresSeriesExistAtZero(t *testing.T) {
	m := New()
	for _, outcome := range []string{"closed", "unconfirmed"} {
		v, ok := publishFailures(t, m, outcome)
		if !ok {
			t.Fatalf("series outcome=%q not registered before any failure", outcome)
		}
		if v != 0 {
			t.Fatalf("outcome=%q: got %v, want 0", outcome, v)
		}
	}
}

func TestInstrumentedBusCountsPublishFailuresByOutcome(t *testing.T) {
	t.Run("closed", func(t *testing.T) {
		m := New()
		inner := events.New()
		bus := NewInstrumentedBus(inner, m)
		if err := bus.Publish(events.Event{Type: "test", WorkspaceID: "ws-1"}); err != nil {
			t.Fatalf("live publish: %v", err)
		}
		inner.Close()
		if err := bus.Publish(events.Event{Type: "test", WorkspaceID: "ws-1"}); !errors.Is(err, events.ErrBusClosed) {
			t.Fatalf("the wrapper must return the inner error unchanged; got %v", err)
		}

		if v, _ := publishFailures(t, m, "closed"); v != 1 {
			t.Fatalf("closed: got %v, want 1", v)
		}
		if v, _ := publishFailures(t, m, "unconfirmed"); v != 0 {
			t.Fatalf("unconfirmed: got %v, want 0", v)
		}
		// Attempts are unchanged in meaning: both publishes count.
		if v := counterValue(t, m, "pad_eventbus_publish_total", nil); v != 2 {
			t.Fatalf("publish_total: got %v, want 2 (attempts, failures included)", v)
		}
	})

	t.Run("unconfirmed", func(t *testing.T) {
		m := New()
		bus := NewInstrumentedBus(failingBus{MemoryBus: events.New(), err: errors.New("redis: connection refused")}, m)
		if err := bus.Publish(events.Event{Type: "test", WorkspaceID: "ws-1"}); err == nil {
			t.Fatal("the wrapper swallowed the inner error")
		}
		if v, _ := publishFailures(t, m, "unconfirmed"); v != 1 {
			t.Fatalf("unconfirmed: got %v, want 1", v)
		}
		if v, _ := publishFailures(t, m, "closed"); v != 0 {
			t.Fatalf("closed: got %v, want 0", v)
		}
	})
}
