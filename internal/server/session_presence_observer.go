package server

import "sync"

// Presence operation labels, passed to the failure callback. Bounded by
// construction so they are safe as metric label values.
const (
	// PresenceOpRegister — the initial write for a newly connected
	// session. Failure leaves the session out of the picker until the
	// first successful renewal.
	PresenceOpRegister = "register"
	// PresenceOpRenew — the keepalive write. Sustained failure lets the
	// entry expire on its TTL while the connection is still live.
	PresenceOpRenew = "renew"
	// PresenceOpDeregister — the explicit removal on disconnect. Failure
	// leaves a dead session listed until its TTL expires, so the picker
	// OVER-reports and a push aimed at it reaches nobody.
	PresenceOpDeregister = "deregister"
	// PresenceOpList — reading a user's sessions. Failure surfaces to the
	// caller as a 503 rather than as a short list.
	PresenceOpList = "list"
	// PresenceOpPrune — removing index members whose session key is gone.
	// Failure is the most benign: the index carries a stale member that
	// the next read skips and the next prune retries.
	PresenceOpPrune = "prune"
)

// presenceObservable is the nil-safe holder RedisSessionPresence embeds
// for its failure callback (BUG-2727).
//
// A CALLBACK, not an interface. An earlier version of this file declared
// a one-method PresenceObserver interface plus an adapter type plus a
// constructor, for a single production consumer — and did so in the same
// diff where RedisHealth and the stream gauge both take plain callbacks
// for exactly the same job (codex round 8). internal/watchevents keeps an
// interface because it reports five distinct conditions and a five-arg
// callback would be unreadable; one condition does not earn one.
//
// The registry is deliberately FAIL-SOFT everywhere — a failed write
// risks leaving a live session unlisted rather than tearing down the SSE
// connection it belongs to — which is exactly why the failures need a
// counter: nothing else surfaces them to anyone.
//
// RISKS, not guarantees: a failure means the operation reported an
// error, and Redis can fail a pipeline or a script after it applied. The
// per-op consequences in internal/metrics are written the same way.
type presenceObservable struct {
	mu       sync.RWMutex
	onFailed func(op string)
}

// SetFailureObserver attaches the callback; nil detaches. Safe to call
// after the registry is running. The callback must be safe for concurrent
// use and must not block — it is called on the request and renewal paths.
func (o *presenceObservable) SetFailureObserver(fn func(op string)) {
	o.mu.Lock()
	o.onFailed = fn
	o.mu.Unlock()
}

func (o *presenceObservable) reportOpFailed(op string) {
	o.mu.RLock()
	fn := o.onFailed
	o.mu.RUnlock()
	if fn != nil {
		fn(op)
	}
}
