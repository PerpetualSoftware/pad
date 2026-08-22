package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// redisProbeInterval is how often the prober pings Redis. Chosen to be
	// a few times finer than the presence TTL (90s) so a loss of Redis is
	// visible in the health payload and in pad_redis_up well before the
	// first user-visible consequence — a session expiring out of the
	// picker — rather than at the same moment.
	redisProbeInterval = 15 * time.Second

	// redisProbeTimeout bounds one ping ONCE A CONNECTION EXISTS.
	// Deliberately short: this is a liveness question, and a probe that
	// hangs is itself the answer.
	//
	// It does NOT bound the whole probe — see RedisHealth's doc comment.
	// Establishing a connection is governed by the client's DialTimeout
	// (go-redis v9 default: 5s), which this context cannot shorten, so the
	// real worst case for one probe is that dial rather than this value.
	// redisProbeInterval is comfortably longer than the dial bound, so a
	// probe against a black-holed server still finishes before the next
	// tick is due.
	redisProbeTimeout = 2 * time.Second
)

// RedisHealth periodically probes Redis and caches the result (BUG-2727).
//
// WHY A PROBER AND NOT A SCRAPE-TIME COLLECTOR. Both /api/v1/health/ready and
// /metrics need to answer "is Redis reachable", and both are called by
// automated systems on their own schedules. Dialling from inside either
// one makes Pad's Redis load a function of how many monitors are pointed
// at it, and makes the answer depend on WHO ASKED — a scrape and a
// readiness check a millisecond apart could disagree. One prober on a
// fixed interval, two readers of its cached result, and the reported value
// is the same for everyone.
//
// WHAT IT DOES NOT DO. It does not gate readiness. /api/v1/health/ready stays
// database-only, deliberately: the REST API, the web UI and every
// ITEM-writing path work with Redis down, so failing readiness on a blip
// would pull healthy replicas out of the load balancer and convert a
// degraded feature into an outage.
//
// "Every write path" would be too strong, and this comment said it:
// POST push answers 503 for a session-targeted push it cannot resolve,
// and 502 push_unconfirmed when the publish itself fails. Those are the
// paths whose whole job IS cross-instance delivery — the thing that is
// actually down. This type exists to make that VISIBLE, not to act on
// it.
//
// TIMEOUT NOTE, measured: go-redis does NOT apply a command context to
// connection ESTABLISHMENT. A call with a 150ms context against a server
// that accepts connections and never answers took 5.0s — the client's
// DialTimeout. So the probe's context bounds the PING once a connection
// exists and nothing else; the dial is bounded by the client's own
// DialTimeout (go-redis v9 default 5s, which cmd/pad takes). That is why
// the probe interval sits well above the dial bound rather than just
// above the probe timeout.
type RedisHealth struct {
	client   *redis.Client
	interval time.Duration
	timeout  time.Duration

	// onProbe, when non-nil, is called after every probe with its result,
	// with no lock held. It is how the Prometheus gauge is written
	// without this type importing the metrics package.
	//
	// It runs on the probe goroutine, so it must not call Stop — that
	// waits for the goroutine it is running on. Set once at construction
	// and never written again, so it needs no lock of its own.
	onProbe func(ok bool)

	mu        sync.RWMutex
	probed    bool
	ok        bool
	lastErr   string
	lastCheck time.Time

	// lifecycleMu serializes Start and Stop ENTIRELY — including Stop's
	// cancel and wait. Guarding only the fields is not enough: a Start
	// racing into the window between Stop's unlock and its wait installs
	// a loop that Stop neither cancels nor can end, and both share one
	// WaitGroup. The two operations are the unit that must be atomic.
	//
	// Deliberately NOT mu: probe() takes mu on the goroutine Stop waits
	// for, so holding mu across wg.Wait would deadlock. Lock order where
	// both are taken is lifecycleMu then mu, never the reverse.
	lifecycleMu sync.Mutex
	started     bool
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

// RedisHealthStatus is the cached probe result, as reported by
// /api/v1/health/ready.
type RedisHealthStatus struct {
	// Reachable is the last probe's verdict. Meaningless until Probed.
	Reachable bool `json:"reachable"`
	// Probed is false in the window between the server accepting traffic
	// and the first probe completing. Reported rather than defaulted:
	// "not measured yet" and "measured, and it is down" are different
	// facts, and only one of them is worth waking someone for.
	Probed bool `json:"probed"`
	// Error is the last probe's failure text, empty when Reachable.
	Error string `json:"error,omitempty"`
	// LastCheck is when the last probe completed, zero before the first.
	LastCheck time.Time `json:"last_check,omitempty"`
}

// NewRedisHealth builds a prober for client. onProbe may be nil.
func NewRedisHealth(client *redis.Client, onProbe func(ok bool)) *RedisHealth {
	return &RedisHealth{
		client:   client,
		interval: redisProbeInterval,
		timeout:  redisProbeTimeout,
		onProbe:  onProbe,
	}
}

// Start begins probing in the background. It runs one probe SYNCHRONOUSLY
// before returning, so a server that has finished starting has a real
// answer rather than a Probed=false window that only closes an interval
// later — the boot case is exactly when an operator is watching.
//
// That is one PING more than strictly needed at boot, since
// cmd/pad/cmd_server.go already pings when it dials and treats a failure
// there as FATAL. Two consequences:
//
//   - The redundancy is deliberate. Reusing the dial-time result would
//     couple this type to its caller's startup sequence for one round
//     trip on a path that runs once per process.
//   - Because that earlier ping is fatal, probe()'s "unreachable at
//     startup" branch cannot fire in the shipped binary. It is kept for
//     an embedder that makes the dial-time check non-fatal.
func (h *RedisHealth) Start() {
	if h == nil || h.client == nil {
		return
	}

	// IDEMPOTENT, and serialized against Stop for the whole operation:
	// without this a second Start leaks the first loop and Stop cancels
	// only the second, so wg.Wait never returns.
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	if h.started {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.started = true
	h.cancel = cancel

	h.probe(ctx)

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ticker := time.NewTicker(h.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.probe(ctx)
			}
		}
	}()
}

