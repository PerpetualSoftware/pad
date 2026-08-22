// Package metrics provides Prometheus instrumentation for the Pad server.
// It uses a custom registry (not the global default) for test isolation
// and explicit control over exposed metrics.
package metrics

import (
	"database/sql"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics holds all Prometheus collectors and the custom registry.
type Metrics struct {
	Registry *prometheus.Registry

	// HTTP request metrics
	HTTPRequestsTotal   *prometheus.CounterVec
	HTTPRequestDuration *prometheus.HistogramVec
	HTTPResponseSize    *prometheus.HistogramVec

	// SSE connection metrics (single gauge to avoid unbounded label cardinality)
	SSEConnectionsActive *prometheus.Gauge

	// EventBus metrics
	EventBusPublishTotal *prometheus.Counter
	EventBusSubscribers  *prometheus.Gauge

	// Redis operability metrics (BUG-2727). Wired from
	// cmd/pad/cmd_server.go and only meaningful on a deployment with
	// PAD_REDIS_URL set — a single-process binary never touches Redis, so
	// these stay at their zero values there, which is the honest reading
	// rather than an absence.
	//
	// RedisUp is written by internal/server's health prober, NOT sampled
	// on scrape: a collector that dials on every scrape turns a monitoring
	// system into a load generator and makes the metric's value depend on
	// who is asking. It is 1 when the last probe succeeded and 0 when it
	// failed.
	//
	// REGISTERED SEPARATELY, via RegisterRedisUp, so a deployment with no
	// Redis omits the series entirely rather than exporting a permanent 0
	// (codex round 1 P2). A 0 means "Redis is down" to anything scraping
	// it, so registering unconditionally would have every single-process
	// binary alerting on an outage of a dependency it does not have.
	// Absence is the honest signal for "not applicable", and it matches
	// /health/ready, which omits its redis block on the same condition.
	RedisUp prometheus.Gauge

	// WatchNotificationsDroppedTotal counts notifications this instance
	// RECEIVED and could not hand to one of its own subscribers. It does
	// not count go-redis discarding messages before we see them — that
	// shows up as a sequence gap instead; see watchevents.Observer for why
	// the distinction matters when reading these two together.
	WatchNotificationsDroppedTotal *prometheus.CounterVec

	// WatchSequenceGapsTotal counts gap EVENTS;
	// WatchNotificationsMissedTotal counts the notifications those gaps
	// span. Both, because one gap of 500 and 500 gaps of one are very
	// different incidents and a single counter cannot tell them apart.
	WatchSequenceGapsTotal        prometheus.Counter
	WatchNotificationsMissedTotal prometheus.Counter

	// WatchSequenceResetsTotal counts id-space changes (epoch change or
	// the shared counter going backwards) — each one drops this
	// instance's replay buffer, so resumes across it answer
	// sync_required.
	WatchSequenceResetsTotal *prometheus.CounterVec

	// WatchReceiveLoopExitsTotal counts the receive loop stopping. Any
	// non-zero value outside a shutdown means an instance that publishes
	// fine and receives nothing.
	WatchReceiveLoopExitsTotal prometheus.Counter

	// StreamConnectionsActive counts every held streaming connection —
	// BOTH /api/v1/events and /api/v1/events/stream — which is the
	// population PAD_SSE_MAX_CONNECTIONS and PAD_SSE_MAX_PER_USER
	// actually bound (BUG-2726).
	//
	// SSEConnectionsActive above is NOT that number and never was: it is
	// written by the events.EventBus wrapper, so it has only ever counted
	// the workspace stream. Before this change that was every SSE
	// connection Pad had a limit for; it no longer is, so an operator
	// watching it against the global limit would have been reading one
	// endpoint's share of a two-endpoint budget. Both are kept — the old
	// one is still the right number for per-workspace questions — and
	// both now say in their Help which population they cover.
	StreamConnectionsActive prometheus.Gauge

	// SessionPresenceFailuresTotal counts failed presence operations by
	// op. A sustained non-zero rate means sessions are missing from
	// GET /api/v1/sessions and untargetable by a session-directed push,
	// which is invisible to the user beyond a push that quietly reaches
	// nobody.
	SessionPresenceFailuresTotal *prometheus.CounterVec

	// MCP traffic metrics (PLAN-943 TASK-961). Wired from
	// internal/server/middleware_mcp_audit.go (per-request seam) and
	// internal/server/middleware_mcp_auth.go + middleware_auth.go
	// (denial seams).
	//
	// Cardinality note for MCPToolCallsTotal: the user_id label is
	// bounded by the cloud-deployment user count (target hundreds-to-
	// low-thousands during alpha) and the tool label is bounded by the
	// catalog (~7 tools today). 1000 users × 7 tools × 4 statuses =
	// 28k series, well within Prometheus' comfort zone. If we ever
	// open the surface to a much larger user set, drop the user_id
	// label and lean on the audit log for per-user forensics — the
	// audit row already carries that data without a series-explosion
	// risk.
	MCPToolCallsTotal    *prometheus.CounterVec
	MCPToolCallDuration  *prometheus.HistogramVec
	MCPAuthzDenialsTotal *prometheus.CounterVec
	MCPActiveSessions    prometheus.Gauge

	// OAuth flow metrics (PLAN-943 TASK-961). Wired from
	// internal/server/handlers_oauth.go (per-handler seams) and
	// internal/oauth/storage.go (revocation TTL observation).
	OAuthFlowsTotal            *prometheus.CounterVec
	OAuthFlowDuration          *prometheus.HistogramVec
	OAuthTokenRevocationsTotal *prometheus.CounterVec
	OAuthTokenTTLSeconds       prometheus.Histogram
}

// New creates a new Metrics instance with a custom registry and registers
// all application metrics plus Go runtime and process collectors.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	// Go runtime + process collectors (goroutines, memory, GC, file descriptors)
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	httpRequestsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_http_requests_total",
		Help: "Total number of HTTP requests by method, route, and status code.",
	}, []string{"method", "route", "status"})

	httpRequestDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pad_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds by method, route, and status code.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route", "status"})

	httpResponseSize := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pad_http_response_size_bytes",
		Help:    "HTTP response size in bytes by method, route, and status code.",
		Buckets: prometheus.ExponentialBuckets(100, 10, 7), // 100B to 100MB
	}, []string{"method", "route", "status"})

	sseConnectionsActive := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pad_sse_connections_active",
		Help: "Active connections on the workspace activity stream (/api/v1/events) only. See pad_stream_connections_active for all streaming connections.",
	})

	eventBusPublishTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_eventbus_publish_total",
		Help: "Total number of events published to the event bus.",
	})

	eventBusSubscribers := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pad_eventbus_subscribers",
		Help: "Current number of event bus subscribers.",
	})

	// =====================================================================
	// MCP traffic metrics (PLAN-943 TASK-961)
	// =====================================================================

	mcpToolCallsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_mcp_tool_calls_total",
		Help: "Total number of MCP tool-call requests by user, tool, and outcome status.",
	}, []string{"user_id", "tool", "status"})

	// Buckets target the latency spread we expect for MCP tool calls:
	// trivial reads (pad_meta) settle in single-digit ms, item lookups
	// in tens of ms, search / dashboard aggregations in hundreds of
	// ms. The default Prometheus buckets bottom out at 5ms which is
	// already too coarse for the fast path; this set adds 1ms and 2ms
	// buckets so we can see the floor of cache-hit reads, and tops
	// out at 30s to catch runaway dispatches without producing a
	// useless +Inf-only signal.
	mcpToolCallDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pad_mcp_tool_call_duration_seconds",
		Help:    "MCP tool-call duration in seconds by tool.",
		Buckets: []float64{0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	}, []string{"tool"})

	// reason vocabulary, populated from the documented seams:
	//   - "audience_mismatch"          (middleware_mcp_auth.go)
	//   - "rate_limited"               (middleware_mcp_audit.go emitMCPAuditDenied)
	//   - "workspace_not_in_allowlist" (middleware_auth.go RequireWorkspaceAccess)
	//   - "not_a_member"               (middleware_auth.go RequireWorkspaceAccess)
	//   - "tier_mismatch"              (reserved; emitted when scope-policy denies an MCP-origin call)
	mcpAuthzDenialsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_mcp_authz_denials_total",
		Help: "Total number of /mcp authorization denials by reason.",
	}, []string{"reason"})

	// mcp_active_sessions tracks Streamable HTTP sessions inferred
	// from the JSON-RPC `initialize` method (open) and HTTP DELETE
	// (close per MCP spec). Caveat: a session that drops without a
	// DELETE (client crash, network blip) leaves the gauge inflated
	// until the server restarts. Future work could add a TTL sweep
	// keyed on session-id, but for v1 the simple +1/-1 signal is
	// useful enough for alerting on anomalous session counts.
	mcpActiveSessions := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pad_mcp_active_sessions",
		Help: "Number of currently-open MCP Streamable HTTP sessions.",
	})

	// =====================================================================
	// OAuth flow metrics (PLAN-943 TASK-961)
	// =====================================================================

	// stage vocabulary:
	//   - "started"   — /oauth/authorize rendered the consent page
	//   - "completed" — /oauth/authorize/decide approved
	//   - "abandoned" — /oauth/authorize/decide denied
	//   - "failed"    — error path in either /authorize or /authorize/decide
	oauthFlowsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_oauth_flows_total",
		Help: "Total number of OAuth authorization flow events by stage.",
	}, []string{"stage"})

	// Per-handler durations let ops spot a slow consent render (DB
	// lookups for the user's workspace list) vs a slow code-exchange
	// (fosite signing + storage round-trip). Buckets mirror the MCP
	// histogram for cross-comparison.
	oauthFlowDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pad_oauth_flow_duration_seconds",
		Help:    "Duration of OAuth flow handlers in seconds by stage (authorize / decide / token / revoke).",
		Buckets: []float64{0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	}, []string{"stage"})

	// reason vocabulary:
	//   - "user_initiated" — caller hit /oauth/revoke directly
	//   - "rotated"        — refresh-token rotation revoked the parent family
	//   - "replayed"       — replay-detection revoked the family
	// The latter two emit from internal/oauth/storage.go via the
	// OnTokenRevoked observer hook; reasons not yet wired stay zero
	// until their seams are added.
	oauthTokenRevocationsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_oauth_token_revocations_total",
		Help: "Total number of OAuth token revocations by reason.",
	}, []string{"reason"})

	// Token TTL ranges from "revoked seconds after issuance" (theft
	// detection, accidental revoke) to "natural expiry at the issued
	// lifetime" (typically ~1h for access tokens, ~30d for refresh).
	// Buckets span sub-minute through 60d so the histogram captures
	// both fast-revocation events and long-lived refresh-token
	// lifetimes.
	oauthTokenTTLSeconds := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "pad_oauth_token_ttl_seconds",
		Help:    "Distribution of OAuth token lifetimes (issuance to revocation/expiry) in seconds.",
		Buckets: []float64{10, 60, 300, 900, 3600, 21600, 86400, 7 * 86400, 30 * 86400, 60 * 86400},
	})

	redisUp := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pad_redis_up",
		Help: "1 if the last Redis health probe succeeded, 0 if it failed. Not exported when the deployment has no Redis.",
	})

	watchNotificationsDroppedTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_watchevents_notifications_dropped_total",
		Help: "Notifications received by this instance but not delivered to a local subscriber, by reason.",
	}, []string{"reason"})

	watchSequenceGapsTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_watchevents_sequence_gaps_total",
		Help: "Times this instance detected a forward jump in the notification id sequence, i.e. it missed at least one notification.",
	})

	watchNotificationsMissedTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_watchevents_notifications_missed_total",
		Help: "Notifications spanned by detected sequence gaps — how many were missed, where pad_watchevents_sequence_gaps_total counts how often.",
	})

	watchSequenceResetsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_watchevents_sequence_resets_total",
		Help: "Times the notification id space changed under this instance, dropping its replay buffer, by reason.",
	}, []string{"reason"})

	watchReceiveLoopExitsTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_watchevents_receive_loop_exits_total",
		Help: "Times the Redis subscription receive loop stopped. Non-zero outside shutdown means this instance receives no notifications at all.",
	})

	streamConnectionsActive := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pad_stream_connections_active",
		Help: "Active streaming connections across both SSE endpoints — the population bounded by PAD_SSE_MAX_CONNECTIONS.",
	})

	sessionPresenceFailuresTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_session_presence_failures_total",
		Help: "Failed session-presence operations by op. Failures leave sessions unlisted and untargetable.",
	}, []string{"op"})

	reg.MustRegister(
		streamConnectionsActive,
		watchNotificationsDroppedTotal,
		watchSequenceGapsTotal,
		watchNotificationsMissedTotal,
		watchSequenceResetsTotal,
		watchReceiveLoopExitsTotal,
		sessionPresenceFailuresTotal,
		httpRequestsTotal,
		httpRequestDuration,
		httpResponseSize,
		sseConnectionsActive,
		eventBusPublishTotal,
		eventBusSubscribers,
		mcpToolCallsTotal,
		mcpToolCallDuration,
		mcpAuthzDenialsTotal,
		mcpActiveSessions,
		oauthFlowsTotal,
		oauthFlowDuration,
		oauthTokenRevocationsTotal,
		oauthTokenTTLSeconds,
	)

	return &Metrics{
		Registry: reg,

		RedisUp:                        redisUp,
		StreamConnectionsActive:        streamConnectionsActive,
		WatchNotificationsDroppedTotal: watchNotificationsDroppedTotal,
		WatchSequenceGapsTotal:         watchSequenceGapsTotal,
		WatchNotificationsMissedTotal:  watchNotificationsMissedTotal,
		WatchSequenceResetsTotal:       watchSequenceResetsTotal,
		WatchReceiveLoopExitsTotal:     watchReceiveLoopExitsTotal,
		SessionPresenceFailuresTotal:   sessionPresenceFailuresTotal,

		HTTPRequestsTotal:          httpRequestsTotal,
		HTTPRequestDuration:        httpRequestDuration,
		HTTPResponseSize:           httpResponseSize,
		SSEConnectionsActive:       &sseConnectionsActive,
		EventBusPublishTotal:       &eventBusPublishTotal,
		EventBusSubscribers:        &eventBusSubscribers,
		MCPToolCallsTotal:          mcpToolCallsTotal,
		MCPToolCallDuration:        mcpToolCallDuration,
		MCPAuthzDenialsTotal:       mcpAuthzDenialsTotal,
		MCPActiveSessions:          mcpActiveSessions,
		OAuthFlowsTotal:            oauthFlowsTotal,
		OAuthFlowDuration:          oauthFlowDuration,
		OAuthTokenRevocationsTotal: oauthTokenRevocationsTotal,
		OAuthTokenTTLSeconds:       oauthTokenTTLSeconds,
	}
}

