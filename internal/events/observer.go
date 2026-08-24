package events

import "sync"

// Observer receives OPERATIONAL events from an EventBus — the conditions an
// operator would want to alert on, as opposed to the events the bus exists to
// deliver.
//
// WHY THIS EXISTS ALONGSIDE metrics.InstrumentedBus, WHICH ALREADY WRAPS THIS
// BUS. Read this before "fixing" the duplication back into the wrapper.
//
// A wrapper can instrument anything visible at the EventBus interface:
// publishes, subscriptions, subscriber counts. That covered everything this
// package reported before BUG-2731. It cannot cover what BUG-2731 added,
// because those conditions are detected INSIDE the bus and are invisible from
// outside it:
//
//   - A resume this instance could not serve is a nil return from EventsSince,
//     and a wrapper CAN see that nil. What it cannot see is WHY — cold start,
//     eviction, a stopped subscription — which is the part an operator acts
//     on, and recovering it outside means reimplementing the coverage rules
//     being wrapped.
//   - A sequence reset is detected on the RECEIVE path, in fanOut, which no
//     caller ever invokes and no wrapper can observe.
//
// So the two coexist deliberately, split by what each can actually see, which
// is the same split internal/watchevents made for the same reason. Both keep
// Prometheus out of this package.
//
// Implementations must be safe for concurrent use and must not block. Every
// report fires with no bus lock held, so an observer may call back into the
// bus — except into a method that itself REPORTS (EventsSince can), which
// would be unbounded mutual recursion no lock discipline can prevent.
type Observer interface {
	// ResumeGap reports that a Last-Event-ID resume could not be served from
	// this instance's view, so the client is told sync_required and re-fetches.
	//
	// Every increment is one client being told to reconcile. NOT necessarily a
	// full re-fetch: the web client answers sync_required with an incremental
	// /changes delta and only falls back to a full refresh after a long
	// absence or a failure (web/src/lib/services/sync.svelte.ts).
	//
	// It is the metric that makes this fix falsifiable in production — the
	// claim is that the syncs it adds are the WARRANTED ones, and a RATE that
	// does not settle after a deploy is the evidence against it.
	ResumeGap(workspaceID string)

	// SequenceReset reports that this instance's replay coverage was thrown
	// away because something happened it cannot account for. reason is bounded
	// so it can be a metric label:
	//
	//   "subscription_resumed" — a pub/sub connection dropped and resubscribed,
	//                             so this instance cannot account for whatever
	//                             was published while it was away.
	//
	// A bounded label rather than a bare counter because the reason is what an
	// operator acts on, and because BUG-2736's ID-space work adds reasons here
	// (a changed epoch, a restarted counter) that are diagnosed differently
	// from a connection flap.
	SequenceReset(reason string)

	// ReceiveLoopExited reports that a workspace's Redis subscription loop
	// has stopped. After this the instance receives nothing for that
	// workspace — including its own publishes, which come back through Redis
	// like everyone else's.
	//
	// Expected at shutdown and whenever the last local subscriber for a
	// workspace leaves. It earns its counter as a RATE: a steady trickle with
	// stable subscriber counts means loops are dying under something other
	// than those two causes, which is otherwise invisible from outside the
	// process.
	ReceiveLoopExited()

	// EventDropped reports that a live subscriber's channel was full, so one
	// event was not delivered TO THAT SUBSCRIBER. reason is bounded so it can
	// be a metric label; today there is one:
	//
	//   "slow_subscriber" — the consumer had not drained its 64-deep channel.
	//
	// This is per-SUBSCRIBER, unlike every other report on this interface:
	// the same event reached every other subscriber for the workspace. It is
	// the counter for the condition BUG-2730 made honest — since that fix the
	// affected subscriber is told (its stream emits sync_required mid-stream),
	// so a non-zero rate here is a population of clients doing an extra delta
	// sync, not a population silently missing data.
	//
	// internal/watchevents has had the equivalent (NotificationDropped) since
	// BUG-2699; this bus's drops were log-only until BUG-2730, which is why a
	// deploy that starts reporting them is not necessarily a regression — it
	// may be the first time they were countable.
	EventDropped(reason string)

	// SubscriptionUnconfirmed reports that a workspace's Redis subscription
	// could not be CONFIRMED within the bus's bound, so the subscribers
	// waiting on it were admitted anyway rather than refused (BUG-2747).
	//
	// It is not itself a coverage ending, and it is counted separately from
	// SequenceReset: nothing has been lost that this instance knows of; what
	// happened is that a stream was admitted whose coverage is UNKNOWN, because
	// Redis had not acknowledged the SUBSCRIBE.
	//
	// THE TWO ARE NOT DISJOINT, and an earlier version of this comment said
	// they were (codex round 3). When the acknowledgement eventually lands,
	// that path calls dropWorkspaceCoverage, which reports SequenceReset with
	// reason subscription_unconfirmed — though only when a buffer existed to
	// drop, which on this path is the uncommon case. So this counter is the
	// dependable one and that reason is corroboration.
	//
	// IT COUNTS ESTABLISHMENTS, NOT CLIENTS — also corrected from an earlier
	// wording. One increment is one workspace subscription whose wait expired,
	// however many subscribers were waiting on it; all of them are told to
	// reconcile when the acknowledgement lands, so the client-side fan-out
	// shows up in the midstream-resync counter rather than here.
	//
	// Expect zero. A non-zero rate means the SUBSCRIBE round trip is slow or
	// stalling — the same Redis condition BUG-2748 makes an availability
	// hazard.
	SubscriptionUnconfirmed()

	// SubscriptionCycled reports that a workspace's Redis subscription received
	// NOTHING — no event, no heartbeat, no acknowledgement — for longer than
	// the bus's idle timeout, so its connection was torn down and replaced
	// (BUG-2738).
	//
	// IT IS COUNTED SEPARATELY FROM SequenceReset FOR A REASON THAT IS NOT
	// STYLISTIC. An idle cycle calls dropWorkspaceCoverage, which reports
	// SequenceReset with reason idle_timeout — but ONLY when a buffer existed
	// to drop, and the case this detector exists for is disproportionately the
	// case with no buffer: a route that wedged early, on a quiet workspace,
	// having delivered nothing this instance could buffer. Reading cycles off
	// the reset counter alone would therefore under-report exactly the
	// incidents it was built to find. This counter is the dependable one; the
	// idle_timeout reset label is corroboration.
	//
	// Expect zero — and note that on heartbeat phase 1 it is zero STRUCTURALLY,
	// because the detector does not run at all there, so a zero says nothing
	// about whether any route has wedged. On phase 2, a non-zero rate means
	// connections between this instance and Redis are being silently
	// blackholed — a NAT idle timeout, a stateful firewall, an overlay network
	// dropping long-lived flows. Compare against TCP keepalive settings on the
	// path before tuning the interval, because tuning the interval treats the
	// symptom.
	SubscriptionCycled()

	// HeartbeatPublishFailed reports that this instance could not publish a
	// liveness heartbeat for one workspace (BUG-2738).
	//
	// IT IS THE DETECTOR SAYING IT CANNOT SEE, not a finding about any peer.
	// While it is firing, idle detection for that workspace is SUSPENDED —
	// silence cannot be read as evidence when we could not ask — so a non-zero
	// rate here means half-open detection is degraded or off for those
	// workspaces, however healthy pad_event_subscription_cycled_total looks.
	//
	// PUBLISH and pub/sub use different connection pools, so this is a signal
	// about the OUTBOUND path specifically: pool exhaustion, a wedged outbound
	// route, or Redis refusing writes. An instance in this state is also
	// failing to deliver its own events to every other instance, which is a
	// larger problem than the one this feature exists to find.
	//
	// Expect zero.
	HeartbeatPublishFailed()
}