// Stop ends probing and waits for the goroutine to exit. Idempotent, and
// safe on a prober that was never started.
//
// MUST NOT be called from an onProbe callback: probe() runs on the
// goroutine Stop waits for, so that self-deadlocks. The callback exists
// to set a gauge, and the contract says so here rather than leaving the
// hazard for whoever writes the second one.
func (h *RedisHealth) Stop() {
	if h == nil {
		return
	}
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()

	if h.cancel == nil {
		h.started = false
		return
	}
	cancel := h.cancel
	h.cancel = nil
	h.started = false

	// Cancel and wait INSIDE the lock, so no Start can interleave and
	// install a loop this Stop will wait for but never cancel.
	cancel()
	h.wg.Wait()
}

func (h *RedisHealth) probe(ctx context.Context) {
	probeCtx, cancel := context.WithTimeout(ctx, h.timeout)
	err := h.client.Ping(probeCtx).Err()
	cancel()

	// A cancelled parent means we are shutting down, not that Redis is
	// gone. Recording it as a failure would flip pad_redis_up to 0 on
	// every clean shutdown and teach whoever alerts on it to ignore the
	// signal.
	if ctx.Err() != nil {
		return
	}

	ok := err == nil

	h.mu.Lock()
	wasProbed, was := h.probed, h.ok
	h.probed = true
	h.ok = ok
	h.lastCheck = time.Now()
	if ok {
		h.lastErr = ""
	} else {
		h.lastErr = err.Error()
	}
	h.mu.Unlock()

	// Log only on TRANSITIONS (and on the first probe), not every tick: a
	// Redis outage lasting an hour would otherwise write 240 identical
	// lines, which is the same log-flood shape the presence renewal
	// warning had to be throttled for.
	switch {
	case !wasProbed && !ok:
		slog.Error("redis: unreachable at startup; activity events stop for ALL clients (no local fan-out fallback), watch notifications and session presence are degraded", "error", err)
	case wasProbed && was && !ok:
		slog.Error("redis: became unreachable; activity events stop for ALL clients (no local fan-out fallback), watch notifications and session presence are degraded", "error", err)
	case wasProbed && !was && ok:
		slog.Info("redis: reachable again")
	}

	if h.onProbe != nil {
		h.onProbe(ok)
	}
}

// Status returns the cached probe result.
func (h *RedisHealth) Status() RedisHealthStatus {
	if h == nil {
		return RedisHealthStatus{}
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return RedisHealthStatus{
		Reachable: h.ok,
		Probed:    h.probed,
		Error:     h.lastErr,
		LastCheck: h.lastCheck,
	}
}
