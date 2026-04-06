package metrics

import (
	"sync"

	"github.com/xarmian/pad/internal/events"
)

// InstrumentedBus wraps an events.EventBus to record Prometheus metrics
// for SSE connections (per workspace) and event publish counts.
// It implements the events.EventBus interface so it can be used as a
// drop-in replacement without changing the interface or its implementations.
type InstrumentedBus struct {
	inner   events.EventBus
	metrics *Metrics

	mu          sync.Mutex
	workspaces  map[chan events.Event]string // channel → workspaceID for gauge decrement
}

// NewInstrumentedBus wraps an EventBus with Prometheus instrumentation.
func NewInstrumentedBus(inner events.EventBus, m *Metrics) *InstrumentedBus {
	return &InstrumentedBus{
		inner:      inner,
		metrics:    m,
		workspaces: make(map[chan events.Event]string),
	}
}

// Subscribe delegates to the inner bus and increments the SSE connection gauge.
func (b *InstrumentedBus) Subscribe(workspaceID string) chan events.Event {
	ch := b.inner.Subscribe(workspaceID)

	b.mu.Lock()
	b.workspaces[ch] = workspaceID
	b.mu.Unlock()

	b.metrics.SSEConnectionsActive.WithLabelValues(workspaceID).Inc()
	(*b.metrics.EventBusSubscribers).Set(float64(b.inner.SubscriberCount()))
	return ch
}

// Unsubscribe delegates to the inner bus and decrements the SSE connection gauge.
func (b *InstrumentedBus) Unsubscribe(ch chan events.Event) {
	b.mu.Lock()
	workspaceID, ok := b.workspaces[ch]
	if ok {
		delete(b.workspaces, ch)
	}
	b.mu.Unlock()

	b.inner.Unsubscribe(ch)

	if ok {
		b.metrics.SSEConnectionsActive.WithLabelValues(workspaceID).Dec()
	}
	(*b.metrics.EventBusSubscribers).Set(float64(b.inner.SubscriberCount()))
}

// Publish delegates to the inner bus and increments the publish counter.
func (b *InstrumentedBus) Publish(event events.Event) {
	b.inner.Publish(event)
	(*b.metrics.EventBusPublishTotal).Inc()
}

// Close delegates to the inner bus.
func (b *InstrumentedBus) Close() {
	b.inner.Close()
}

// SubscriberCount delegates to the inner bus.
func (b *InstrumentedBus) SubscriberCount() int {
	return b.inner.SubscriberCount()
}
