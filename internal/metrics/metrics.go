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
	// cmd/pad/cmd_server.go. Which of them are live depends on the
	// deployment shape, and the distinction is what alerts are built on
	// (asserted in TestMemoryBusDropCounterIsMeaningfulWithoutRedis):
	//
	//   - pad_watchevents_notifications_dropped_total moves on a
	//     single-process binary too — MemoryBus has the same
	//     slow-subscriber drop and the same observer.
	//   - Sequence GAPS, RESETS and receive-loop exits are Redis-only by
	//     construction on both buses: a MemoryBus assigns its own contiguous
	//     ids and has no subscription to lose.
	//   - RESUME GAPS are NOT, on either bus, and reading them as Redis-only
	//     would be wrong (BUG-2731). A single-process instance restarts, and
	//     a resume carrying a cursor from a previous incarnation is
	//     unservable there for exactly the same reason it is on a cold
	//     replica. Both pad_event_resume_gaps_total and
	//     pad_watchevents_resume_gaps_total move without Redis.
	//   - Resume gaps also have a second source that is not a bus at all:
	//     a cursor the HANDLER could not parse never reaches one. See
	//     server.countResumeGap.
	//   - pad_redis_up is not registered at all without Redis; a zero on
	//     a gauge named "up" asserts something false.
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
	// /api/v1/health/ready, which omits its redis block on the same condition.
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

	// WatchResumeGapsTotal counts resumes this instance could not serve,
	// each of which sends a client sync_required. Distinct from
	// WatchSequenceGapsTotal: that one is a delivery fault, this one is
	// the user-visible consequence of any cursor this instance cannot
	// vouch for — a hole, a cold start, an epoch change, a disagreeing
	// shared counter, or a cursor that could not be parsed at all.
	//
	// Moves on a single-process deployment too (BUG-2731): MemoryBus
	// restarts its id sequence, so a resume from a previous incarnation is
	// genuinely unservable there.
	WatchResumeGapsTotal prometheus.Counter

	// WatchSequenceResetsTotal counts this instance's watch replay
	// coverage being dropped, by reason. Two of the reasons mean the id
	// SPACE changed under us (epoch_change, counter_backward). The other
	// two mean it did not: subscription_resumed, where the outage's
	// notifications demonstrably never arrived; and undecodable_message,
	// where the instance knows only that something it could not read
	// arrived on its channel — it may or may not have been ours, and
	// stopping vouching is the honest answer to not being able to tell.
	// All four make resumes across them answer sync_required. Kept in
	// sync with watchevents' Reset* constants.
	WatchSequenceResetsTotal *prometheus.CounterVec

	// WatchReceiveLoopExitsTotal counts the receive loop stopping. Any
	// non-zero value outside a shutdown means an instance that publishes
	// fine and receives nothing.
	WatchReceiveLoopExitsTotal prometheus.Counter

	// EventResumeGapsTotal counts activity-stream (/api/v1/events) resumes
	// this instance could not serve, each of which sends a client
	// sync_required (BUG-2731). The watch stream's twin is
	// WatchResumeGapsTotal; they are separate counters because the two
	// streams have independent id spaces and separate replay machinery, so
	// one number would make either one's incident undiagnosable.
	//
	// EXPECT A STEP AROUND A DEPLOY AND A RETURN TO BASELINE AFTER IT — of the
	// RATE, not of the counter, which only ever increases within a process.
	// Each
	// instance starts with no coverage, so an early resume against a
	// workspace a replica has not seen yet is a warranted gap. It counts
	// RESUMES rather than clients — a deploy nobody reconnects through does
	// not move it, and one client reconnecting repeatedly moves it repeatedly
	// — so the step's size is not the connected-client count. A rate that
	// does NOT settle is the evidence against BUG-2731's central claim, that
	// the syncs it added are only the warranted ones, and is what to alert on.
	//
	// An increment means the client was told to RECONCILE, which is not the
	// same as a full re-fetch: the web client answers with an incremental
	// /changes delta first (web/src/lib/services/sync.svelte.ts).
	EventResumeGapsTotal prometheus.Counter

	// EventEventsDroppedTotal counts activity events this instance received
	// but could not hand to a live subscriber, by reason. Per-SUBSCRIBER, not
	// per-event: the same event reached every subscriber that was keeping up.
	//
	// The watch stream's twin (WatchNotificationsDroppedTotal) has existed
	// since BUG-2699; this one arrives with BUG-2730, which is also what made
	// the drop honest — the affected subscriber is now told mid-stream.
	//
	// Read it alongside pad_event_midstream_resyncs_total, but NOT as a
	// one-to-one correspondence, in either direction: the gap channel
	// coalesces and the handler rate-limits, so a burst of drops on one
	// connection produces a single announcement; and coverage losses produce
	// announcements with no drop at all. A large drop-to-announcement ratio
	// is one client falling a long way behind, not many clients affected. It
	// does NOT move pad_event_resume_gaps_total, which stayed resume-only so
	// existing alerts keep their meaning.
	EventEventsDroppedTotal *prometheus.CounterVec

	// EventMidstreamResyncsTotal and WatchMidstreamResyncsTotal count clients
	// told MID-STREAM that they have a hole (BUG-2730) — a signal that did not
	// exist before, on connections that stay open.
	//
	// SEPARATE FROM THE RESUME COUNTERS ON PURPOSE. Folding them in would have
	// silently changed what every existing alert on
	// pad_*_resume_gaps_total measures, and mixed-version fleets would report
	// two different populations under one name during a rollout.
	//
	// Counts ANNOUNCEMENTS MADE, which is close to clients told but not
	// identical, and the difference matters when reading it:
	//
	//   - a reset that drops buffers moves this once per live subscriber,
	//     while pad_*_sequence_resets_total moves once. That ratio is the
	//     fan-out, and it is the number to look at when deciding whether a
	//     resync storm is underway.
	//   - a coverage loss on a workspace with NO buffer yet announces while
	//     moving no cause counter at all, deliberately: there was no coverage
	//     to end, but the subscribers still have a hole. So this counter can
	//     move with every cause counter flat.
	//   - a burst of drops on ONE connection moves this once, not once per
	//     drop: the gap channel coalesces and the handler rate-limits.
	//   - the same connection can be counted repeatedly over its life, once
	//     per rate-limit window in which it had a gap.
	//   - it increments before the write, so a client that disappears at that
	//     instant is counted having received nothing. Counting after would
	//     lose every announcement to a client that vanished mid-write, which
	//     is the population most worth seeing.
	EventMidstreamResyncsTotal prometheus.Counter
	WatchMidstreamResyncsTotal prometheus.Counter

	// EventSequenceResetsTotal counts activity-stream coverage resets by
	// reason. SEVEN reasons, listed below in the order they were added.
	//
	// If you add another, the count in this line is the first thing to go
	// stale and the last thing anyone reads — it was already wrong by two when
	// BUG-2738 landed. The authoritative list is the Help string on the
	// counter's construction, which is what an operator actually sees, plus
	// the enumeration in internal/events/observer.go and the table in
	// docs/deployment.md. Those three move together.
	//
	//   subscription_resumed — a pub/sub connection dropped and resubscribed,
	//   so ONE workspace's replay buffer was dropped and resumes across the
	//   outage answer sync_required. This tracks Redis connection health:
	//   expect it during a failover and expect it to stop afterwards.
	//
	//   epoch_change — the shared counter's ID space changed generation, so
	//   EVERY buffer was dropped (the counter is global). Expect a handful at
	//   once, correlated with a deploy or a Redis restart. A steady trickle
	//   means the counter or epoch key is being evicted repeatedly.
	//
	//   counter_backward — an ID arrived at or below a buffer's high-water
	//   mark with no generation change. Expected on phase 1 and during any
	//   mixed-version roll; at or near zero once every publisher is flipped.
	//
	//   undecodable_message — a pub/sub message could not be parsed, so that
	//   workspace's coverage ended rather than the buffer claiming a span with
	//   a hole in it. Expect zero; a non-zero count means something is
	//   publishing onto these channels that is not this installation, or a
	//   payload is being truncated in transit, and the events behind it are
	//   lost.
	//
	//   epoch_regressed — the shared generation counter went BACKWARDS and
	//   stayed there. Two causes now, and they are diagnosed differently.
	//   Usually a Redis failover to a replica that lost writes: expect zero,
	//   one per failover is the mechanism recovering, and a repeating count
	//   means the counter is not durable. Since BUG-2740 it can ALSO be the
	//   generation counter having been found corrupted and REPAIRED, which
	//   reseeds it from wall-clock seconds — above any counted history, but
	//   not necessarily above a counter that a collision or a hand-edit had
	//   pushed higher.
	//
	//   subscription_unconfirmed — a subscription was admitted before Redis
	//   acknowledged the SUBSCRIBE, and the acknowledgement then arrived
	//   (BUG-2747). It reaches this counter only when a buffer existed to drop,
	//   which on that path is the uncommon case; the dependable counter for the
	//   condition is pad_event_subscription_unconfirmed_total, and the two are
	//   read together rather than as substitutes.
	//
	//   THERE IS NO REPAIR-SPECIFIC SIGNAL — the repair happens inside a Lua
	//   script, which cannot log through slog and does not change this
	//   counter's label. The tell is the VALUE: read the generation key, and
	//   a repaired one looks like a unix timestamp (ten digits, ~1.7e9)
	//   rather than a small count of id-space resets. That is a deliberate
	//   property of the seed rather than a coincidence.
	//
	// LABELLED FROM THE START rather than shipped as a bare counter, which
	// BUG-2736 immediately vindicated by adding two more values. An operator
	// acts on the difference: a Redis connection FLAP is expected during a
	// failover and self-resolves, a GENERATION change is expected once per
	// cutover, and a persistent counter_backward on a fully flipped deployment
	// is none of those. An alert built on an unlabelled total cannot separate
	// them.
	EventSequenceResetsTotal *prometheus.CounterVec

	// EventReceiveLoopExitsTotal counts a workspace's Redis subscription loop
	// stopping. Expected at shutdown and whenever the last local subscriber
	// for a workspace leaves, so unlike the watch stream's twin it does NOT
	// stay at zero — read it as a RATE against a stable subscriber count.
	EventReceiveLoopExitsTotal prometheus.Counter

	// EventSubscriptionUnconfirmedTotal counts activity-stream subscriptions
	// admitted before Redis acknowledged the SUBSCRIBE, because the wait for
	// that acknowledgement timed out (BUG-2747).
	//
	// EXPECT ZERO. It is a different signal from a sequence reset and is counted
	// separately: nothing is known to have been lost, but a stream was admitted
	// whose coverage this instance cannot describe. The two are not disjoint —
	// when the acknowledgement finally lands, that path calls through to the
	// coverage drop and so CAN also move pad_event_sequence_resets_total with
	// reason subscription_unconfirmed, though only when a buffer existed to
	// drop.
	//
	// IT COUNTS ESTABLISHMENTS, NOT CLIENTS. One increment is one workspace
	// subscription that timed out, however many subscribers were waiting on it
	// — all of them are told to reconcile when the acknowledgement lands, so
	// the client-side fan-out is visible in
	// pad_event_midstream_resyncs_total, not here.
	//
	// A non-zero rate says the SUBSCRIBE round trip is slow or stalling —
	// the same Redis condition that makes BUG-2748's dial an availability
	// hazard — so read it alongside connect latency rather than alongside
	// pad_event_sequence_resets_total.
	EventSubscriptionUnconfirmedTotal prometheus.Counter

	// EventSubscriptionCycledTotal counts workspace subscriptions torn down and
	// replaced because they received NOTHING — no event, no heartbeat, no
	// acknowledgement — for longer than the bus's idle timeout (BUG-2738).
	//
	// WHAT IT DETECTS IS A HALF-OPEN CONNECTION: no FIN, no RST, just a route
	// that stopped working. go-redis cannot see one (its pub/sub health check
	// writes a PING and never reads the reply), so before this existed such an
	// instance sat receiving nothing while its replay buffer went on looking
	// complete and every resume was answered "caught up".
	//
	// READ THIS ONE RATHER THAN THE idle_timeout RESET LABEL. A cycle reports
	// that label only when a buffer existed to drop, and the incidents this
	// detector exists for skew hard toward having none — a route that wedged
	// early, on a quiet workspace, with nothing yet buffered.
	//
	// IT COUNTS REPLACEMENTS, NOT TEARDOWNS. A cycle that tore a subscription
	// down and then installed nothing — the bus was closing, or the last
	// subscriber left while it dialled — does NOT increment this, because
	// counting a shutdown would manufacture the exact signal an operator reads
	// as "connections are being blackholed". Those teardowns remain visible
	// through the idle_timeout reset reason when a buffer existed to drop.
	//
	// EXPECT ZERO — structurally so on heartbeat phase 1, where the detector
	// does not run, so a zero there says nothing about whether a route has
	// wedged; read heartbeat_phase off the startup log first. On phase 2, a
	// non-zero rate means connections to Redis are being silently blackholed: a
	// NAT idle timeout, a stateful firewall, an overlay network dropping
	// long-lived flows. Check TCP keepalive on the path before touching the
	// interval — a shorter interval hides the cause and a longer one widens the
	// silent window.
	EventSubscriptionCycledTotal prometheus.Counter

	// EventHeartbeatPublishFailuresTotal counts liveness heartbeats this
	// instance could not publish (BUG-2738).
	//
	// READ IT AS "DETECTION IS DEGRADED", not as "a peer is broken". While it
	// fires, idle detection for the affected workspaces is SUSPENDED — silence
	// cannot be read as evidence of a dead receive path when the probe that
	// would have produced the traffic never went out — so a healthy-looking
	// pad_event_subscription_cycled_total means less than usual.
	//
	// PUBLISH and pub/sub use different connection pools, so this points at the
	// OUTBOUND path: pool exhaustion, a wedged outbound route, or Redis
	// refusing writes. An instance in this state is also failing to deliver its
	// own events to every other instance, which is a bigger problem than the
	// one this feature exists to find — read it alongside publish latency and
	// pool saturation rather than alongside the cycle counter.
	//
	// EXPECT ZERO.
	EventHeartbeatPublishFailuresTotal prometheus.Counter

	// SessionPresenceFailuresTotal counts failed presence operations by
	// op. READ THE LABEL — the consequences differ, and in opposite
	// directions, so a generic alert on the total leads a responder to
	// the wrong conclusion:
	//
	// A failure means the operation REPORTED an error, which is not quite
	// the same as the operation not happening: Redis can fail a pipeline
	// or a script after it applied, so the write may have landed anyway.
	// The consequences below are what a failure RISKS, not what it
	// guarantees — which is the right reading for an alert either way.
	//
	//   register / renew  — the session may be MISSING from
	//                       GET /api/v1/sessions and untargetable by a
	//                       session-directed push. Under-reporting.
	//   deregister        — a DEAD session may stay listed until its TTL,
	//                       so the picker over-reports and a push aimed
	//                       at it is accepted and reaches nobody.
	//   list              — the read failed and the caller got a 503; no
	//                       wrong answer was given, just no answer.
	//   prune             — a stale index member survives; the next read
	//                       skips it and the next prune retries. Benign.
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
		Help: "Events HANDED to the event bus. On a Redis-backed bus this counts attempts, not confirmed publishes — Publish returns nothing, so a failed Redis publish is logged and still counted here. See pad_redis_up.",
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

	watchResumeGapsTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_watchevents_resume_gaps_total",
		Help: "Resumes this instance could not serve, each sending a client sync_required. Resume-time only — a live subscriber told mid-stream is counted by pad_watchevents_midstream_resyncs_total instead, so existing alerts on this counter keep their meaning.",
	})

	watchSequenceResetsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_watchevents_sequence_resets_total",
		Help: "Times this instance's watch-stream replay coverage was dropped, by reason: epoch_change (the watch epoch token changed, so the IDs are from a different sequence — an opaque UUID here, not a numeric generation), counter_backward (an ID arrived at or below the high-water mark with the epoch unchanged), subscription_resumed (go-redis reconnected and re-subscribed, so whatever was published during the outage never arrived — expect these during a Redis failover and expect them to stop afterwards), undecodable_message (a message on the watch channel could not be parsed; the instance cannot tell whether it was a notification it should have had or something foreign, which is exactly why it stops vouching — expect zero, and suspect a namespace collision). The first two mean the ID space changed under this instance; the last two mean it did not and this instance can no longer account for part of it. Each also announces to the watch subscribers connected at that moment, so each moves pad_watchevents_midstream_resyncs_total by AT MOST one per such subscriber — at most, because that signal is capacity-1 and coalescing, so a second cause firing before a client acts on the first adds no announcement.",
	}, []string{"reason"})

	watchReceiveLoopExitsTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_watchevents_receive_loop_exits_total",
		Help: "Times the Redis subscription receive loop stopped. Non-zero outside shutdown means this instance receives no notifications at all.",
	})

	eventResumeGapsTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_event_resume_gaps_total",
		Help: "Activity-stream resumes this instance could not serve, each sending a client sync_required. Counts resumes, not clients. Resume-time only — a live subscriber told mid-stream is counted by pad_event_midstream_resyncs_total instead, so existing alerts on this counter keep their meaning. Expect a step around a deploy (cold buffers) returning to baseline; a rate that does not settle is the signal.",
	})

	eventMidstreamResyncsTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_event_midstream_resyncs_total",
		Help: "Activity-stream announcements made to a subscriber MID-STREAM, on a connection that stayed open (BUG-2730). Counts announcements, not causes and not distinct clients. A reset that drops buffers moves this once per live subscriber while pad_event_sequence_resets_total moves once, so that ratio is the fan-out; a coverage loss on a workspace with no buffer yet announces while moving NO cause counter, because there was no coverage to end; and a burst of drops on one connection announces once, because signals coalesce and are rate-limited. A fresh counter rather than folding into pad_event_resume_gaps_total, so alerts on that one keep their meaning.",
	})

	watchMidstreamResyncsTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_watchevents_midstream_resyncs_total",
		Help: "Watch-stream announcements made to a subscriber MID-STREAM, on a connection that stayed open (BUG-2730). Counts announcements, not causes and not distinct clients. Its causes are a slow-subscriber drop (pad_watchevents_notifications_dropped_total) and a received sequence gap or reset (pad_watchevents_sequence_gaps_total, pad_watchevents_sequence_resets_total) — a gap announces to EVERY subscriber, so this can exceed all of them.",
	})

	eventEventsDroppedTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_event_events_dropped_total",
		Help: "Activity events not delivered to a live subscriber, by reason: slow_subscriber (that connection's 64-deep channel was full). Per-subscriber, not per-event. Since BUG-2730 a drop also tells that subscriber, so expect pad_event_midstream_resyncs_total to rise with it — but not one-for-one, and not exclusively: signals coalesce and are rate-limited per connection, so drops exceed the announcements THIS cause produces, while coverage losses add announcements with no drop at all. It does NOT move pad_event_resume_gaps_total, which stayed resume-only.",
	}, []string{"reason"})

	eventSequenceResetsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_event_sequence_resets_total",
		Help: "Times activity-event replay coverage was dropped, by reason: subscription_resumed (a Redis connection flap, one workspace's buffer), epoch_change (the shared counter's ID space changed generation, every buffer), counter_backward (an ID at or below a buffer's high-water mark with no generation change), epoch_regressed (the generation counter went backwards and stayed there — usually a Redis failover to a replica that lost writes, and since BUG-2740 also a corrupted generation key having been repaired and reseeded from wall-clock seconds; read the key to tell them apart, a repaired one looks like a unix timestamp), undecodable_message (a pub/sub message could not be parsed, so that workspace's coverage ended), subscription_unconfirmed (a subscription was admitted before Redis acknowledged the SUBSCRIBE and the acknowledgement then arrived; reaches this counter only when a buffer existed to drop — see pad_event_subscription_unconfirmed_total), idle_timeout (a subscription received nothing at all — no event, no heartbeat, no acknowledgement — for longer than the idle timeout, so this instance stopped vouching for its buffer; it means COVERAGE ENDED, not that the connection was replaced — the replacement is attempted afterwards and can install nothing if the instance is shutting down or the workspace loses its last subscriber, so only pad_event_subscription_cycled_total proves a replacement. It establishes that the socket stopped proving it works, NOT that events were observed going missing, and like subscription_unconfirmed it reaches this counter only when a buffer existed to drop).",
	}, []string{"reason"})

	eventReceiveLoopExitsTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_event_receive_loop_exits_total",
		Help: "Times a workspace's activity subscription loop stopped. Expected at shutdown and when a workspace's last local subscriber leaves — read as a rate against a stable subscriber count.",
	})

	eventSubscriptionUnconfirmedTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_event_subscription_unconfirmed_total",
		Help: "Activity-stream subscriptions admitted before Redis acknowledged the SUBSCRIBE, because the wait timed out. Counts ESTABLISHMENTS, not clients — however many subscribers were waiting on one, it increments once. Expect zero. Nothing is known lost; the stream's coverage is simply undescribable until the acknowledgement lands, at which point every subscriber waiting on it is told to reconcile and pad_event_sequence_resets_total may also move with reason subscription_unconfirmed.",
	})

	eventSubscriptionCycledTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_event_subscription_cycled_total",
		Help: "Activity-stream workspace subscriptions torn down and replaced because nothing arrived on them — no event, no heartbeat, no acknowledgement — within the idle timeout (BUG-2738). Detects a HALF-OPEN connection, which go-redis's pub/sub health check cannot see because it writes a PING without reading the reply. Counts REPLACEMENTS, not teardowns: a cycle that installed nothing because the bus was closing or the workspace emptied does not increment it. Expect zero — and structurally zero on heartbeat phase 1, where the detector does not run at all, so a zero there says nothing about whether a route has wedged (read heartbeat_phase off the startup log). Read THIS rather than pad_event_sequence_resets_total{reason=\"idle_timeout\"}, which moves only when a buffer existed to drop and so under-reports exactly the early-wedge case this detector exists for. A non-zero rate means connections to Redis are being silently blackholed — NAT idle timeout, stateful firewall, overlay network dropping long-lived flows; check TCP keepalive on the path before changing the interval.",
	})

	eventHeartbeatPublishFailuresTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_event_heartbeat_publish_failures_total",
		Help: "Liveness heartbeats this instance could not publish (BUG-2738). Read as DETECTION DEGRADED, not as a peer being broken: while it fires, idle detection for those workspaces is suspended, because silence cannot be read as evidence of a dead receive path when the probe never went out. PUBLISH and pub/sub use different connection pools, so this points at the OUTBOUND path — pool exhaustion, a wedged outbound route, or Redis refusing writes. Such an instance is also failing to deliver its own events to every other instance. Expect zero.",
	})

	sessionPresenceFailuresTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_session_presence_failures_total",
		Help: "Failed session-presence operations by op. READ THE LABEL — register/renew RISK a live session being unlisted and untargetable, deregister risks a DEAD one staying listed, list returns 503, prune is benign. A failure means an error was reported; Redis can fail after applying.",
	}, []string{"op"})

	reg.MustRegister(
		watchNotificationsDroppedTotal,
		watchSequenceGapsTotal,
		watchNotificationsMissedTotal,
		watchResumeGapsTotal,
		watchSequenceResetsTotal,
		watchReceiveLoopExitsTotal,
		eventResumeGapsTotal,
		eventEventsDroppedTotal,
		eventMidstreamResyncsTotal,
		watchMidstreamResyncsTotal,
		eventSequenceResetsTotal,
		eventReceiveLoopExitsTotal,
		eventSubscriptionUnconfirmedTotal,
		eventSubscriptionCycledTotal,
		eventHeartbeatPublishFailuresTotal,
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

		RedisUp:                            redisUp,
		WatchNotificationsDroppedTotal:     watchNotificationsDroppedTotal,
		WatchSequenceGapsTotal:             watchSequenceGapsTotal,
		WatchNotificationsMissedTotal:      watchNotificationsMissedTotal,
		WatchResumeGapsTotal:               watchResumeGapsTotal,
		WatchSequenceResetsTotal:           watchSequenceResetsTotal,
		EventResumeGapsTotal:               eventResumeGapsTotal,
		EventEventsDroppedTotal:            eventEventsDroppedTotal,
		EventMidstreamResyncsTotal:         eventMidstreamResyncsTotal,
		WatchMidstreamResyncsTotal:         watchMidstreamResyncsTotal,
		EventSequenceResetsTotal:           eventSequenceResetsTotal,
		EventReceiveLoopExitsTotal:         eventReceiveLoopExitsTotal,
		EventSubscriptionUnconfirmedTotal:  eventSubscriptionUnconfirmedTotal,
		EventSubscriptionCycledTotal:       eventSubscriptionCycledTotal,
		EventHeartbeatPublishFailuresTotal: eventHeartbeatPublishFailuresTotal,
		WatchReceiveLoopExitsTotal:         watchReceiveLoopExitsTotal,
		SessionPresenceFailuresTotal:       sessionPresenceFailuresTotal,

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

// RegisterStreamConnectionsCollector exposes pad_stream_connections_active
// as a CALLBACK collector, sampled on scrape (BUG-2726).
//
// A collector rather than a gauge somebody pushes to, for the same
// reason RegisterDBCollector is one: zero overhead between scrapes,
// always fresh, and no ordering problem. A per-admit callback has two
// failure modes this does not — running under the gate's lock (a
// deadlock for any callback touching the gate) and landing out of order
// (a stale value last, leaving the gauge permanently BELOW the real
// total, which reads as spare capacity that is not there).
//
// Sampling on scrape is safe HERE in a way a Redis PING would not be: no
// I/O, no side effects, and the value cannot depend on who is asking.
// That distinction is why pad_redis_up is a probed gauge and this is
// not.
//
// total must be safe for concurrent use; it is called from the scrape
// goroutine.
func (m *Metrics) RegisterStreamConnectionsCollector(total func() int) {
	m.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "pad_stream_connections_active",
		Help: "Active streaming connections on THIS INSTANCE across both SSE endpoints — the population PAD_SSE_MAX_CONNECTIONS bounds. Limits are per instance; sum across replicas for the deployment total.",
	}, func() float64 { return float64(total()) }))
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