// RegisterRedisUp registers the pad_redis_up gauge. Called only by a
// deployment that actually has Redis (cmd/pad/cmd_server.go, inside the
// PAD_REDIS_URL branch) so that a binary without it exports no series at
// all rather than a permanent 0 — see the RedisUp field comment.
//
// Setting m.RedisUp before this is called is harmless: an unregistered
// Prometheus gauge accepts writes and is simply never gathered.
func (m *Metrics) RegisterRedisUp() {
	m.Registry.MustRegister(m.RedisUp)
}

// RegisterDBCollector registers a callback-based collector that exposes
// database connection pool statistics on each Prometheus scrape.
// This is preferred over a periodic goroutine: zero overhead between
// scrapes and always fresh data.
func (m *Metrics) RegisterDBCollector(db *sql.DB) {
	m.Registry.MustRegister(&dbStatsCollector{db: db})
}

// dbStatsCollector implements prometheus.Collector using db.Stats() callbacks.
type dbStatsCollector struct {
	db *sql.DB
}

var (
	dbOpenDesc = prometheus.NewDesc(
		"pad_db_open_connections",
		"Number of open database connections.",
		nil, nil,
	)
	dbIdleDesc = prometheus.NewDesc(
		"pad_db_idle_connections",
		"Number of idle database connections.",
		nil, nil,
	)
	dbInUseDesc = prometheus.NewDesc(
		"pad_db_in_use_connections",
		"Number of in-use database connections.",
		nil, nil,
	)
	dbWaitCountDesc = prometheus.NewDesc(
		"pad_db_wait_count_total",
		"Total number of connections waited for.",
		nil, nil,
	)
	dbWaitDurationDesc = prometheus.NewDesc(
		"pad_db_wait_duration_seconds_total",
		"Total time blocked waiting for a new connection.",
		nil, nil,
	)
)

