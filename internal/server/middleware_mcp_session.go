package server

import (
	"log/slog"
	"sync"
	"time"
)

// MCP session tracker (PLAN-943 TASK-1120).
//
// Replaces the naive "+1 on initialize, -1 on HTTP DELETE" gauge
// accounting that TASK-961 shipped. The naive scheme drifted up over
// time because clients that crash, lose network, or restart mid-
// session never emit the spec'd DELETE — the gauge was monotonically
// non-decreasing under real-world failures.
//
// This tracker keeps an in-memory map keyed by the canonical
// `Mcp-Session-Id` header (set by mcp-go's StreamableHTTPServer on
// every initialize response and echoed by the client on every
// subsequent request). Every request that carries a session-id
// touches the entry's lastSeen; explicit DELETE evicts; and a
// periodic sweeper evicts entries older than the configured TTL.
//
// Gauge semantics:
//
//   - The active-sessions Prometheus gauge is set to len(sessions)
//     by the onChange callback on every state-changing op. Set
//     (not Inc/Dec) so concurrent observers always see a value
//     consistent with the post-op map state — no possibility of
//     gauge ≠ map size after a sweep evicts N at once.
//   - onChange fires only when the size actually changes, so a
//     touch on an existing entry doesn't churn the gauge.
//
// Bounds:
//
//   - One tracker per Server (see s.mcpSessions, wired by
//     startMCPSessionTracker). Cross-process aggregation is not in
//     scope; pad-cloud's Prometheus federates per-node series and
//     the dashboard sums by job.
//   - The map can grow unbounded between sweeps if a flood of
//     sessions opens with no DELETE; bounded in practice by the
//     OAuth token-issuance rate × the TTL window, which is
//     sub-MB even pessimistically. If we ever see this become a
//     real cap, the sweeper interval can be lowered without API
//     impact.

const (
	// defaultMCPSessionTTL is the default eviction window. Conservative
	// enough that a long-idle agent session (Claude Desktop sitting
	// open overnight while the user is in meetings) doesn't get
	// double-counted, but tight enough that a crashed client clears
	// within an hour. Override via PAD_MCP_SESSION_TTL.
	defaultMCPSessionTTL = 30 * time.Minute

	// defaultMCPSessionSweepInterval is how often the sweeper walks
	// the map. Smaller than the TTL so a crashed session is evicted
	// within ttl + sweep_interval at worst. Override via
	// PAD_MCP_SESSION_SWEEP_INTERVAL.
	defaultMCPSessionSweepInterval = 5 * time.Minute
)

// mcpSessionTracker is the session lifecycle bookkeeper described
// above. Field-level concurrency: mu guards sessions; ttl is
// immutable post-construction; stop is signalled-once via stopped.
type mcpSessionTracker struct {
	mu       sync.Mutex
	sessions map[string]time.Time

	ttl      time.Duration
	stop     chan struct{}
	stopped  sync.Once
	onChange func(count int)
}

// newMCPSessionTracker constructs a tracker with the given TTL and
// optional onChange callback. ttl <= 0 falls back to the default;
// onChange may be nil (the tracker still works, just without gauge
// updates — useful in tests).
func newMCPSessionTracker(ttl time.Duration, onChange func(count int)) *mcpSessionTracker {
	if ttl <= 0 {
		ttl = defaultMCPSessionTTL
	}
	return &mcpSessionTracker{
		sessions: make(map[string]time.Time),
		ttl:      ttl,
		stop:     make(chan struct{}),
		onChange: onChange,
	}
}

// touch inserts or refreshes an entry. No-op on empty id (defensive:
// callers pre-filter, but a missing header should never bump anything).
// Fires onChange only when the size actually changes (insert, not
// refresh) so the gauge doesn't churn on every per-session tool call.
func (t *mcpSessionTracker) touch(id string) {
	if id == "" {
		return
	}
	t.mu.Lock()
	_, existed := t.sessions[id]
	t.sessions[id] = time.Now().UTC()
	n := len(t.sessions)
	t.mu.Unlock()
	if !existed && t.onChange != nil {
		t.onChange(n)
	}
}

// evict removes an entry. No-op on empty id or unknown id. Fires
// onChange only when an entry was actually removed.
func (t *mcpSessionTracker) evict(id string) {
	if id == "" {
		return
	}
	t.mu.Lock()
	_, existed := t.sessions[id]
	delete(t.sessions, id)
	n := len(t.sessions)
	t.mu.Unlock()
	if existed && t.onChange != nil {
		t.onChange(n)
	}
}

