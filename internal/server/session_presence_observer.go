package server

import (
	"sync"

	"github.com/PerpetualSoftware/pad/internal/metrics"
)

// PresenceObserver receives operational events from the Redis-backed
// session-presence registry (BUG-2727).
//
// The registry is deliberately FAIL-SOFT everywhere: a failed write leaves
// a live session unlisted rather than tearing down the SSE connection it
// belongs to, and a failed read answers the sessions endpoint with a 503
// rather than an empty list. That posture is right, and it is exactly why
// the failures need a counter — a fail-soft subsystem reports nothing to
// the user beyond a push that quietly reaches fewer sessions than it
// should, so a log line is the only trace and log lines are not alertable
// without someone already looking.
//
// Implementations must be safe for concurrent use and must not block.
type PresenceObserver interface {
	// PresenceOpFailed reports one failed Redis operation. op is a bounded
	// label from the PresenceOp* constants, never free text — it becomes a
	// metric label.
	PresenceOpFailed(op string)
}

// Presence operation labels. Bounded by construction so they are safe as
// metric label values.
const (
	// PresenceOpRegister — the initial write for a newly connected
	// session. It leaves the session out of the picker until the first
	// successful renewal.
	PresenceOpRegister = "register"
	// PresenceOpRenew — the keepalive write. Sustained failure lets the
	// entry expire on its TTL while the connection is still live.
	PresenceOpRenew = "renew"
	// PresenceOpDeregister — the explicit removal on disconnect. Failure
	// leaves a dead session listed until its TTL expires, so the picker
	// over-reports.
	PresenceOpDeregister = "deregister"
	// PresenceOpList — reading a user's sessions. Failure surfaces to the
	// caller as a 503 rather than as a short list.
	PresenceOpList = "list"
	// PresenceOpPrune — removing index members whose session key is gone.
	// Failure is the most benign: the index carries a stale member that
	// the next read skips and the next prune retries.
	PresenceOpPrune = "prune"
)

// presenceObservable is the nil-safe holder RedisSessionPresence embeds.
type presenceObservable struct {
	obsMu sync.RWMutex
	obs   PresenceObserver
}

// SetObserver attaches a PresenceObserver; nil detaches. Safe to call
// after the registry is running.
func (o *presenceObservable) SetObserver(obs PresenceObserver) {
	o.obsMu.Lock()
	o.obs = obs
	o.obsMu.Unlock()
}

func (o *presenceObservable) reportOpFailed(op string) {
	o.obsMu.RLock()
	obs := o.obs
	o.obsMu.RUnlock()
	if obs != nil {
		obs.PresenceOpFailed(op)
	}
}

// metricsPresenceObserver adapts the Prometheus metrics into a
// PresenceObserver. It lives here rather than in internal/metrics because
// internal/server already imports internal/metrics; the reverse would be
// an import cycle.
type metricsPresenceObserver struct {
	m *metrics.Metrics
}

func (o metricsPresenceObserver) PresenceOpFailed(op string) {
	o.m.SessionPresenceFailuresTotal.WithLabelValues(op).Inc()
}

// NewMetricsPresenceObserver returns a PresenceObserver that writes into
// m's Prometheus counters. Exported because the wiring lives in cmd/pad.
func NewMetricsPresenceObserver(m *metrics.Metrics) PresenceObserver {
	return metricsPresenceObserver{m: m}
}