// Drop reasons. Bounded by construction so they are safe as metric labels.
const (
	// DropReasonSlowSubscriber is the only drop reason today: a subscriber's
	// buffered channel was full at fan-out time.
	DropReasonSlowSubscriber = "slow_subscriber"
)

// Reset reasons. Bounded by construction so they are safe as metric labels.
const (
	// ResetReasonSubscriptionResumed is scoped to ONE workspace: a dropped
	// subscription says nothing about any other channel. See Observer.
	ResetReasonSubscriptionResumed = "subscription_resumed"

	// ResetReasonEpochChange means the shared Redis counter's ID SPACE
	// changed: the epoch travelling with an arriving message is not the one
	// this instance had adopted (BUG-2736). Every buffer is dropped, not just
	// the arriving event's workspace, because the counter is global.
	//
	// What an operator does with it: a handful at once, correlated with a
	// deploy or a Redis restart, is the mechanism working. A steady trickle
	// means the counter key is being evicted or deleted repeatedly — check
	// maxmemory policy against the events keyspace.
	ResetReasonEpochChange = "epoch_change"

	// ResetReasonCounterBackward means an ID arrived at or below a buffer's
	// high-water mark WITHOUT an epoch change: the same numeric space
	// delivered something out of order, or restarted inside its own epoch.
	//
	// WHAT TO EXPECT DEPENDS ON THE PHASE. On phase 1 it can be non-zero at
	// any time: that path assigns and publishes in two calls, so two instances
	// can interleave, and a counter reset there carries no epoch to explain
	// itself. During any roll with two publisher versions running, expect it
	// to rise. On phase 2 with every publisher flipped, expect it at or near
	// zero — a persistent rate there is an anomaly worth investigating rather
	// than tuning away. See the comment at the branch that reports it.
	ResetReasonCounterBackward = "counter_backward"

	// ResetReasonEpochRegressed means a LOWER generation was observed, so this
	// instance could not vouch for the space its buffers described and dropped
	// them.
	//
	// TWO CAUSES, told apart by COUNT rather than by anything at the moment it
	// fires. A single one alongside an epoch_change is a message that was in
	// flight when the generation rotated — benign, and the drop next to it is
	// nearly free because the rotation had already dropped the buffers. A
	// RUN of them means the counter itself went backwards and stayed there,
	// realistically a Redis failover to a replica that lost writes; the bus
	// recovers by adopting the lower generation, but every ID space boundary
	// it reports is suspect until the counter is durable again.
	ResetReasonEpochRegressed = "epoch_regressed"

	// ResetReasonUndecodableMessage means a pub/sub message could not be
	// parsed, so ONE workspace's coverage ended rather than the buffer going
	// on claiming a span that now has a hole in it.
	//
	// Expect zero. A non-zero count means something is publishing onto this
	// installation's channels that is not this installation — a namespace
	// collision is the likely cause — or that a payload is being truncated in
	// transit. Either way the events behind it are lost; the counter is what
	// says so.
	ResetReasonUndecodableMessage = "undecodable_message"

	// ResetReasonSubscriptionUnconfirmed means a subscription was admitted
	// before Redis acknowledged the SUBSCRIBE, and the acknowledgement then
	// arrived (BUG-2747). Everything published in between reached this
	// instance not at all, so the span the admitted subscribers sat through
	// is one their stream cannot account for.
	//
	// It reaches the reset counter only when a buffer existed to drop, which
	// on this path is the uncommon case — see dropWorkspaceCoverage for the
	// deliberate asymmetry between the metric and the client signal. The
	// dependable counter for this condition is Observer.SubscriptionUnconfirmed.
	ResetReasonSubscriptionUnconfirmed = "subscription_unconfirmed"

	// ResetReasonIdleTimeout means a workspace's Redis subscription received
	// nothing at all for longer than the bus's idle timeout, so this instance
	// STOPPED VOUCHING FOR ITS BUFFER (BUG-2738).
	//
	// IT DOES NOT SAY THE CONNECTION WAS REPLACED, and an earlier version of
	// this comment claimed it did (codex round 6). This reason is emitted
	// before the re-establishment is attempted, and the attempt can install
	// nothing — the bus closes, or the last subscriber leaves while we dial.
	// Observer.SubscriptionCycled is the one that means "replaced"; this one
	// means "coverage ended".
	//
	// WHAT IT ESTABLISHES IS NOT THAT EVENTS WERE LOST, unlike
	// subscription_resumed: nothing was observed going missing. What it says is
	// that the socket stopped proving it works, and a socket that cannot be
	// proved cannot back a coverage claim. The silence includes this instance's
	// own heartbeats, which is what makes it diagnostic rather than a guess
	// about how busy the workspace is — and is why the detector only runs on
	// heartbeat phase 2. On phase 1 this reason is structurally never emitted.
	//
	// It reaches this counter only when a buffer existed to drop. Read
	// Observer.SubscriptionCycled for the dependable count — the no-buffer case
	// is over-represented here for the reason recorded there.
	ResetReasonIdleTimeout = "idle_timeout"
)

