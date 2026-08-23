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
// a 60s send timeout, logging only through its own internal logger.
//
// Such a drop is USUALLY visible here by its CONSEQUENCE, as a
// SequenceGap: the discarded message leaves a hole in the id sequence
// exactly like one lost across a reconnect. Two boundaries on that, both
// real:
//
//   - Detection needs a LATER notification to expose the hole. Drop the
//     newest message on a bus that then goes quiet and no gap is ever
//     reported, because nothing arrives to be non-consecutive with.
//   - A gap says "this instance missed something", NOT "Redis was at
//     fault". Reconnects produce them too. Do not read a gap as evidence
//     of any particular cause.
//
// go-redis's own log line is the only direct evidence of that drop, which
// is why cmd/pad routes its logger into slog.
//
// Implementations must be safe for concurrent use and must not block.
//
// They MAY call back into the bus. Every report fires AFTER the bus
// releases its mutex, so an observer that publishes or subscribes cannot
// deadlock the receive loop — a property of the bus rather than a rule
// for implementers, because an exported seam whose safety depends on
// callers reading a comment fails the first time somebody does not.
//
// TWO things an observer must not do. Call a bus method that REPORTS —
// SubscribeAndReplaySince can raise a resume gap — because that is
// unbounded mutual recursion, which no lock discipline here can prevent.
// And call Close: reports fire on the RECEIVE GOROUTINE, and Close waits
// on that goroutine through b.wg.Wait(), so an observer closing the bus
// from a callback waits on itself forever. Not a lock-ordering problem
// and not fixable by one — the goroutine is the resource being waited
// for. (Found by a lifecycle review on BUG-2739; the contract said only
// the first, which reads as though the second were allowed.)
type Observer interface {
	// NotificationDropped reports a notification this instance received but
	// could not deliver to one of its own subscribers. reason is a bounded
	// label ("slow_subscriber"), never free text — it becomes a metric
	// label.
	NotificationDropped(reason string)

	// SequenceGap reports that the received id sequence skipped forward:
	// this instance missed `missing` notifications. Both kinds of client are
	// told (see fanOutLocally): a resume across the gap is answered with
	// sync_required, and since BUG-2730 every subscriber holding a stream
	// open across it is signalled and gets the same answer mid-stream.
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

	// SequenceReset reports that this instance's replay coverage was
	// dropped. reason is bounded: "epoch_change" (the shared epoch key
	// changed), "counter_backward" (an id arrived at or below the
	// high-water mark), "subscription_resumed" (go-redis reconnected and
	// re-subscribed, so the outage's notifications never arrived) or
	// "undecodable_message" (a message on the channel could not be parsed).
	//
	// The first two mean the ID SPACE changed under us; the last two mean it
	// did not and we simply missed part of it. Both end coverage, because
	// the buffer cannot account for the span either way.
	SequenceReset(reason string)

	// ReceiveLoopExited reports that the single consumer of the shared
	// Redis channel has stopped. After this the instance publishes fine and
	// receives nothing — including its own publishes, which are delivered
	// through Redis like everyone else's.
	//
	// EXPECTED TO STAY AT ZERO, and that is the point: a should-never-
	// fire alarm on a state otherwise undetectable from outside the
	// process — an instance that publishes fine, answers health checks,
	// and receives nothing.
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

// pendingReports accumulates what a locked section wants to report, so
// the reports can be fired after the lock is released. See Observer.
type pendingReports struct {
	drops      []string
	resets     []string
	gapMissing []int64
	resumeGaps int
}

func (p *pendingReports) drop(reason string)  { p.drops = append(p.drops, reason) }
func (p *pendingReports) reset(reason string) { p.resets = append(p.resets, reason) }
func (p *pendingReports) resumeGap()          { p.resumeGaps++ }

func (p *pendingReports) gap(missing int64) {
	p.gapMissing = append(p.gapMissing, missing)
}

// flush fires everything collected. MUST be called with no bus lock held.
func (o *observable) flush(p *pendingReports) {
	for _, r := range p.drops {
		o.reportDropped(r)
	}
	for _, r := range p.resets {
		o.reportReset(r)
	}
	for _, m := range p.gapMissing {
		o.reportGap(m)
	}
	for i := 0; i < p.resumeGaps; i++ {
		o.reportResumeGap()
	}
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
	ResetReasonCounterBackward = "counter_backward"

	// ResetReasonSubscriptionResumed means go-redis reconnected and
	// re-subscribed: whatever was published while the connection was down
	// never reached this instance. Expect these during a Redis failover and
	// expect them to stop afterwards.
	ResetReasonSubscriptionResumed = "subscription_resumed"

	// ResetReasonUndecodableMessage means a message on the watch channel
	// could not be parsed, so this instance missed a notification whose id it
	// cannot even name. Expect zero; suspect a namespace collision.
	ResetReasonUndecodableMessage = "undecodable_message"
)