func (c *dbStatsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- dbOpenDesc
	ch <- dbIdleDesc
	ch <- dbInUseDesc
	ch <- dbWaitCountDesc
	ch <- dbWaitDurationDesc
}

func (c *dbStatsCollector) Collect(ch chan<- prometheus.Metric) {
	stats := c.db.Stats()
	ch <- prometheus.MustNewConstMetric(dbOpenDesc, prometheus.GaugeValue, float64(stats.OpenConnections))
	ch <- prometheus.MustNewConstMetric(dbIdleDesc, prometheus.GaugeValue, float64(stats.Idle))
	ch <- prometheus.MustNewConstMetric(dbInUseDesc, prometheus.GaugeValue, float64(stats.InUse))
	ch <- prometheus.MustNewConstMetric(dbWaitCountDesc, prometheus.CounterValue, float64(stats.WaitCount))
	ch <- prometheus.MustNewConstMetric(dbWaitDurationDesc, prometheus.CounterValue, stats.WaitDuration.Seconds())
}

// =====================================================================
// OAuth active-token gauge (PLAN-943 TASK-961)
// =====================================================================

// OAuthActiveTokensProvider returns the current count of active OAuth
// access tokens. Implemented by *store.Store.CountActiveOAuthAccessTokens.
//
// Defined as a function-typed alias rather than an interface so cmd/pad
// can wire a method value (no interface adapter ceremony) and tests can
// pass a closure that returns a fixed number without needing a fake
// store.
type OAuthActiveTokensProvider func() (int64, error)

