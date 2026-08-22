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
}

// Reset reasons. Bounded by construction so they are safe as metric labels.
const (
	// ResetReasonSubscriptionResumed is scoped to ONE workspace: a dropped
	// subscription says nothing about any other channel. See Observer.
	ResetReasonSubscriptionResumed = "subscription_resumed"
)

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
