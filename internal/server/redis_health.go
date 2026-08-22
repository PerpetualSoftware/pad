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
// database-only, deliberately: the REST API, the web UI and every write
// path work with Redis down, so failing readiness on a Redis blip would
// pull healthy replicas out of the load balancer and convert a degraded
// feature into an outage. This type exists to make the degradation
// VISIBLE, not to act on it.
//
// TIMEOUT NOTE, measured on BUG-2698's day-50 run rather than assumed:
// go-redis does NOT apply a command context to connection ESTABLISHMENT.
// A call with a 150ms context against a server that accepts connections
// and never answers took 5.0s — the client's DialTimeout. So the probe's
// context bounds the PING once a connection exists and nothing else; the
// dial is bounded by the client's own DialTimeout (go-redis v9 default
// 5s, and cmd/pad/cmd_server.go takes the default). That is why the probe
// interval is set well above the dial bound rather than just above the
// probe timeout.
type RedisHealth struct {
	client   *redis.Client
	interval time.Duration
	timeout  time.Duration

	// onProbe, when non-nil, is called after every probe with its result.
	// It is how the Prometheus gauge is written without this type
	// importing the metrics package.
	onProbe func(ok bool)

	mu        sync.RWMutex
	probed    bool
	ok        bool
	lastErr   string
	lastCheck time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
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
// That is one PING more than strictly needed at boot: cmd/pad/cmd_server.go
// already pings when it dials, and treats a failure there as FATAL
// (codex round 8). Two consequences worth stating rather than leaving to
// be rediscovered:
//
//   - The redundancy is deliberate. Reusing the dial-time result would
//     couple this type to its caller's startup sequence for the sake of
//     one round trip on a path that runs once per process.
//   - Because that earlier ping is fatal, the "unreachable at startup"
//     branch in probe() cannot fire in the shipped binary — the process
//     exits first. It is kept because it is reachable for any embedder
//     that makes the dial-time check non-fatal, and because the
//     alternative is a prober whose first-probe behaviour depends on a
//     policy decision made somewhere else.
func (h *RedisHealth) Start() {
	if h == nil || h.client == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
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

// Stop ends probing and waits for the goroutine to exit. Idempotent.
func (h *RedisHealth) Stop() {
	if h == nil || h.cancel == nil {
		return
	}
	h.cancel()
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