// RegisterOAuthActiveTokensCollector exposes pad_oauth_active_tokens as
// a callback-driven gauge. Pulled on every scrape — same pattern as the
// db-stats collector — so the count is always fresh and we never spawn
// a polling goroutine.
//
// Provider errors are logged and the sample is OMITTED for that scrape
// (no metric emitted on the channel). Why not NewInvalidMetric: that
// path makes Registry.Gather() return an error, which promhttp's
// default handler turns into an HTTP 500 — failing the entire scrape
// including every other metric on the registry. A transient SQLite
// blip should drop ONE gauge for one scrape, not the whole observability
// surface. Prometheus tolerates a missing series cleanly (renders as
// a gap; alerting rules can use absent() or stale-for thresholds).
//
// Codex review on the TASK-961 PR caught the prior NewInvalidMetric
// approach — original comment claimed Prometheus would drop the NaN,
// but the actual behavior is whole-scrape failure.
func (m *Metrics) RegisterOAuthActiveTokensCollector(provider OAuthActiveTokensProvider) {
	if provider == nil {
		return
	}
	m.Registry.MustRegister(&oauthActiveTokensCollector{provider: provider})
}

type oauthActiveTokensCollector struct {
	provider OAuthActiveTokensProvider
}

var oauthActiveTokensDesc = prometheus.NewDesc(
	"pad_oauth_active_tokens",
	"Number of active (non-revoked, non-pruned) OAuth access tokens.",
	nil, nil,
)

func (c *oauthActiveTokensCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- oauthActiveTokensDesc
}

func (c *oauthActiveTokensCollector) Collect(ch chan<- prometheus.Metric) {
	count, err := c.provider()
	if err != nil {
		// Log + skip. Emitting NewInvalidMetric here would surface as
		// an error from Registry.Gather() and fail the entire scrape
		// via promhttp's default error handler — a single misbehaving
		// store call must not take out every unrelated metric.
		slog.Warn("oauth active-tokens collector: provider failed; skipping sample for this scrape", "error", err)
		return
	}
	ch <- prometheus.MustNewConstMetric(oauthActiveTokensDesc, prometheus.GaugeValue, float64(count))
}
