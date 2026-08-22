package metrics

import "github.com/PerpetualSoftware/pad/internal/watchevents"

// WatchEventsObserver adapts a Metrics into a watchevents.Observer
// (BUG-2727), so internal/watchevents reports its operational events
// without importing Prometheus.
//
// This is an ADAPTER rather than a bus wrapper — the shape
// NewInstrumentedBus uses for events.EventBus — because every condition it
// reports is detected on the RECEIVE path, inside the bus, and is invisible
// from outside the Bus interface. A wrapper can count publishes and
// subscribers; it cannot see a notification that never arrived.
type WatchEventsObserver struct {
	m *Metrics
}

// NewWatchEventsObserver returns an observer writing into m.
func NewWatchEventsObserver(m *Metrics) *WatchEventsObserver {
	return &WatchEventsObserver{m: m}
}

// Compile-time proof this satisfies the seam it exists for. Without it, a
// later signature change in the interface would surface as a wiring error
// in cmd/pad rather than here.
var _ watchevents.Observer = (*WatchEventsObserver)(nil)

func (o *WatchEventsObserver) NotificationDropped(reason string) {
	o.m.WatchNotificationsDroppedTotal.WithLabelValues(reason).Inc()
}

// SequenceGap increments both counters: the gap event, and the number of
// notifications it spans. A non-positive `missing` still counts as an
// event — the bus only calls this on a forward jump, so a value that is
// not positive means the caller's arithmetic changed, and swallowing it
// would hide that regression rather than the gap.
func (o *WatchEventsObserver) SequenceGap(missing int64) {
	o.m.WatchSequenceGapsTotal.Inc()
	if missing > 0 {
		o.m.WatchNotificationsMissedTotal.Add(float64(missing))
	}
}

func (o *WatchEventsObserver) SequenceReset(reason string) {
	o.m.WatchSequenceResetsTotal.WithLabelValues(reason).Inc()
}

func (o *WatchEventsObserver) ReceiveLoopExited() {
	o.m.WatchReceiveLoopExitsTotal.Inc()
}
