package metrics

import (
	"context"
	"sync"

	"github.com/PerpetualSoftware/pad/internal/events"
)

// InstrumentedBus wraps an events.EventBus to record Prometheus metrics
// for SSE connections (per workspace) and event publish counts.
// It implements the events.EventBus interface so it can be used as a drop-in
// replacement anywhere one is wired. Being a drop-in does NOT mean the
// interface is frozen: this type tracks it, and every method that grew a
// return value (BUG-2730's gap channel among them) grew one here too.
type InstrumentedBus struct {
	inner   events.EventBus
	metrics *Metrics

	mu         sync.Mutex
	workspaces map[chan events.Event]string // channel → workspaceID for gauge decrement
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
func (b *InstrumentedBus) Subscribe(ctx context.Context, workspaceID string) (chan events.Event, <-chan struct{}, events.SubscribeOutcome) {
	ch, gaps, outcome := b.inner.Subscribe(ctx, workspaceID)
	if outcome != events.SubscribeOK {
		return nil, nil, outcome
	}
	b.trackSubscription(ch, workspaceID)
	return ch, gaps, outcome
}

// SubscribeIfAllowed delegates the atomic check-and-subscribe to the inner bus
// and updates Prometheus gauges on success.
//
// EVERY NON-OK OUTCOME IS TREATED THE SAME HERE, deliberately: SubscribeOK is
// the only one that hands back a channel, so it is the only one this wrapper
// has anything to track. Distinguishing a limit refusal from a cancellation is
// the HANDLER's job (BUG-2749) — this type counts subscriptions, and there is
// no subscription in either case.
func (b *InstrumentedBus) SubscribeIfAllowed(ctx context.Context, workspaceID string, maxPerWorkspace int) (chan events.Event, <-chan struct{}, events.SubscribeOutcome) {
	ch, gaps, outcome := b.inner.SubscribeIfAllowed(ctx, workspaceID, maxPerWorkspace)
	if outcome != events.SubscribeOK {
		return nil, nil, outcome
	}

	b.trackSubscription(ch, workspaceID)
	return ch, gaps, outcome
}

// SubscribeAndReplaySince delegates the atomic check-subscribe-and-replay to
// the inner bus and updates the same gauges as SubscribeIfAllowed.
//
// The gap channel and the replay set pass through untouched: both are the
// inner bus's own knowledge (BUG-2730), and this wrapper exists to count
// subscriptions, not to have an opinion about coverage.
func (b *InstrumentedBus) SubscribeAndReplaySince(ctx context.Context, workspaceID string, sinceID int64, maxPerWorkspace int) (chan events.Event, []events.Event, <-chan struct{}, events.SubscribeOutcome) {
	ch, missed, gaps, outcome := b.inner.SubscribeAndReplaySince(ctx, workspaceID, sinceID, maxPerWorkspace)
	if outcome != events.SubscribeOK {
		return nil, nil, nil, outcome
	}

	b.trackSubscription(ch, workspaceID)
	return ch, missed, gaps, outcome
}

// trackSubscription records the channel's workspace and bumps the gauges, so
// every successful subscribe path does the same bookkeeping.
func (b *InstrumentedBus) trackSubscription(ch chan events.Event, workspaceID string) {
	b.mu.Lock()
	b.workspaces[ch] = workspaceID
	b.mu.Unlock()

	(*b.metrics.SSEConnectionsActive).Inc()
	(*b.metrics.EventBusSubscribers).Set(float64(b.inner.SubscriberCount()))
}

// Unsubscribe delegates to the inner bus and decrements the SSE connection gauge.
func (b *InstrumentedBus) Unsubscribe(ch chan events.Event) {
	b.mu.Lock()
	_, ok := b.workspaces[ch]
	if ok {
		delete(b.workspaces, ch)
	}
	b.mu.Unlock()

	b.inner.Unsubscribe(ch)

	if ok {
		(*b.metrics.SSEConnectionsActive).Dec()
	}
	(*b.metrics.EventBusSubscribers).Set(float64(b.inner.SubscriberCount()))
}

// Publish delegates to the inner bus and increments the publish counter.
//
// ATTEMPTS, NOT CONFIRMATIONS, and the counter's Help says so.
// events.EventBus.Publish returns nothing, so a Redis publish that failed
// is indistinguishable here from one that succeeded — during an outage
// this counter keeps climbing while nothing is delivered. Fixing it
// properly means Publish reporting acceptance, which is the same change
// BUG-2699 made for the watch bus and the same interface-wide edit;
// tracked separately rather than smuggled into an instrumentation
// wrapper.
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

// WorkspaceSubscriberCount delegates to the inner bus.
func (b *InstrumentedBus) WorkspaceSubscriberCount(workspaceID string) int {
	return b.inner.WorkspaceSubscriberCount(workspaceID)
}

// EventsSince delegates to the inner bus.
func (b *InstrumentedBus) EventsSince(workspaceID string, sinceID int64) []events.Event {
	return b.inner.EventsSince(workspaceID, sinceID)
}
