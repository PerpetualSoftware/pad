package metrics

import "github.com/PerpetualSoftware/pad/internal/events"

// EventsObserver adapts a Metrics into an events.Observer (BUG-2731).
//
// This package ALREADY wraps events.EventBus with InstrumentedBus, and the two
// coexist deliberately. A wrapper sees the EventBus interface: publishes,
// subscribes, subscriber counts. It could see a nil return from EventsSince,
// but not WHY the bus refused — cold start, eviction, reset — without
// reimplementing the coverage rules it wraps, and it cannot see a coverage
// reset at all, since that is detected on the receive path which no caller
// invokes. Same split, same reason, as WatchEventsObserver.
type EventsObserver struct {
	m *Metrics
}

// NewEventsObserver returns an observer writing into m.
func NewEventsObserver(m *Metrics) *EventsObserver {
	return &EventsObserver{m: m}
}

// Compile-time proof this satisfies the seam it exists for, so a later
// signature change surfaces here rather than as a wiring error in cmd/pad.
var _ events.Observer = (*EventsObserver)(nil)

// ResumeGap takes the workspace ID but deliberately does NOT label the metric
// with it. Workspace count is unbounded and operator-controlled, so a
// per-workspace label is a cardinality bomb on a busy installation.
//
// The argument stays on the interface because the bus knows the workspace and
// a future observer — a sampling logger, a per-tenant debug hook — would need
// it; this adapter is simply not that consumer. Dropping it from the interface
// to match this one implementation would be the harder change to undo.
func (o *EventsObserver) ResumeGap(string) {
	o.m.EventResumeGapsTotal.Inc()
}

func (o *EventsObserver) SequenceReset(reason string) {
	o.m.EventSequenceResetsTotal.WithLabelValues(reason).Inc()
}

func (o *EventsObserver) ReceiveLoopExited() {
	o.m.EventReceiveLoopExitsTotal.Inc()
}

// EventDropped mirrors WatchEventsObserver.NotificationDropped. reason is
// bounded by the events package, so it is safe as a label.
func (o *EventsObserver) EventDropped(reason string) {
	o.m.EventEventsDroppedTotal.WithLabelValues(reason).Inc()
}
