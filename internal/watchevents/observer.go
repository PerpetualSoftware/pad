package watchevents

import "sync"

// Observer receives OPERATIONAL events from a Bus — the conditions an
// operator would want to alert on, as opposed to the notifications the bus
// exists to deliver. It is the seam internal/metrics implements so this
// package can stay free of a Prometheus dependency, mirroring the reason
// events.EventBus is instrumented by a wrapper rather than by importing
// metrics.
//
// A wrapper does not work HERE, though, and that asymmetry is the whole
// reason this interface exists: every condition below is detected INSIDE
// the bus, on the receive path, and is invisible at the Bus interface. A
// caller sees Publish succeed and Subscribe return a channel whether or
// not this instance is quietly missing notifications.
//
// WHAT THIS DOES AND DOES NOT SEE (BUG-2727). These callbacks report what
// PAD detects. They do NOT hook go-redis, which has its own silent drop:
// it buffers 100 messages per subscription and discards further ones after
// a 60s send timeout, logging only through its own internal logger. That
// drop is not reported here directly — it is reported by its CONSEQUENCE,
// as a SequenceGap, because a discarded message leaves a hole in the id
// sequence exactly like a message lost across a reconnect does. So
// SequenceGap counts "this instance missed something", not "Redis was at
// fault"; do not read a gap as evidence of any particular cause.
//
// Implementations must be safe for concurrent use and must not block —
// they are called while the bus holds its mutex on the receive path.
type Observer interface {
	// NotificationDropped reports a notification this instance received but
	// could not deliver to one of its own subscribers. reason is a bounded
	// label ("slow_subscriber"), never free text — it becomes a metric
	// label.
	NotificationDropped(reason string)

	// SequenceGap reports that the received id sequence skipped forward:
	// this instance missed `missing` notifications. Resumes across the gap
	// are answered with sync_required (see fanOutLocally); subscribers
	// holding a stream open across it are told nothing, which is BUG-2730
	// and not this seam's job to fix.
	SequenceGap(missing int64)

	// ResumeGap reports that a RESUME could not be served from this
	// instance's view — the client is told sync_required and re-fetches.
	//
	// Separate from SequenceGap rather than folded into it, because an
	// operator diagnoses them differently and one counter would conflate
	// them. SequenceGap means "ids arrived here with a hole in them", a
	// delivery fault. This one means "a client's cursor is outside what
	// this instance can vouch for" — which happens for a hole, but also
	// on a cold start, after an epoch change, and when the shared counter
	// disagrees. It is the only one of the two that is always
	// USER-VISIBLE, since it produces a resync.
	ResumeGap()

	// SequenceReset reports that the id space itself changed under this
	// instance and the replay buffer was dropped. reason is bounded:
	// "epoch_change" (the shared epoch key changed) or "counter_backwards"
	// (an id arrived at or below the high-water mark).
	SequenceReset(reason string)

	// ReceiveLoopExited reports that the single consumer of the shared
	// Redis channel has stopped. After this the instance publishes fine and
	// receives nothing — including its own publishes, which are delivered
	// through Redis like everyone else's.
	//
	// Reachability, verified against go-redis v9.22.0 rather than assumed:
	// PubSub.Channel's message channel is closed ONLY on pool.ErrClosed,
	// i.e. the client or the PubSub was closed. Every other receive error
	// is retried indefinitely, and a health-check goroutine pings every 3s
	// and reconnects (re-subscribing) on failure. So in practice this fires
	// at shutdown, and a firing OUTSIDE shutdown means the client was
	// closed underneath the bus — which is why it is worth a counter and an
	// ERROR log rather than a silent return.
	ReceiveLoopExited()
}

// observable is the shared, nil-safe Observer holder both bus
// implementations embed. Reporting through it before SetObserver is
// called — which is every bus in every test that does not opt in, and
// every single-process deployment — is a no-op.
type observable struct {
	obsMu sync.RWMutex
	obs   Observer
}

// SetObserver attaches an Observer. Passing nil detaches. Safe to call
// after the bus is running; cmd/pad/cmd_server.go calls it at wiring time.
func (o *observable) SetObserver(obs Observer) {
	o.obsMu.Lock()
	o.obs = obs
	o.obsMu.Unlock()
}

func (o *observable) observer() Observer {
	o.obsMu.RLock()
	obs := o.obs
	o.obsMu.RUnlock()
	return obs
}

func (o *observable) reportDropped(reason string) {
	if obs := o.observer(); obs != nil {
		obs.NotificationDropped(reason)
	}
}

func (o *observable) reportGap(missing int64) {
	if obs := o.observer(); obs != nil {
		obs.SequenceGap(missing)
	}
}

func (o *observable) reportReset(reason string) {
	if obs := o.observer(); obs != nil {
		obs.SequenceReset(reason)
	}
}

func (o *observable) reportResumeGap() {
	if obs := o.observer(); obs != nil {
		obs.ResumeGap()
	}
}

func (o *observable) reportReceiveLoopExited() {
	if obs := o.observer(); obs != nil {
		obs.ReceiveLoopExited()
	}
}

// Drop reasons and reset reasons. Bounded by construction so they can be
// metric labels without a cardinality risk.
const (
	DropReasonSlowSubscriber = "slow_subscriber"

	ResetReasonEpochChange     = "epoch_change"
	ResetReasonCounterBackward = "counter_backwards"
)