// THE ONE THING AN OBSERVER CALLBACK MUST NOT DO is call a Subscribe path on
// the bus that is reporting to it.
//
// Callbacks run synchronously, and several of the paths that report — a late
// subscription acknowledgement, an idle-fired cycle — do so while holding that
// workspace's establishment record. A Subscribe arriving there waits on a
// record only the reporting goroutine can retire, and the reporting goroutine
// is waiting on the callback: neither moves again. Publishing, reading, and
// unsubscribing from a callback are all fine and are exercised by this
// package's tests; subscribing is the one door that is closed.
//
// observable is the shared, nil-safe Observer holder both bus implementations
// embed. Reporting before SetObserver is called — every bus in every test that
// does not opt in — is a no-op.
type observable struct {
	obsMu sync.RWMutex
	obs   Observer
}

// SetObserver attaches an Observer. Passing nil detaches. Safe to call after
// the bus is running; cmd/pad/cmd_server.go calls it at wiring time.
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

func (o *observable) reportResumeGap(workspaceID string) {
	if obs := o.observer(); obs != nil {
		obs.ResumeGap(workspaceID)
	}
}

func (o *observable) reportReset(reason string) {
	if obs := o.observer(); obs != nil {
		obs.SequenceReset(reason)
	}
}

func (o *observable) reportReceiveLoopExited() {
	if obs := o.observer(); obs != nil {
		obs.ReceiveLoopExited()
	}
}

func (o *observable) reportSubscriptionUnconfirmed() {
	if obs := o.observer(); obs != nil {
		obs.SubscriptionUnconfirmed()
	}
}

func (o *observable) reportSubscriptionCycled() {
	if obs := o.observer(); obs != nil {
		obs.SubscriptionCycled()
	}
}

func (o *observable) reportHeartbeatPublishFailed() {
	if obs := o.observer(); obs != nil {
		obs.HeartbeatPublishFailed()
	}
}

func (o *observable) reportDropped(reason string) {
	if obs := o.observer(); obs != nil {
		obs.EventDropped(reason)
	}
}