// sweep walks the map, evicts entries older than ttl, and returns
// the eviction count. onChange fires once at the end with the new
// total — single observation regardless of how many were evicted,
// which avoids spurious gauge oscillation on a large sweep.
func (t *mcpSessionTracker) sweep() int {
	cutoff := time.Now().UTC().Add(-t.ttl)
	t.mu.Lock()
	var evicted int
	for id, last := range t.sessions {
		if last.Before(cutoff) {
			delete(t.sessions, id)
			evicted++
		}
	}
	n := len(t.sessions)
	t.mu.Unlock()
	if evicted > 0 && t.onChange != nil {
		t.onChange(n)
	}
	return evicted
}

// size returns the current entry count. Used by tests and by
// startMCPSessionTracker's initial gauge prime.
func (t *mcpSessionTracker) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.sessions)
}

// run drives the periodic sweeper at the given interval. Exits on
// stop. Tracked via Server.bg so Stop() can drain in-flight sweeps
// before the process exits.
func (t *mcpSessionTracker) run(interval time.Duration) {
	if interval <= 0 {
		interval = defaultMCPSessionSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-ticker.C:
			if n := t.sweep(); n > 0 {
				slog.Debug("mcp session sweep evicted stale entries", "evicted", n, "remaining", t.size())
			}
		}
	}
}

// shutdown signals the sweeper to exit. Idempotent — safe to call
// from Server.Stop alongside other shutdown paths that may have
// already fired during a test that exercises shutdown twice.
func (t *mcpSessionTracker) shutdown() {
	t.stopped.Do(func() {
		close(t.stop)
	})
}

// SetMCPSessionTrackerConfig stashes ttl + sweep-interval overrides
// for the session tracker before it's started. Either may be 0 to
// keep the package default. Must be called before SetMCPTransport
// (which spawns the tracker via startMCPSessionTracker); calling
// after has no effect because the tracker reads these fields once
// at construction time.
//
// Wired by cmd/pad from PAD_MCP_SESSION_TTL /
// PAD_MCP_SESSION_SWEEP_INTERVAL so operators can tune the gauge's
// staleness floor without recompiling.
func (s *Server) SetMCPSessionTrackerConfig(ttl, sweepInterval time.Duration) {
	s.mcpSessionTTL = ttl
	s.mcpSessionSweepInterval = sweepInterval
}

// startMCPSessionTracker constructs the tracker + spawns the sweep
// goroutine. Called once at startup from SetMCPTransport (alongside
// startMCPAuditWriter). No-op if already started — supports the
// test pattern where multiple Server instances over a shared store
// would otherwise double-spawn.
//
// Reads ttl + sweep interval from the Server fields populated by
// SetMCPSessionTrackerConfig. Zero values fall back to the package
// defaults.
func (s *Server) startMCPSessionTracker() {
	if s.mcpSessions != nil {
		return
	}
	onChange := func(int) {} // no-op default — replaced below if metrics are wired
	if s.metrics != nil {
		gauge := s.metrics.MCPActiveSessions
		onChange = func(n int) {
			gauge.Set(float64(n))
		}
	}
	s.mcpSessions = newMCPSessionTracker(s.mcpSessionTTL, onChange)
	sweepInterval := s.mcpSessionSweepInterval
	s.goAsync(func() {
		s.mcpSessions.run(sweepInterval)
	})
}

// stopMCPSessionTracker signals the sweeper to exit. Called from
// Server.Stop. Idempotent.
func (s *Server) stopMCPSessionTracker() {
	if s.mcpSessions == nil {
		return
	}
	s.mcpSessions.shutdown()
}

// trackMCPSession is the per-request hook the audit middleware calls
// after next.ServeHTTP. It pulls the session id from the response
// header (set by mcp-go on initialize responses) or the request
// header (echoed by the client on subsequent requests), then either
// touches or evicts based on the request method.
//
// Method semantics:
//   - HTTP DELETE on /mcp is the spec'd session-end signal. Evict
//     unconditionally; failed DELETEs don't matter (the client
//     considers the session over either way).
//   - Anything else is a per-message call; touch updates lastSeen.
//
// Status filter: only touch on successful or unknown-status responses
// (httpStatus 0 covers the no-status case from inner handlers that
// never called WriteHeader). A failed initialize doesn't open a
// session — touching anyway would re-introduce the original drift bug
// in a subtler form. classifyMCPResult collapses 0+200 to "ok" for
// the audit row's status, so this matches the audit row's view.
//
// No-op when the tracker isn't wired (selfhost / tests).
const mcpSessionIDHeader = "Mcp-Session-Id"

func (s *Server) trackMCPSession(reqHeader, respHeader func(string) string, method string, httpStatus int) {
	if s.mcpSessions == nil {
		return
	}
	id := respHeader(mcpSessionIDHeader)
	if id == "" {
		id = reqHeader(mcpSessionIDHeader)
	}
	if id == "" {
		return
	}
	if method == "DELETE" {
		s.mcpSessions.evict(id)
		return
	}
	if httpStatus != 0 && (httpStatus < 200 || httpStatus >= 300) {
		return
	}
	s.mcpSessions.touch(id)
}
