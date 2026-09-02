package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/billing"
	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/email"
	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/metrics"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/oauth"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/textguard"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
	"github.com/PerpetualSoftware/pad/internal/webhooks"
)

type Server struct {
	// afterItemPreRead is a TEST-ONLY seam, nil in production. When set, the
	// item-update handler calls it after loading its own copy of the item and
	// before handing the write to the store — the window in which another
	// writer can commit a change this request did not make (BUG-2776). A hook
	// makes that interleaving deterministic; two real goroutines produce it
	// only sometimes, and a test that reproduces a race sometimes is a
	// detector with an unknown rate rather than a regression test.
	//
	// Set it only while no other request is in flight against this Server: it
	// is read on a request path with no synchronisation.
	//
	// It fires once per item-update REQUEST, so a hook that issues its own
	// update must nil the field for the duration of that nested call or it
	// recurses without end. Every test here does exactly that; the pattern is
	// load-bearing, not incidental (codex round 3).
	afterItemPreRead func(itemID string)

	store                 *store.Store
	router                *chi.Mux
	routerOnce            sync.Once            // ensures setupRouter runs once, after all config
	admitOnce             sync.Once            // lazily builds streamAdmit for servers that never call SetSSELimits
	streamGaugeFor        *metrics.Metrics     // the metrics instance pad_stream_connections_active is registered on (BUG-2726)
	httpServer            *http.Server         // underlying HTTP server (set during ListenAndServe)
	webFS                 fs.FS                // embedded web UI static files (optional)
	events                events.EventBus      // real-time event bus (optional)
	watchEvents           watchevents.Bus      // watch/nudge notification bus (optional, TASK-2533)
	sessionPresence       SessionPresence      // live event-stream connections per user (optional, PLAN-2558 S1)
	redisHealth           *RedisHealth         // cached Redis reachability, reported by /api/v1/health/ready and pad_redis_up (optional, BUG-2727)
	collab                *collab.RoomManager  // Yjs collab room manager (PLAN-1248); optional
	webhooks              *webhooks.Dispatcher // webhook dispatcher (optional)
	email                 *email.Sender        // transactional email sender (optional)
	emailAPIKey           string               // Maileroo API key (used for unsubscribe HMAC)
	emailEnvConfigured    bool                 // email was wired from env vars (SetEmailSender); reconfigureEmail must not tear this down when platform settings clear the key
	rateLimiters          *RateLimiters        // per-endpoint rate limiters
	baseURL               string               // public base URL for generating links (e.g. invite URLs)
	corsOrigins           string               // comma-separated CORS origins (empty = localhost defaults)
	secureCookies         bool                 // set Secure flag on cookies (for TLS deployments)
	metrics               *metrics.Metrics     // Prometheus metrics (optional)
	metricsToken          string               // shared bearer token for /metrics scrapes ("" = loopback-only)
	trustedProxyCIDRs     []*net.IPNet         // CIDRs allowed to set X-Forwarded-For (nil = proxy headers untrusted)
	ipChangeEnforceStrict bool                 // when true, revoke+reject sessions whose client IP OR User-Agent hash differs from the one recorded at session creation
	sseMaxConnections     int                  // global SSE connection limit (0 = unlimited)
	sseMaxPerWorkspace    int                  // per-workspace SSE connection limit (0 = unlimited)
	// midStreamGapCooldownOverride shortens the mid-stream gap rate limit.
	// Zero (production) means midStreamGapCooldown. Tests only; set before
	// serving, read per connection.
	midStreamGapCooldownOverride time.Duration
	sseMaxPerUser                int              // per-user SSE connection limit across BOTH stream endpoints (0 = unlimited, BUG-2726)
	streamAdmit                  *streamAdmission // shared admission gate for both stream endpoints (BUG-2726)
	cloudMode                    bool             // true when running as Pad Cloud (PAD_CLOUD=true or PAD_MODE=cloud)
	cloudSecrets                 []string         // shared secrets for sidecar ↔ pad communication (supports rotation)
	cloudSidecar                 CloudSidecar     // reverse pad → pad-cloud client (e.g. Stripe cancel on account delete); nil = not configured
	billingAvailable             bool             // true when PAD_BILLING_AVAILABLE=true — gates Stripe Checkout CTAs in the web UI (TASK-800)
	version                      string           // release version (e.g. "dev", "1.2.3")
	commit                       string           // git commit hash
	buildTime                    string           // build timestamp
	twoFAChallengeSecret         []byte           // HMAC key for 2FA challenge tokens

	// Attachments storage. Wired via SetAttachments at startup; nil-checked
	// by handlers so a server constructed for a test that doesn't need
	// uploads still compiles and serves every other endpoint.
	attachments        *attachments.Registry
	attachmentMaxBytes int64 // per-file upload cap; 0 = use defaultAttachmentMaxBytes

	// Image processor used by the upload handler to derive thumbnail
	// variants (TASK-878) and by the editor's rotate / crop tools
	// (TASK-879/880). Wired via SetImageProcessor; nil-checked by
	// callers so a server without image processing — e.g. a self-host
	// build that doesn't want the dependency — still serves every
	// other endpoint and stores originals untouched.
	imageProcessor attachments.Processor

	// MCP Streamable HTTP transport (PLAN-943 TASK-950). Wired via
	// SetMCPTransport at startup when the deployment is in cloud mode.
	// nil on self-hosted deployments and on any cloud build that hasn't
	// constructed the MCP server yet — registerMCPRoutes nil-checks so
	// the routes don't mount in either case. See handlers_mcp.go.
	mcpTransport     http.Handler
	mcpPublicURL     string // canonical public URL of the MCP vhost (e.g. https://mcp.getpad.dev)
	mcpAuthServerURL string // canonical URL of the OAuth auth server (e.g. https://app.getpad.dev), TASK-951

	// MCP tool-surface descriptor source (PLAN-1888 / TASK-1891). Wired
	// via SetToolSurfaceHandler at startup from mcp.ToolSurfaceJSON. The
	// injection mirrors SetMCPTransport and exists for the same reason:
	// internal/mcp imports internal/server (dispatch_http.go), so this
	// package CANNOT import internal/mcp to build the catalog JSON
	// itself. cmd/pad/main.go imports both and injects the serializer.
	// nil → GET /api/v1/mcp/tool-surface returns 404 (handler not wired).
	toolSurfaceJSON func() ([]byte, error)

	// OAuth 2.1 authorization server (PLAN-943 TASK-1024 sub-PR B,
	// HTTP handlers in TASK-1025 sub-PR C). Wired via SetOAuthServer
	// at startup when the deployment is in cloud mode + has the
	// fosite-backed server constructed. nil disables the OAuth
	// surface — registerOAuthRoutes nil-checks so the routes don't
	// mount on self-hosted deployments. See handlers_oauth.go.
	oauthServer *oauth.Server

	// claimSecret is the HMAC key for stateless 6-digit claim codes
	// (PLAN-1519 / TASK-1521 / IDEA-1517 §4). Wired by SetClaimSecret
	// at startup — production reuses the deployment's 32-byte
	// encryption key (cfg.EncryptionKey) since both are server-
	// stable secrets with equivalent rotation cadence. nil/short →
	// /api/v1/oauth/claim returns 412 "claim_disabled" on every
	// request, surfacing a clear misconfiguration signal rather than
	// silently accepting forgeable codes.
	claimSecret []byte

	// oauthMetricsWired records whether wireOAuthMetricsObserver has
	// already attached the active-tokens callback collector. Re-
	// registering would panic via prometheus.MustRegister, so the flag
	// guards the one-shot registration. The TTL observer side is
	// idempotent (just a function-pointer set) and runs unconditionally.
	oauthMetricsWired bool

	// MCP audit log async writer (PLAN-943 TASK-960). Spawned by
	// startMCPAuditWriter at startup when MCP is wired; shut down
	// from Server.Stop. nil-safe: every audit-emitting code path
	// nil-checks so MCP-less builds + tests that don't start the
	// writer still work. See middleware_mcp_audit.go.
	mcpAudit *mcpAuditWriter

	// MCP session tracker (PLAN-943 TASK-1120). Replaces the naive
	// +1/-1 active-sessions accounting from TASK-961. Wired by
	// startMCPSessionTracker (called from SetMCPTransport in cloud
	// mode); shut down from Server.Stop alongside the audit writer.
	// nil-safe: trackMCPSession + the gauge-update path both
	// nil-check so non-cloud builds + tests run without the tracker.
	// See middleware_mcp_session.go.
	mcpSessions             *mcpSessionTracker
	mcpSessionTTL           time.Duration // 0 → defaultMCPSessionTTL
	mcpSessionSweepInterval time.Duration // 0 → defaultMCPSessionSweepInterval

	// storageInfoCache memoizes per-workspace storage usage summaries
	// behind a short TTL (storageInfoTTL). Reduces DB load on the
	// Settings → Storage page and quota-aware UI surfaces. Initialized
	// in newServer; never nil so handlers can call get/set without
	// guarding.
	storageInfoCache *storageInfoCache

	// copyItemFn indirects Store.CopyItemAcrossWorkspaces for the
	// cross-workspace copy endpoint (PLAN-2357 / TASK-2365).
	//
	// It exists because DR-13 forbids the endpoint from ever transparently
	// retrying a mutating copy — there is no idempotency key in v1, so a
	// retry after a post-commit failure duplicates the item — and the only
	// falsifiable way to assert "called exactly once" is to count the calls.
	// Several tests also use it to inject the store's typed errors and to
	// land a concurrent mutation deterministically between the handler's
	// authorization and the store call. See handleCopyItem and
	// TestCopyEndpoint_DoesNotRetryOnAmbiguousError.
	//
	// It is nil on every production path — nothing outside package server can
	// set it, no constructor or setter assigns it, and only _test.go files do
	// (Codex round 6: it is compiled into the binary, so "test-only" describes
	// the convention, not a compiler-enforced guarantee).
	copyItemFn func(store.CrossWorkspaceCopyRequest) (*store.CrossWorkspaceCopyResult, error)

	// importBundleMaxBytes caps a single workspace import bundle.
	// 0 → defaultImportBundleMaxBytes (2 GiB). Set via
	// SetImportBundleMaxBytes from cmd/pad/main.go using the
	// PAD_IMPORT_BUNDLE_MAX_BYTES env var so operators with larger
	// exports can opt in without recompiling.
	importBundleMaxBytes int64

	// importArtifactMaxBytes caps a single playbook/convention artifact
	// import (POST /workspaces/{ws}/import-artifact). 0 →
	// defaultImportArtifactMaxBytes (1 MiB). A single artifact is tiny;
	// the cap is the first line of defense against an oversized body
	// being materialized before the YAML-bomb guard runs. Set via
	// SetImportArtifactMaxBytes from cmd/pad/main.go.
	importArtifactMaxBytes int64

	// orphanGC holds the periodic-sweep config + lifecycle for the
	// attachment orphan garbage collector (TASK-886). Configured via
	// SetOrphanGCConfig and started via StartOrphanGC. Stop() signals
	// the loop to exit and waits for it via the bg WaitGroup.
	orphanGC orphanGCConfig

	// opLogGC holds the periodic-sweep config + lifecycle for the
	// Yjs op-log prune sweeper (TASK-1309). Mirrors orphanGC's
	// pattern. Configured via SetOpLogGCConfig + started via
	// StartOpLogGC; Stop() signals the loop via stopOpLogGC.
	opLogGC opLogGCConfig

	// tokenReaper holds the periodic-sweep config + lifecycle for the
	// short-lived-credential reaper (PLAN-1933 DR-5 / TASK-1936).
	// Mirrors orphanGC/opLogGC. Configured via SetTokenReaperConfig +
	// started via StartTokenReaper; Stop() signals the loop via
	// stopTokenReaper.
	tokenReaper tokenReaperConfig

	// workspacePurge holds the periodic-sweep config + lifecycle for the
	// soft-deleted-workspace hard-purge sweeper (TASK-1966 — the 30-day
	// GDPR erasure SLA). Mirrors orphanGC. Configured via
	// SetWorkspacePurgeConfig + started via StartWorkspacePurgeSweeper;
	// Stop() signals the loop via stopWorkspacePurgeSweeper.
	workspacePurge workspacePurgeConfig

	// outboxDrain holds the periodic config + lifecycle for the SPEC-3 event
	// outbox drain (TASK-2714). Mirrors orphanGC. Configured via
	// SetOutboxDrainConfig + started via StartOutboxDrain; Stop() signals the
	// loop via stopOutboxDrain.
	outboxDrain outboxDrainConfig

	// inFlightUploadHashes tracks content_hash values for uploads
	// that have called AttachmentStore.Put but not yet inserted the
	// attachments row. Without this, the orphan GC could delete a
	// blob between Put and CreateAttachment, leaving a live row that
	// references a missing blob (Codex P2 on PR #307 round 1).
	//
	// A plain map + mutex rather than sync.Map: counters need
	// atomic-with-delete semantics (decrement-then-delete-if-zero
	// must be one critical section, not two — sync.Map.CompareAndDelete
	// addresses the entry but not the inc/dec interleaving). Codex
	// P1 round 2 caught the prior sync.Map version racing on
	// release-vs-reload of the same hash.
	inFlightHashesMu sync.Mutex
	inFlightHashes   map[string]int64

	// rowlessNoListerOnce gates the once-per-process notice that a
	// registered attachment backend lacks the Lister capability, leaving
	// the rowless-blob sweep (BUG-2406) inert for it. Logged rather than
	// silently skipped so an operator can tell the leak class is
	// unguarded on that backend; once, so a 24h-cadence sweep doesn't
	// turn it into log spam.
	rowlessNoListerOnce sync.Once

	// rowlessPreDeleteHook, when non-nil, runs inside the rowless
	// sweep's in-flight critical section immediately BEFORE the
	// delete-time row re-check. Test seam only (injectedStageFailure
	// precedent): it lets a test commit a row for the hash at exactly
	// the point that distinguishes the batched subtraction from the
	// re-check, making the TOCTOU leg deterministic.
	rowlessPreDeleteHook func(hash string)

	// bg tracks fire-and-forget goroutines spawned by request handlers
	// (TouchUserActivity in middleware_auth, async email sends, etc.) so
	// the server can drain them before shutdown / test cleanup. Without
	// this, tests using t.TempDir() race the still-running goroutine's
	// SQLite WAL write against TempDir RemoveAll, leaving "directory not
	// empty" cleanup errors in CI. See BUG-842.
	bg sync.WaitGroup

	// First-run bootstrap token (TASK-1167 / PLAN-1166). When non-empty,
	// handleBootstrap accepts the value via the X-Bootstrap-Token header
	// from non-loopback peers (self-host mode only — cloud mode never
	// loads or honors a token, D2/D10). Wired at startup via
	// SetBootstrapToken; cleared by consumeBootstrapToken after the first
	// admin is created.
	//
	// The mutex protects the token field AND the entire validate-token →
	// check-UserCount → CreateUser → consume sequence in handleBootstrap.
	// Two simultaneous valid-token requests with different emails would
	// otherwise create two admins from one token (F5). Bootstrap happens
	// once per install, so the contention window is irrelevant.
	bootstrapMu        sync.Mutex
	bootstrapToken     string
	bootstrapTokenPath string

	// bypassSetupToken, when true, allows the first-admin bootstrap POST to
	// succeed from any IP without an X-Bootstrap-Token header — i.e. the
	// /setup form on the web UI works directly, without the operator having
	// to copy a token out of `docker logs`. Wired from PAD_BYPASS_SETUP_TOKEN
	// at startup via SetBypassSetupToken (cmd/pad/main.go).
	//
	// Self-host only — cloud mode IGNORES this flag entirely (D2/D10 from
	// the original logs-token design: cloud bootstrap stays loopback-only).
	// The UserCount==0 gate in handleBootstrap is unchanged: once the first
	// admin exists, the bootstrap endpoint returns 409 "already initialized"
	// regardless of bypass. This matches the operator's mental model — the
	// flag opens up the *first-run* surface, not registration in general.
	//
	// Operators on trusted networks (Unraid LAN, Tailscale-only deployments,
	// homelabs behind a firewall) typically prefer this; operators with
	// public exposure should leave it off and use the logs-token path.
	bypassSetupToken bool

	// restoreAckFault is a TEST SEAM (always nil in production). When non-nil,
	// handleRestoreItemVersion's collab commit closure invokes it AFTER the restore
	// transaction has durably committed; a non-nil return simulates a Postgres commit
	// whose acknowledgement was lost at the connection boundary (the tx landed, but
	// the driver surfaces an error), exercising BUG-2276 residual 1's commit-outcome
	// reconciliation end-to-end through the real handler.
	restoreAckFault func() error

	// watchPredicatesLoadFault is a TEST SEAM (always nil in production,
	// TASK-2533). When non-nil, loadWatchPredicates calls it before
	// touching the store; a non-nil return short-circuits the real
	// ListWatchesForUser call and is returned as the reload error —
	// exercising GET /api/v1/events/stream's reval-tick error path
	// (codex round 4: a watch-list reload failure must not also skip
	// the identity/visibility refresh) deterministically, without
	// needing to actually break the DB connection mid-test.
	//
	// atomic.Pointer, not a plain func field (codex round 5 finding 2):
	// unlike restoreAckFault — set once, synchronously, before the single
	// HTTP request that will read it, so goroutine-creation's own
	// happens-before edge makes a plain field safe there — this seam is
	// set by a test AFTER the SSE stream's background goroutine is
	// already running and reading it on every reval tick. A plain field
	// written from the test's goroutine while that goroutine reads it
	// concurrently is a genuine, if timing-dependent, data race
	// (verified: restoreAckFault's OWN usage doesn't share this flaw,
	// since it's never touched after the goroutine that reads it starts,
	// so it was intentionally left as a plain field rather than changed
	// too).
	watchPredicatesLoadFault atomic.Pointer[func() error]

	// watchRevalTickOverride is a TEST SEAM (always nil in production,
	// BUG-2570). When non-nil, GET /api/v1/events/stream selects reval
	// ticks from this channel instead of the interval ticker, letting a
	// test drive each revalidation tick explicitly. That is the only way
	// to pin assertions to a SPECIFIC tick: with a free-running ticker,
	// no test ordering can guarantee that an unwanted extra tick doesn't
	// fire between two test steps — an extra SUCCESSFUL tick resets the
	// visibility cache and reloads the watch list, masking exactly the
	// reset-skipped-on-fault regression the reval-fault test guards, and
	// enough extra FAULTING ticks clear the watch set (both observed as
	// codex-round findings on BUG-2570's first fix attempt).
	//
	// Same atomic.Pointer rationale as watchPredicatesLoadFault above:
	// read by the stream's background goroutine (once, at stream setup)
	// while tests may write it. Tests must set it BEFORE connecting the
	// stream they want to drive — a write after setup is not observed.
	watchRevalTickOverride atomic.Pointer[chan time.Time]
}

// goAsync spawns fn in a goroutine that's tracked by s.bg, so Stop() can
// wait for in-flight background work to finish. Use this for any
// fire-and-forget work that touches the database, filesystem, or external
// services from inside a request handler — never bare `go func() {...}()`.
func (s *Server) goAsync(fn func()) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		// Recover from panics in fn so a single bad background task
		// (e.g. deriveThumbnails hitting a Go image-decoder panic on a
		// crafted upload, or an email send) can't crash the whole
		// single-binary server for every tenant. chi's Recoverer only
		// covers request goroutines, not these detached ones. The
		// deferred Done() above still fires because recover() keeps the
		// goroutine from unwinding past this point.
		defer func() {
			if r := recover(); r != nil {
				slog.Error("background task panicked",
					"panic", r,
					"stack", string(debug.Stack()))
			}
		}()
		fn()
	}()
}

// recoverSweeper is the panic firewall for the long-running background
// sweeper loops (orphan GC, op-log GC, token reaper, workspace purge).
// Each of those manages its own s.bg.Add/Done + stop-channel lifecycle,
// so — unlike fire-and-forget work — they can't just route through
// goAsync without breaking their shutdown handling or double-counting
// s.bg. Instead each spawns `defer s.recoverSweeper("<name>")` as a
// deferred call INSIDE its goroutine (BUG-2071): a panic in the loop
// body is logged with a stack (matching goAsync's style) and unwinds
// cleanly, the goroutine's own deferred s.bg.Done() still fires because
// recover() stops the unwind here, and Stop() still returns. Without it a
// panic in any sweeper takes down the whole single-binary server for
// every tenant. Must be `defer`-called directly in the goroutine for
// recover() to catch the panic.
func (s *Server) recoverSweeper(name string) {
	if r := recover(); r != nil {
		slog.Error("background sweeper panicked",
			"sweeper", name,
			"panic", r,
			"stack", string(debug.Stack()))
	}
}

// Stop waits for all background goroutines started via goAsync to finish
// AND drains the rate-limiter cleanup goroutines spawned at construction
// time (BUG-851). Safe to call multiple times. Should be called before
// Store.Close() so in-flight DB writes don't race a closed connection
// (or worse, the SQLite -wal/-shm file removal in t.TempDir cleanup).
func (s *Server) Stop() {
	// Signal long-running background loops (orphan GC, etc.) to exit.
	// Each loop registers itself on s.bg, so the Wait() below blocks
	// until they actually finish and any in-flight goroutines drain.
	s.stopOrphanGC()
	// Yjs op-log prune sweeper (TASK-1309). Same lifecycle pattern;
	// signals BEFORE Wait() so the goroutine sees the close and exits.
	s.stopOpLogGC()
	// Short-lived-credential reaper (PLAN-1933 DR-5 / TASK-1936). Same
	// lifecycle pattern; signal BEFORE Wait() so the goroutine exits.
	s.stopTokenReaper()
	// Soft-deleted-workspace hard-purge sweeper (TASK-1966). Same
	// lifecycle pattern; signal BEFORE Wait() so the goroutine exits.
	s.stopWorkspacePurgeSweeper()
	// SPEC-3 event outbox drain (TASK-2714). Same lifecycle pattern; an
	// in-flight delivery is tracked on s.bg and awaited below.
	s.stopOutboxDrain()
	// MCP audit writer / sweeper run on s.bg too. Signal first so
	// the workers see the close BEFORE Wait() blocks; without the
	// signal Wait would hang forever on the writer's blocking
	// queue receive.
	s.stopMCPAuditWriter()
	// MCP session tracker (TASK-1120) runs its sweeper on s.bg too.
	// Order with the audit writer doesn't matter — both are
	// independent goroutines; we just need the close BEFORE Wait().
	s.stopMCPSessionTracker()
	// Close the collab room manager BEFORE bg.Wait() so any in-flight
	// op-log GC sweep (TASK-1309) blocked on a per-item lock behind
	// an active Join can drain. collab.Close() tears down the Joins
	// (their WS readLoops return, runConn unwinds, itemLocks
	// release), which unblocks the GC's per-item PruneItemOpLogIfDormantBefore
	// call. Without this ordering, Stop() can deadlock: GC waits on
	// itemLock; Join holds itemLock until WS closes; WS only closes
	// when collab.Close() runs; collab.Close() only runs after
	// bg.Wait(); bg.Wait() never returns because GC is stuck.
	// Per Codex review of TASK-1309 [P2]. nil-safe: collab is optional.
	if s.collab != nil {
		s.collab.Close()
	}
	s.bg.Wait()
	// Watch/nudge bus (BUG-2651). Closed AFTER bg.Wait() so a background
	// producer cannot publish into a bus that is already tearing down; both
	// implementations are safe if one does anyway (MemoryBus finds no
	// subscribers, RedisBus finds a cancelled context and fails closed).
	//
	// This matters more than it did for MemoryBus, whose Close only dropped
	// channels: RedisBus holds a receive goroutine and a Redis subscription
	// from construction, so skipping it leaks both for the process's life
	// (Codex round 1 P2). Closing also closes every subscriber channel, which
	// is how a still-open SSE stream learns to unwind.
	if s.watchEvents != nil {
		s.watchEvents.Close()
	}
	s.rateLimiters.Stop() // nil-safe via the RateLimiters receiver guard
}

func New(s *store.Store) *Server {
	rl := NewRateLimiters()
	// PAD_DISABLE_RATE_LIMITS turns off ALL HTTP rate limiting when set to a
	// truthy value. It exists ONLY for the E2E harness (BUG-2089): every
	// Playwright test shares one loopback IP (127.0.0.1), so the auth limiter
	// (5 logins/min/IP) trips the moment a spec logs in a couple of browser
	// clients — collab-persistence.spec.ts logs in two per test. A nil
	// rateLimiters makes RateLimit() a pass-through (see middleware_ratelimit.go
	// line 379), and Stop() + the MCP path are already nil-safe. Never set this
	// in production or self-host; it's an explicit opt-in so it can't flip on
	// by accident.
	if disabled, _ := strconv.ParseBool(os.Getenv("PAD_DISABLE_RATE_LIMITS")); disabled {
		rl = nil
	}
	return &Server{
		store:            s,
		rateLimiters:     rl,
		storageInfoCache: newStorageInfoCache(storageInfoTTL),
	}
}

// Init2FASecret loads the 2FA challenge signing key from platform_settings.
// If no key exists (first run), a new random key is generated and persisted.
// This must be called before the server handles requests so that challenge
// tokens survive process restarts and work across multiple instances.
func (s *Server) Init2FASecret() error {
	const settingKey = "2fa_challenge_secret"

	existing, err := s.store.GetPlatformSetting(settingKey)
	if err != nil {
		return fmt.Errorf("load 2FA secret: %w", err)
	}

	if existing != "" {
		decoded, err := base64.StdEncoding.DecodeString(existing)
		if err != nil {
			return fmt.Errorf("decode 2FA secret: %w", err)
		}
		s.twoFAChallengeSecret = decoded
		return nil
	}

	// First run — generate and persist a new secret.
	// Multiple instances may race here on a fresh database; after persisting,
	// re-read the winning value so all instances converge on the same key.
	secret, err := generateTwoFASecret()
	if err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(secret)
	if err := s.store.SetPlatformSetting(settingKey, encoded); err != nil {
		return fmt.Errorf("persist 2FA secret: %w", err)
	}

	// Re-read to pick up whichever instance won the race (upsert may have
	// been overwritten by a concurrent instance between our check and write).
	final, err := s.store.GetPlatformSetting(settingKey)
	if err != nil {
		return fmt.Errorf("re-read 2FA secret: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(final)
	if err != nil {
		return fmt.Errorf("decode 2FA secret after re-read: %w", err)
	}
	s.twoFAChallengeSecret = decoded
	slog.Info("initialized 2FA challenge signing key")
	return nil
}

// SetCloudMode enables cloud mode with the shared sidecar secret(s).
// Accepts a comma-separated list of secrets for rotation support:
// "new-key,old-key" — both are accepted for INBOUND calls from pad-cloud.
// The OUTBOUND direction (pad → pad-cloud, see SetCloudSidecar) is
// configured separately via PAD_CLOUD_OUTBOUND_SECRET or derived from the
// last entry of this list — see cmd/pad/main.go for the resolution order.
func (s *Server) SetCloudMode(secret string) {
	s.cloudMode = true
	for _, k := range strings.Split(secret, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			s.cloudSecrets = append(s.cloudSecrets, k)
		}
	}
	// Propagate to the email sender so transactional emails carry the
	// getpad.dev marketing footer (docs/brand.md §7) on Cloud installs.
	// Self-hosted deployments leave cloudMode false on the sender, keeping
	// outgoing mail neutral so operators can ship under their own brand.
	if s.email != nil {
		s.email.SetCloudMode(true)
	}
}

// CloudSidecar is the reverse pad → pad-cloud client interface. Concrete
// implementation lives in internal/billing so server has no direct Stripe
// dependency. Kept as an interface so tests can inject fakes without
// spinning up a real HTTP server or touching Stripe.
type CloudSidecar interface {
	// CancelCustomer asks pad-cloud to cancel every active Stripe subscription
	// for customerID and then delete the Stripe customer object. Used by
	// handleDeleteAccount to cascade account deletion through to Stripe billing
	// (TASK-690).
	//
	// Failure contract: any non-nil error means the caller MUST abort the
	// local delete. pad-cloud normalizes Stripe's "already gone" cases to a
	// 200 on its side (see pad-cloud stripe.go isStripeAlreadyGone), so
	// every error we see here is a real failure — transport, 4xx (ops
	// misconfig), or 5xx (upstream breakage). Continuing after an error
	// would wipe the user's StripeCustomerID while leaving the subscription
	// billing, which is exactly the regression TASK-690 exists to prevent.
	CancelCustomer(customerID string) error

	// GetBillingMetrics fetches an aggregated Stripe-derived snapshot from
	// pad-cloud's /admin/metrics/billing endpoint (active subs, MRR, ARR,
	// churn, cancellations). Used by handleAdminBillingStats to power the
	// admin Billing dashboard (TASK-827 / PLAN-825).
	//
	// Failure contract: returns an error on transport failure or non-200
	// status. The admin handler treats any error as "degrade to local-only"
	// and surfaces the distinction in its response via cloud_unreachable —
	// it never propagates the upstream failure to the operator's browser.
	GetBillingMetrics() (*billing.BillingMetricsResponse, error)
}

// SetCloudSidecar installs the reverse pad → pad-cloud client. Called from
// cmd/pad/main.go when PAD_CLOUD_SIDECAR_URL + PAD_CLOUD_SECRET are set.
// When unset, handleDeleteAccount skips the Stripe cancel step (self-hosted
// deploys that don't run a Stripe-backed sidecar have nothing to cascade).
func (s *Server) SetCloudSidecar(c CloudSidecar) {
	s.cloudSidecar = c
}

// SetBillingAvailable marks this deployment as having Stripe Checkout wired
// up. Called from cmd/pad/main.go when PAD_BILLING_AVAILABLE=true is set.
// When false (the default), the session payload advertises billing_available=false
// so the web UI hides Stripe CTAs rather than dead-ending at a 503. TASK-800.
func (s *Server) SetBillingAvailable(v bool) {
	s.billingAvailable = v
}

// IsCloud reports whether the server is running in cloud mode.
func (s *Server) IsCloud() bool {
	return s.cloudMode
}

// SetVersion stores the build version info for the health endpoint.
func (s *Server) SetVersion(version, commit, buildTime string) {
	s.version = version
	s.commit = commit
	s.buildTime = buildTime
}

// SetBaseURL sets the public base URL used for generating shareable links.
//
// If the supplied URL has an unspecified bind-all host ("0.0.0.0", "::",
// "[::]"), this logs a WARN: such a URL is the right thing to *bind* to
// but the wrong thing to *send* to a recipient (their browser cannot
// resolve 0.0.0.0 / :: as a connect target). Callers shipping email
// links from such a deployment should set PAD_URL or PUBLIC_URL to the
// real public hostname (e.g. https://app.getpad.dev). See BUG-899.
func (s *Server) SetBaseURL(rawURL string) {
	s.baseURL = strings.TrimRight(rawURL, "/")
	if s.baseURL == "" {
		return
	}
	if u, err := url.Parse(s.baseURL); err == nil {
		switch u.Hostname() {
		case "", "0.0.0.0", "::":
			slog.Warn("server base URL has an unspecified host; emailed links (password reset, invites, share links) will not be reachable. Set PAD_URL or PUBLIC_URL to the deployment's public URL (e.g. https://app.getpad.dev).", "base_url", s.baseURL)
		}
	}
}

// specialUseTLDs is the (finite) set of reserved top-level names from the IANA
// Special-Use Domain Names registry + related RFCs that never resolve to a
// public web host, so an emailed link using one is undeliverable. Matched on
// the final label so example.com (public) is allowed while foo.example /
// foo.test / bar.internal / pad.home.arpa / x.onion / y.alt (non-public) are
// rejected. Sources: RFC 6761 (localhost/invalid/test/example), RFC 6762
// (local), RFC 8375 (home.arpa) + the "arpa" infrastructure TLD, RFC 7686
// (onion), RFC 9476 (alt), and ICANN-reserved "internal".
var specialUseTLDs = map[string]bool{
	"localhost": true, "local": true, "internal": true,
	"invalid": true, "test": true, "example": true, "arpa": true,
	"onion": true, "alt": true,
}

// hasUsableBaseURL reports whether s.baseURL is a URL a verification-email
// recipient on the public internet can actually reach. It supersets the
// unreachable-host warning in SetBaseURL (BUG-899): the bind-all hosts
// 0.0.0.0 / :: are the right thing to bind() to but the wrong thing to email,
// and so is every other host an external recipient can't resolve or route to.
//
// This gates the cloud self-serve signup path (PLAN-1933 DR-6): creating an
// UNVERIFIED user whose only way out of the write-lock is an emailed link we
// can't deliver would strand them permanently. So "usable" is conservative —
// anything not clearly a public web endpoint disqualifies self-serve signup:
//
//   - scheme must be http/https (a browser can't follow ftp://, file://, …);
//   - no query/fragment (the link is built by concatenation, so either would
//     push the /verify-email/<token> route into the query/fragment);
//   - a present port must be a valid TCP port (1–65535);
//   - the host must be a valid public DNS FQDN, NOT a literal IP. A real cloud
//     verification endpoint is a hostname (Pad Cloud is app.getpad.dev);
//     bare-IP base URLs aren't used for emailed links, and exhaustively
//     enumerating every non-public IP range (loopback / private / CGNAT /
//     TEST-NET / 6to4 / benchmarking / reserved / … across IPv4 and IPv6) is a
//     losing game — so we require a hostname and fail closed on any IP literal.
//
// No usable base URL → no self-serve signup (registration stays closed rather
// than minting a write-locked user). Self-host and admin/invitation signup are
// unaffected either way.
func (s *Server) hasUsableBaseURL() bool {
	if s.baseURL == "" {
		return false
	}
	u, err := url.Parse(s.baseURL)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	// The verification link is built by concatenation (baseURL +
	// "/verify-email/" + token), so a base URL carrying a query or fragment
	// would push the route into the query/fragment and break the link.
	if u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	// A present port must be a valid TCP port (1–65535); url.Parse accepts
	// out-of-range numeric ports that no client can actually connect to.
	if p := u.Port(); p != "" {
		n, perr := strconv.Atoi(p)
		if perr != nil || n < 1 || n > 65535 {
			return false
		}
	}

	// Reject literal IPs outright — a usable public verification endpoint is a
	// DNS hostname, and "is this IP publicly reachable" is not decidable from a
	// finite denylist. Fail closed on any IP.
	if _, aerr := netip.ParseAddr(host); aerr == nil {
		return false
	}

	// The host must be a syntactically-valid, multi-label public FQDN (rejects
	// malformed hosts like ".com", "foo..com", "-a.com" and special-use TLDs).
	return isPublicDNSName(host)
}

// isPublicDNSName reports whether host is a syntactically-valid public FQDN
// (RFC 1123 labels) whose TLD is neither a special-use reserved name nor
// all-numeric. Empty labels, over-length labels, and invalid characters are
// rejected. Assumes host is not an IP literal (that's handled before this is
// called). Punycode/IDN TLDs (xn--…) are accepted since they aren't all-digit.
func isPublicDNSName(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !isValidDNSLabel(l) {
			return false
		}
	}
	tld := labels[len(labels)-1]
	if specialUseTLDs[tld] || isAllDigits(tld) {
		return false
	}
	return true
}

// isValidDNSLabel reports whether l is a valid RFC 1123 hostname label:
// 1–63 chars of [a-z0-9-], not starting or ending with a hyphen.
func isValidDNSLabel(l string) bool {
	if len(l) == 0 || len(l) > 63 {
		return false
	}
	if l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

// isAllDigits reports whether s is non-empty and entirely ASCII digits. A
// public TLD is never all-numeric (RFC 3696), so an all-digit final label
// signals a malformed host rather than a reachable name.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// emailConfigured reports whether this instance can actually SEND an emailed
// link — a sender is wired AND the public base URL is usable. This is the
// DR-6 gate for cloud email self-registration: the sender-only check (s.email
// != nil) is insufficient because link generation also needs a reachable
// public base URL (see handleRegister's verification email + BUG-899).
func (s *Server) emailConfigured() bool {
	return s.email != nil && s.hasUsableBaseURL()
}

// SetEventBus attaches an event bus for real-time SSE streaming.
func (s *Server) SetEventBus(bus events.EventBus) {
	s.events = bus
}

// SetWatchEventsBus attaches the watch/nudge notification bus consumed by
// GET /api/v1/events/stream (TASK-2533). Nil-checked by every producer and
// by the stream handler, so a server constructed without one (e.g. a test
// that doesn't exercise watches) still serves every other endpoint.
func (s *Server) SetWatchEventsBus(bus watchevents.Bus) {
	s.watchEvents = bus
}

// SetSessionPresence attaches the live-session registry read by
// GET /api/v1/sessions and written by GET /api/v1/events/stream
// (PLAN-2558 S1). Nil-checked at both ends, so a server constructed
// without one still streams events — it just can't answer "who is
// listening?", and says so with a 503 rather than an empty list (see
// handleListSessions).
func (s *Server) SetSessionPresence(p SessionPresence) {
	s.sessionPresence = p
}

// SetRedisHealth attaches the Redis reachability prober (BUG-2727). Nil
// on a deployment with no Redis, in which case /api/v1/health/ready reports no
// redis block at all — the honest shape, since "no Redis configured" and
// "Redis configured and down" are different states and a `false` would
// merge them.
//
// It does NOT make readiness depend on Redis; see RedisHealth's doc
// comment for why that is deliberate.
func (s *Server) SetRedisHealth(h *RedisHealth) {
	s.redisHealth = h
}

// SetCollabRoomManager attaches a Yjs collab RoomManager (PLAN-1248).
// When set, the /api/v1/collab/{itemID} WebSocket endpoint hands new
// connections to the manager for op-log replay + fan-out. When nil,
// the endpoint exists but answers 503 — that's intentional so a
// self-host build that wants the editor without collab can leave
// this unwired without surfacing surprise behaviour.
func (s *Server) SetCollabRoomManager(rm *collab.RoomManager) {
	s.collab = rm
}

// SetWebhookDispatcher attaches a webhook dispatcher for outgoing
// notifications. Delivery goroutines are routed through s.goAsync so they're
// tracked on s.bg — Server.Stop() waits for in-flight deliveries (closing the
// BUG-842 shutdown race where a detached delivery writes to a closed store)
// and inherits goAsync's panic recovery (BUG-2011).
func (s *Server) SetWebhookDispatcher(d *webhooks.Dispatcher) {
	if d != nil {
		d.SetSpawn(s.goAsync)
	}
	s.webhooks = d
}

// SetEmailSender attaches a transactional email sender.
// The apiKey is stored separately for deriving the unsubscribe HMAC secret.
//
// If the server is already in cloud mode when this is called (i.e.
// SetCloudMode ran before email config arrived from main.go), propagate
// the flag so the new sender adds the getpad.dev marketing footer to
// outgoing emails. Without this, the cloud-mode flag would silently
// fail to take effect when callers wired email and cloud mode in
// either order.
func (s *Server) SetEmailSender(e *email.Sender, apiKey ...string) {
	s.email = e
	// A sender wired here comes from an out-of-band source (env vars at startup).
	// Mark it so reconfigureEmail leaves it in place when platform settings carry
	// no key — env is the deployment baseline, not something the admin UI disables.
	s.emailEnvConfigured = e != nil
	if len(apiKey) > 0 {
		s.emailAPIKey = apiKey[0]
	}
	if s.cloudMode && s.email != nil {
		s.email.SetCloudMode(true)
	}
}

// SetCORSOrigins configures allowed CORS origins (comma-separated).
func (s *Server) SetCORSOrigins(origins string) {
	s.corsOrigins = origins
}

// SetAttachments wires the attachment storage Registry that the upload
// and download handlers use. Pass maxBytes = 0 to keep the
// defaultAttachmentMaxBytes ceiling (25 MiB).
func (s *Server) SetAttachments(reg *attachments.Registry, maxBytes int64) {
	s.attachments = reg
	s.attachmentMaxBytes = maxBytes
}

// SetImageProcessor wires the image processor that the upload handler
// uses to derive thumbnail variants (TASK-878). Optional — without it
// uploads still succeed but no thumbnails are generated; the
// download handler's variant fallback path returns the original blob.
// The capabilities endpoint reflects whichever processor is wired.
func (s *Server) SetImageProcessor(p attachments.Processor) {
	s.imageProcessor = p
}

// markUploadInFlight increments the in-flight counter for a content
// hash. Returns a release func the caller MUST defer; the release
// decrements and removes the entry once it hits zero. Used by the
// upload handler to fence Put + CreateAttachment against orphan-GC
// blob deletions of the same hash.
//
// Increment + map-store + decrement + delete all run under one
// mutex so a concurrent uploadInFlight call can't observe a stale
// "0" between the last release-decrement and the next-upload
// increment. The earlier sync.Map version split increment from
// LoadOrStore-then-atomic-add and missed that window (Codex P1 on
// PR #307 round 2).
func (s *Server) markUploadInFlight(hash string) func() {
	s.inFlightHashesMu.Lock()
	if s.inFlightHashes == nil {
		s.inFlightHashes = make(map[string]int64)
	}
	s.inFlightHashes[hash]++
	s.inFlightHashesMu.Unlock()
	return func() {
		s.inFlightHashesMu.Lock()
		defer s.inFlightHashesMu.Unlock()
		s.inFlightHashes[hash]--
		if s.inFlightHashes[hash] <= 0 {
			delete(s.inFlightHashes, hash)
		}
	}
}

// uploadInFlight reports whether any upload is currently materializing
// a blob with the given hash. The orphan GC consults this before
// deleting a blob — if an upload just finished Put but hasn't
// inserted the row yet, GC must NOT reclaim the blob.
func (s *Server) uploadInFlight(hash string) bool {
	s.inFlightHashesMu.Lock()
	defer s.inFlightHashesMu.Unlock()
	return s.inFlightHashes[hash] > 0
}

// SetImportBundleMaxBytes overrides the default 2 GiB cap on a
// single workspace import bundle. Set to 0 to fall back to the
// default. Wired from PAD_IMPORT_BUNDLE_MAX_BYTES in cmd/pad/main.go
// so operators with workspaces over 2 GiB can opt in without
// recompiling. Larger caps trade memory headroom (one blob in
// flight at a time, ≤25 MiB) for a longer import wall-clock.
func (s *Server) SetImportBundleMaxBytes(n int64) {
	s.importBundleMaxBytes = n
}

// SetImportArtifactMaxBytes overrides the default 1 MiB cap on a single
// playbook/convention artifact import. Set to 0 to fall back to the
// default. Wired from PAD_IMPORT_ARTIFACT_MAX_BYTES in cmd/pad/main.go.
func (s *Server) SetImportArtifactMaxBytes(n int64) {
	s.importArtifactMaxBytes = n
}

// SetSecureCookies enables the Secure flag on all cookies.
func (s *Server) SetSecureCookies(secure bool) {
	s.secureCookies = secure
}

// countResumeGap records a resume this instance refused to serve, for the gaps
// the BUS never sees (BUG-2731, codex round 12).
//
// The buses count their own coverage-based refusals, which is where most of
// them happen. A cursor we could not even PARSE never reaches a bus, so
// without this the counters undercount exactly the resyncs an operator is most
// likely to be asked about — a client stuck in a resync loop because it keeps
// sending a cursor nobody can read. No double counting: these paths return
// before any bus call.
//
// Split by stream, matching the two counters' separate identities.
func (s *Server) countResumeGap(activity bool) {
	if s.metrics == nil {
		return
	}
	if activity {
		s.metrics.EventResumeGapsTotal.Inc()
		return
	}
	s.metrics.WatchResumeGapsTotal.Inc()
}

// countMidStreamResync records a client told MID-STREAM that it missed
// events, on a connection that stayed open (BUG-2730).
//
// Deliberately NOT countResumeGap: that counter's population is resumes this
// instance could not serve, and existing alerts are written against it.
// Widening it in place would have changed what those alerts measure without
// changing their name, and a mixed-version fleet would report two populations
// under one metric for the length of a rollout.
func (s *Server) countMidStreamResync(activity bool) {
	if s.metrics == nil {
		return
	}
	if activity {
		s.metrics.EventMidstreamResyncsTotal.Inc()
		return
	}
	s.metrics.WatchMidstreamResyncsTotal.Inc()
}

// SetMetrics attaches Prometheus metrics to the server.
// Must be called before the first request is served.
//
// Side effect (TASK-961): when both metrics AND the OAuth server are
// wired, this also attaches the OAuth-active-tokens callback collector
// and the revocation TTL observer. Order-independent — both
// SetMetrics and SetOAuthServer call wireOAuthMetricsObserver, which
// no-ops until both prerequisites are present.
func (s *Server) SetMetrics(m *metrics.Metrics) {
	s.metrics = m
	s.wireOAuthMetricsObserver()
	s.wireStreamGauge()
}

// wireOAuthMetricsObserver attaches the OAuth metrics that need both
// the metrics registry AND the OAuth server: the active-tokens
// callback collector (reads via the store) and the per-revocation
// TTL observer (fires from internal/oauth/storage.go on every
// access-token family revocation).
//
// Idempotent — re-registering the same collector would panic via
// prometheus.MustRegister, so we guard with a flag. Setting the
// observer multiple times is harmless (just replaces the function
// pointer).
//
// Why this lives on Server rather than in cmd/pad: it composes two
// optional Server fields whose set-order isn't guaranteed by the
// boot sequence, and centralizing the wiring here keeps the cmd/pad
// startup path declarative ("set X, set Y") without an explicit
// "now wire the cross-cut" call.
func (s *Server) wireOAuthMetricsObserver() {
	if s.metrics == nil || s.oauthServer == nil {
		return
	}
	if !s.oauthMetricsWired {
		s.metrics.RegisterOAuthActiveTokensCollector(s.store.CountActiveOAuthAccessTokens)
		s.oauthMetricsWired = true
	}
	s.oauthServer.Storage().SetRevocationObserver(func(kind string, ttl time.Duration) {
		s.metrics.OAuthTokenRevocationsTotal.WithLabelValues(kind).Inc()
		s.metrics.OAuthTokenTTLSeconds.Observe(ttl.Seconds())
	})
}

// SetMetricsToken configures the static bearer token required to scrape
// /metrics. When empty (the default), /metrics is exposed only to loopback
// callers so a self-hosted Prometheus on the same host keeps working
// without config — but LAN/internet scrapes are refused. A non-empty
// token requires "Authorization: Bearer <token>" regardless of source.
func (s *Server) SetMetricsToken(token string) {
	s.metricsToken = strings.TrimSpace(token)
}

// metricsAuth gates the /metrics endpoint. See SetMetricsToken for the
// policy. Uses constant-time comparison to avoid leaking the configured
// token via response timing.
func (s *Server) metricsAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.metricsToken == "" {
			// No token configured → loopback-only access.
			if !requestIsLoopback(r) {
				writeError(w, http.StatusForbidden, "forbidden",
					"/metrics is restricted to loopback when PAD_METRICS_TOKEN is unset")
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		const prefix = "Bearer "
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, prefix) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			writeError(w, http.StatusUnauthorized, "unauthorized",
				"Missing Bearer token for /metrics")
			return
		}
		given := strings.TrimSpace(strings.TrimPrefix(authHeader, prefix))
		if subtle.ConstantTimeCompare([]byte(given), []byte(s.metricsToken)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			writeError(w, http.StatusUnauthorized, "unauthorized",
				"Invalid Bearer token for /metrics")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SetSSELimits configures the streaming connection limits. A value of 0
// means unlimited for any of them.
//
// `global` and `perUser` bound BOTH stream endpoints together —
// /api/v1/events and /api/v1/events/stream — through one admission gate,
// because a held connection costs the same process resources whichever
// one opened it (BUG-2726). `perWorkspace` bounds only /api/v1/events,
// which is the only workspace-scoped one.
func (s *Server) SetSSELimits(global, perWorkspace, perUser int) {
	s.sseMaxConnections = global
	s.sseMaxPerWorkspace = perWorkspace
	s.sseMaxPerUser = perUser
	// Updates the EXISTING gate rather than replacing it — see setLimits
	// for why replacing strands held slots and drifts the gauge.
	// admission() builds one on first use with the values just stored.
	s.admission().setLimits(global, perUser)
	s.wireStreamGauge()
}

// wireStreamGauge registers pad_stream_connections_active as a
// scrape-time collector over the admission gate's total. Called from both
// SetSSELimits and SetMetrics because either can land first.
//
// Registered ONCE PER METRICS INSTANCE. MustRegister panics on a
// duplicate and both callers can fire, so it cannot register every time;
// a once-per-SERVER guard would be wrong in the other direction, since
// SetMetrics can install a different registry and would leave it
// silently missing the series.
//
// The closure reads s.admission() at scrape time rather than capturing
// the gate, so it stays correct if the gate is ever rebuilt.
func (s *Server) wireStreamGauge() {
	if s.metrics == nil || s.streamGaugeFor == s.metrics {
		return
	}
	s.streamGaugeFor = s.metrics
	s.metrics.RegisterStreamConnectionsCollector(func() int {
		return s.admission().heldTotal()
	})
}

// admission returns the shared stream admission gate, constructing an
// unbounded one on first use so a Server built without SetSSELimits (every
// test that does not care about limits) still has a working gate rather
// than a nil check at each call site.
//
// SetSSELimits reconfigures this gate in place rather than replacing it,
// so a late call neither strands held slots nor over-grants capacity.
//
// That is damage limitation, NOT a live-reconfiguration feature (codex
// round 9). SetSSELimits also writes plain Server fields that request
// handlers read — sseMaxPerWorkspace among them — so calling it while
// requests are in flight is a data race regardless of how careful this
// gate is. It is config-time: call it before ListenAndServe. The in-place
// update exists so that a test, or an embedder that does call it late,
// does not silently blow past its own limit.
func (s *Server) admission() *streamAdmission {
	s.admitOnce.Do(func() {
		if s.streamAdmit == nil {
			s.streamAdmit = newStreamAdmission(s.sseMaxConnections, s.sseMaxPerUser)
			// The lazily-built gate needs the gauge too, or a server
			// wired with metrics but never given explicit limits would
			// export a permanently-zero pad_stream_connections_active
			// while serving streams — the same shape of lie as an
			// unregistered metric reporting 0.
			s.wireStreamGauge()
		}
	})
	return s.streamAdmit
}

// SetTrustedProxies configures which direct TCP peers are allowed to set
// X-Real-IP / X-Forwarded-For on incoming requests. Accepts a comma-
// separated list of CIDRs or bare IPs (e.g. "10.0.0.0/8, 172.16.0.0/12").
// When empty (the default), proxy headers are ignored entirely — the
// actual TCP peer address is used for rate limiting, the bootstrap
// loopback check, and audit logging.
func (s *Server) SetTrustedProxies(spec string) {
	s.trustedProxyCIDRs = ParseTrustedProxyCIDRs(spec)
}

// SetIPChangeEnforce controls how the auth middleware reacts when a
// session's binding (client IP OR User-Agent hash) changes mid-lifetime:
//   - mode == "strict": revoke the session and reject the request (the token
//     is treated as possibly stolen). Covers BOTH the IP and the UA signal —
//     one flag arms the whole session-binding enforcement.
//   - anything else (default): log to the audit log, update the stored IP,
//     and let the request through. Strict mode breaks legitimate mobility
//     (mobile roaming, VPN toggles for IP; browser/WebView updates for UA) so
//     it is opt-in for high-sensitivity deployments via the
//     PAD_IP_CHANGE_ENFORCE env var. See handleSessionIPChange /
//     handleSessionUAChange for the per-signal semantics.
func (s *Server) SetIPChangeEnforce(mode string) {
	s.ipChangeEnforceStrict = strings.EqualFold(strings.TrimSpace(mode), "strict")
}

// reconfigureEmail reads email settings from the platform_settings table
// and updates (or creates) the email sender. Called after admin settings change.
func (s *Server) reconfigureEmail() {
	apiKey, _ := s.store.GetPlatformSetting(settingMailerooAPIKey)
	fromAddr, _ := s.store.GetPlatformSetting(settingEmailFrom)
	fromName, _ := s.store.GetPlatformSetting(settingEmailFromName)

	if apiKey == "" {
		// No platform-settings key. If email was wired from env vars, leave that
		// sender in place — env is the deployment baseline. Otherwise the admin
		// cleared the only email config (e.g. selecting provider "None"), so tear
		// down the live sender: clearing the DB key alone left the running process
		// sending mail until restart (BUG-1890).
		if !s.emailEnvConfigured {
			s.email = nil
			s.emailAPIKey = ""
		}
		return
	}

	s.emailAPIKey = apiKey
	if s.email == nil {
		// Create a new sender from platform settings
		s.email = email.NewSender(apiKey, fromAddr, fromName, s.baseURL)
	} else {
		// Update existing sender
		s.email.Configure(apiKey, fromAddr, fromName, s.baseURL)
	}
	// Propagate cloud mode whichever way email was wired — see SetEmailSender
	// for the matching note. Configure() preserves cloudMode on existing
	// senders since SetCloudMode is independent; this branch covers the
	// fresh-NewSender path.
	if s.cloudMode {
		s.email.SetCloudMode(true)
	}
}

// InitEmailFromSettings loads email config from platform settings on startup,
// merging with any env-var-based sender that was already attached.
func (s *Server) InitEmailFromSettings() {
	s.reconfigureEmail()
}

func (s *Server) setupRouter() {
	r := chi.NewRouter()

	// Infrastructure middleware (applies to all routes including /metrics)
	// CapturePeerAddr MUST run before TrustedProxyRealIP so downstream code
	// that needs to verify the real TCP peer (e.g. the bootstrap loopback
	// check) can read the untampered value from request context even on
	// deployments with a trusted reverse proxy in front.
	r.Use(CapturePeerAddr)
	// RealIP is gated on PAD_TRUSTED_PROXIES. When unset (the default), proxy
	// headers are ignored and the real TCP peer address is used everywhere.
	// This prevents X-Forwarded-For spoofing from bypassing rate limits, the
	// bootstrap loopback check, or audit logs on direct-exposed deployments.
	r.Use(TrustedProxyRealIP(s.trustedProxyCIDRs))
	r.Use(chimiddleware.RequestID)
	r.Use(StructuredLogger)
	if s.metrics != nil {
		r.Use(MetricsMiddleware(s.metrics))
	}
	r.Use(chimiddleware.Recoverer)

	// Security headers (applies to all routes)
	r.Use(SecurityHeaders)
	if s.secureCookies {
		r.Use(StrictTransportSecurity)
	}

	// One CORS handler, shared. The /api/v1 group mounts it as ordinary
	// middleware; ValidatePath serves its REJECTION through the same
	// instance, because that rejection short-circuits above the group and
	// would otherwise answer a cross-origin caller without the CORS
	// headers its siblings carry — see the middleware's doc comment.
	corsMW := cors.Handler(cors.Options{
		AllowedOrigins: parseCORSOrigins(s.corsOrigins),
		AllowedMethods: []string{"GET", "POST", "PATCH", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token", "X-Share-Password", "X-Bootstrap-Token"},
		// Credentials flag is gated on an operator explicitly listing
		// PAD_CORS_ORIGINS. The CLI uses Bearer tokens so the default
		// "no CORS_ORIGINS set" path doesn't need credential sharing;
		// leaving it off by default prevents cross-origin fetches from
		// a browser on a different site from piggy-backing cookies
		// on the victim's session.
		AllowCredentials: corsAllowCredentials(s.corsOrigins),
		MaxAge:           300,
	})

	// Reject a request whose decoded path is not valid UTF-8 (or contains a
	// NUL) before anything routes it, so no handler can hand such a segment
	// to the store (BUG-2782). Placed at the END of the infrastructure block
	// deliberately: after StructuredLogger and MetricsMiddleware so a
	// rejection is still logged with a request id and still counted (as
	// route "unmatched", status 400) rather than being invisible to an
	// operator watching for a flood, and after SecurityHeaders so the 400
	// carries them like every other response.
	r.Use(ValidatePath(corsMW))

	// The query-string half of the same rule (BUG-2784). Separate middleware
	// rather than one combined check so the two rejections carry distinct
	// error codes; ordered after ValidatePath so a request that is bad in
	// both places is answered for its path, which is the more specific fault.
	r.Use(ValidateQuery(corsMW))

	// MCP Streamable HTTP transport + OAuth discovery endpoints
	// (PLAN-943 TASK-950). Mounted outside the standard /api/v1
	// auth-required group because:
	//
	//   - /mcp uses Bearer auth via its own MCPBearerAuth middleware,
	//     producing the spec-shape 401 + WWW-Authenticate that MCP
	//     clients expect (the API-stack 401 envelope is JSON-only and
	//     would fail Claude Desktop's discovery handshake).
	//   - /.well-known/oauth-protected-resource and
	//     /.well-known/oauth-authorization-server are public discovery
	//     documents (RFC 9728 / RFC 8414); routing them through
	//     TokenAuth+SessionAuth+RequireAuth would 401 unauth probes.
	//
	// No-op when SetMCPTransport hasn't been called or cloud mode is
	// off — see registerMCPRoutes for the gating.
	s.registerMCPRoutes(r)

	// OAuth 2.1 authorization-server flow endpoints (PLAN-943
	// TASK-1025 sub-PR C). /oauth/{register,authorize,token,
	// authorize/decide} mounted alongside /mcp + /.well-known/*,
	// outside /api/v1's auth-required group. CSRF middleware runs
	// only on /api/* paths so /oauth/* is naturally exempt; the
	// consent-decision endpoint adds its own form-token check
	// using the existing __Host-pad_csrf cookie.
	//
	// SessionAuth runs in this group so /oauth/authorize can detect
	// whether the user is logged in via the __Host-pad_session
	// cookie. SessionAuth falls through gracefully when no cookie
	// is present (handlers see currentUser(r)==nil and redirect to
	// /login). RequireAuth is intentionally NOT used — /oauth/authorize
	// must be reachable anonymously to trigger the login redirect.
	//
	// RateLimit gates /oauth/register specifically (per Codex review
	// #372 round 2 — the DCR endpoint is open by RFC 7591 design,
	// but unlimited writes to oauth_clients are an obvious DoS
	// surface). The middleware short-circuits other /oauth/* paths
	// because they're either session-bound or PKCE-bound; explicit
	// per-endpoint limits arrive with TASK-959.
	//
	// No-op when SetOAuthServer hasn't been called or cloud mode is off.
	r.Group(func(r chi.Router) {
		r.Use(s.requireCloudMode)
		r.Use(s.SessionAuth)
		r.Use(s.RateLimit)
		s.registerOAuthRoutes(r)
	})

	// Prometheus scrape endpoint — exempt from the standard auth/CSRF stack
	// (Prometheus can't present a session cookie or pass a CSRF header), but
	// gated by a dedicated static bearer token. Without the gate, any
	// unauthenticated caller on the network can read workspace counts, API
	// usage patterns, and — via label enumeration — user/workspace IDs.
	//
	// The gate runs in three layers:
	//   1. No PAD_METRICS_TOKEN → endpoint is open ONLY to loopback. Safe
	//      default for self-hosters running Prometheus on the same box.
	//   2. PAD_METRICS_TOKEN set → "Authorization: Bearer <token>" required.
	//      Compared in constant time; empty/missing header → 401.
	//   3. In either case the SecurityHeaders / rate-limit / logging chain
	//      already wraps this group from the outer r.Use() calls above.
	if s.metrics != nil {
		r.Group(func(r chi.Router) {
			r.Use(s.metricsAuth)
			r.Handle("/metrics", promhttp.HandlerFor(s.metrics.Registry, promhttp.HandlerOpts{}))
		})
	}

	// All other routes — full middleware stack
	r.Group(func(r chi.Router) {
		r.Use(corsMW)
		r.Use(s.TokenAuth)
		r.Use(s.SessionAuth)
		r.Use(s.RateLimit)
		r.Use(s.CSRFProtect)
		r.Use(s.RequireAuth)
		// PLAN-1933 DR-4: block content-mutating requests from an
		// authenticated cloud user whose email is unverified. Mounted
		// AFTER RequireAuth so currentUser is already resolved; a no-op
		// on self-host and for verified / unauthenticated callers. The
		// method gate here covers the /api/v1 surface (session + PAT);
		// the collab GET-upgrade, the OAuth-provider flow, and the MCP
		// write path are gated at their own out-of-band mounts.
		r.Use(s.RequireVerifiedEmail)
		r.Use(jsonContentType)

		// SSE endpoint (outside jsonContentType middleware — but inherits auth)
		r.Get("/api/v1/events", s.handleSSE)

		// User-scoped watch/nudge event stream (TASK-2533, DOC-2479).
		// Unlike /api/v1/events above, this is NOT workspace-scoped — a
		// caller's watches and addressed pushes can span
		// every workspace they belong to. Lives alongside the other SSE
		// endpoint for the same "outside jsonContentType, inherits auth"
		// reason.
		r.Get("/api/v1/events/stream", s.handleWatchEventsStream)

		// Live-session presence (PLAN-2558 S1) — the READ side of the
		// registry the stream above writes. Mounted here beside the
		// endpoint it reports on rather than in the /api/v1 Route block
		// below: the two are one feature, and a reader asking "what
		// fills this list?" should find the answer on the adjacent
		// line. Self-scoped; see handleListSessions on why there is
		// deliberately no admin view.
		r.Get("/api/v1/sessions", s.handleListSessions)

		// WebSocket endpoint for Yjs-based collaborative editing on a
		// single item (PLAN-1248). Lives outside jsonContentType for
		// the same reason as SSE: the response is a WS upgrade, not
		// JSON. Inherits the auth middleware chain — handleCollab
		// then re-checks workspace access keyed on the item's
		// workspace ID (the URL only carries itemID).
		r.Get("/api/v1/collab/{itemID}", s.handleCollab)

		// API routes
		r.Route("/api/v1", func(r chi.Router) {
			r.Get("/health", s.handleHealth)
			r.Get("/health/live", s.handleHealthLive)
			r.Get("/health/ready", s.handleHealthReady)
			r.Get("/plan-limits", s.handleGetPlanLimits) // Public: billing page reads plan limits
			r.Get("/unsubscribe", s.handleUnsubscribe)   // Public: email opt-out (HMAC-signed)

			// Server capabilities — public so the editor can fetch it
			// pre-login and gate per-format rotate / crop UI on the
			// processor's reach (TASK-878). The response is static for
			// the lifetime of the binary; clients can cache freely.
			r.Get("/server/capabilities", s.handleServerCapabilities)

			// Auth endpoints (exempt from auth middleware)
			r.Route("/auth", func(r chi.Router) {
				r.Get("/session", s.handleSessionCheck)
				r.Post("/bootstrap", s.handleBootstrap)
				r.Post("/register", s.handleRegister)
				r.Get("/check-username", s.handleCheckUsername)
				r.Post("/login", s.handleLogin)
				r.Post("/logout", s.handleLogout)
				r.Get("/me", s.handleGetCurrentUser)
				r.Patch("/me", s.handleUpdateCurrentUser)

				// Password reset
				r.Post("/forgot-password", s.handleForgotPassword)
				r.Post("/reset-password", s.handleResetPassword)
				// Localhost-only recovery escape hatch (self-host, non-cloud).
				r.Post("/local-reset", s.handleLocalReset)

				// Email verification (PLAN-1933 Wave 3b). Both are
				// enumeration-safe and rate-limited (middleware_ratelimit.go
				// reuses the PasswordReset bucket) and are already in
				// RequireVerifiedEmail's exempt list so an unverified user
				// can reach them to clear their own unverified state.
				r.Post("/verify-email", s.handleVerifyEmail)
				r.Post("/resend-verification", s.handleResendVerification)

				// Two-factor authentication
				r.Post("/2fa/setup", s.handleTOTPSetup)
				r.Post("/2fa/verify", s.handleTOTPVerify)
				r.Post("/2fa/disable", s.handleTOTPDisable)
				r.Post("/2fa/login-verify", s.handleTOTPLoginVerify)

				// Account management (GDPR)
				r.Post("/delete-account", s.handleDeleteAccount)
				r.Get("/export", s.handleExportAccount)

				// User-scoped API tokens
				r.Get("/tokens", s.handleListUserTokens)
				r.Post("/tokens", s.handleCreateUserToken)
				r.Delete("/tokens/{tokenID}", s.handleDeleteUserToken)
				r.Post("/tokens/{tokenID}/rotate", s.handleRotateUserToken)

				// Cloud: OAuth login/linking (called by pad-cloud sidecar, protected by cloud secret)
				r.Post("/oauth-login", s.handleOAuthLogin)
				r.Post("/oauth-link", s.handleOAuthLink)
				r.Post("/oauth-unlink", s.handleOAuthUnlink)

				// CLI browser-based auth flow
				r.Post("/cli/sessions", s.handleCreateCLIAuthSession)
				r.Get("/cli/sessions/{code}", s.handlePollCLIAuthSession)
				r.Post("/cli/sessions/{code}/approve", s.handleApproveCLIAuthSession)
			})

			// Admin endpoints (admin-only, handlers check role internally)
			r.Route("/admin", func(r chi.Router) {
				r.Get("/settings", s.handleGetPlatformSettings)
				r.Patch("/settings", s.handleUpdatePlatformSettings)
				r.Post("/test-email", s.handleTestEmail)

				// Cloud sidecar endpoints — only exist in cloud mode. requireCloudMode
				// returns 404 outside cloud mode so a self-hosted deployment doesn't
				// expose "Cloud mode not configured" to unauthenticated probes.
				r.Group(func(r chi.Router) {
					r.Use(s.requireCloudMode)
					r.Post("/plan", s.handleSetPlan)                                // Cloud: sidecar sets user plans; also accessible to admins
					r.Post("/stripe-customer-id", s.handleSetStripeCustomerID)      // Cloud: sidecar stores Stripe customer ID after checkout
					r.Get("/user-by-customer", s.handleGetUserByCustomerID)         // Cloud: sidecar looks up user by Stripe customer ID
					r.Post("/stripe-event-processed", s.handleStripeEventProcessed) // Cloud: sidecar webhook idempotency (TASK-696)
					r.Post("/stripe-event-unmark", s.handleStripeEventUnmark)       // Cloud: sidecar handler-failure rollback (TASK-736)
					r.Post("/payment-failed", s.handlePaymentFailed)                // Cloud: sidecar forwards invoice.payment_failed to trigger email (TASK-712)

					// Admin Billing dashboard data (TASK-827 / PLAN-825). Proxies
					// pad-cloud's /admin/metrics/billing for Stripe-derived stats
					// (active subs, MRR, ARR, churn) and merges with local
					// users-table aggregates (customers_by_plan, new_signups_30d).
					// Always returns 200; degraded states (sidecar unreachable,
					// Stripe not configured) are surfaced as flags in the body.
					r.Get("/billing-stats", s.handleAdminBillingStats)
				})

				// User management
				r.Get("/users", s.handleAdminListUsers)
				r.Get("/users/{userID}", s.handleAdminGetUser)
				r.Patch("/users/{userID}", s.handleAdminUpdateUser)
				r.Post("/users/{userID}/reset-password", s.handleAdminResetPassword)
				r.Get("/users/{userID}/workspaces", s.handleAdminGetUserWorkspaces)
				r.Get("/users/{userID}/detail", s.handleAdminGetUserDetail)
				r.Get("/users/{userID}/activity", s.handleAdminGetUserActivity)
				r.Get("/users/{userID}/metrics", s.handleAdminGetUserMetrics)
				r.Post("/users/{userID}/disable", s.handleAdminDisableUser)
				r.Post("/users/{userID}/enable", s.handleAdminEnableUser)
				r.Post("/users/{userID}/verify-email", s.handleAdminVerifyEmail)

				// Invitations
				r.Get("/invitations", s.handleAdminListInvitations)
				r.Post("/invitations/{invID}/resend", s.handleAdminResendInvitation)
				r.Delete("/invitations/{invID}", s.handleAdminDeleteInvitation)

				// Plan limits
				r.Get("/limits", s.handleAdminGetLimits)
				r.Patch("/limits", s.handleAdminUpdateLimits)

				// Platform stats
				r.Get("/stats", s.handleAdminStats)

				// MCP audit log — admin-only full-table view (TASK-960).
				// Powers /console/admin/mcp-audit. Per-connection
				// drilldown that users see for their own connections
				// lives at /api/v1/connected-apps/{id}/audit (registered
				// outside the admin group so non-admin users can read
				// their own).
				r.Get("/mcp-audit", s.handleAdminMCPAudit)
			})

			// Audit log (admin-only)
			r.Get("/audit-log", s.handleAuditLog)

			// MCP per-connection audit (TASK-960). Owner-only via the
			// store query (user_id is one of the WHERE clauses);
			// returns the requesting user's own MCP activity for one
			// connection. The handler runs inside the standard
			// /api/v1 auth-required group, so unauthenticated callers
			// 401 here just like every other API endpoint.
			r.Get("/connected-apps/{id}/audit", s.handleMCPConnectionAudit)

			// Connected-apps management (TASK-954). Lists every
			// active OAuth grant chain the user has authorized
			// (Claude Desktop, Cursor, …) and lets them revoke one.
			// Cloud-mode-gated because OAuth is a cloud-only
			// surface — self-hosted deployments would always see
			// an empty list.
			r.Group(func(r chi.Router) {
				r.Use(s.requireCloudMode)
				r.Get("/connected-apps", s.handleListConnectedApps)
				r.Delete("/connected-apps/{id}", s.handleRevokeConnectedApp)
				// PLAN-1519 / TASK-1524 / IDEA-1517 §3: mutation
				// endpoints for the connections-page UI. Per-field
				// patches rather than a general PATCH for cleaner
				// error envelopes + audit shape.
				r.Patch("/connected-apps/{id}/name", s.handleRenameConnectedApp)
				r.Patch("/connected-apps/{id}/flags", s.handleUpdateConnectedAppFlags)
				r.Post("/connected-apps/{id}/workspaces", s.handleAddConnectedAppWorkspace)
				r.Delete("/connected-apps/{id}/workspaces/{slug}", s.handleRemoveConnectedAppWorkspace)
			})

			// Templates
			r.Get("/templates", s.handleListTemplates)

			// Convention Library
			r.Get("/convention-library", s.handleConventionLibrary)

			// Playbook Library
			r.Get("/playbook-library", s.handlePlaybookLibrary)

			// Single library entry by title (conventions first, then playbooks).
			// TASK-1561 / PLAN-1560.
			r.Get("/library/entry", s.handleLibraryEntry)

			// URL import — fetch a remote page and return markdown.
			// Side-effect-free; the client decides what to do with the
			// markdown. See PLAN-1467 / TASK-1472 / internal/urlimport.
			r.Post("/import/url", s.handleImportURL)

			// Invitations (outside workspace scope)
			r.Post("/invitations/{code}/accept", s.handleAcceptInvitation)

			// Non-consuming invitation preview (BUG-1934). Public/pre-auth
			// (exempted in isPublicAPIPath) so the logged-out /join page can
			// prefill the invited email read-only and pick register-vs-login
			// mode. Always HTTP 200 + rate limited (see middleware_ratelimit.go)
			// so it can't be used to enumerate invite codes.
			r.Get("/invitations/{code}/preview", s.handlePreviewInvitation)

			// OAuth client public-info (PLAN-943 TASK-1027 sub-PR E).
			// Read-only consent-screen support for OAuth clients
			// registered via /oauth/register. Auth-required (inherits
			// RequireAuth from the parent group); cloud-mode-gated so
			// self-hosted deployments without an OAuth server don't
			// expose a hollow endpoint. Returns four non-sensitive
			// fields (client_id, client_name, logo_uri, redirect_uris)
			// — see handlers_oauth_clients.go for the full leak-surface
			// rationale.
			r.Group(func(r chi.Router) {
				r.Use(s.requireCloudMode)
				r.Get("/oauth/clients/{id}/public-info", s.handleOAuthClientPublicInfo)
			})

			// Share link resolution (outside workspace scope, no auth required)
			r.Get("/s/{token}", s.handleResolveShareLink)
			// Share-link asset bytes (BUG-2389 2b / TASK-2637): rendered image
			// VARIANTS for attachments embedded in the shared content. Same
			// public/no-auth group; protected links gate on a short-lived
			// signed ref minted by handleResolveShareLink. Originals and
			// file downloads are out of scope by authorization.
			r.Get("/s/{token}/attachments/{attachmentID}", s.handleGetShareLinkAttachment)

			// Claim-code redemption (PLAN-1519 / TASK-1521 / IDEA-1517 §4).
			// POST /api/v1/oauth/claim with body {workspace, code} grants
			// the calling OAuth connection access to one workspace via a
			// stateless 6-digit HMAC code the user generated in the web
			// UI's "Connect project" modal. Auth: standard /api/v1 chain
			// (TokenAuth + RequireAuth); the handler itself short-circuits
			// the side effect when the caller isn't an OAuth grant (PAT /
			// CLI session) and 412s when the claim secret isn't wired.
			r.Post("/oauth/claim", s.handleOAuthClaim)

			// Workspaces
			r.Route("/workspaces", func(r chi.Router) {
				r.Get("/", s.handleListWorkspaces)
				r.Post("/", s.handleCreateWorkspace)
				r.Post("/import", s.handleImportWorkspace)
				r.Put("/reorder", s.handleReorderWorkspaces)

				// Soft-delete recovery (PLAN-1969 / TASK-1970). Both live
				// OUTSIDE the /{slug} RequireWorkspaceAccess subrouter
				// because that middleware resolves only LIVE workspaces
				// (deleted_at IS NULL) and would 404 a soft-deleted one
				// before the handler ran. The static "/deleted" segment is
				// registered before the /{slug} param route so chi matches
				// it exactly (static beats param); it lists the caller's own
				// deleted-but-restorable workspaces. "/{slug}/restore"
				// resolves the soft-deleted row itself and enforces
				// owner-only authz inside the handler.
				r.Get("/deleted", s.handleListDeletedWorkspaces)
				r.Post("/{slug}/restore", s.handleRestoreWorkspace)

				r.Route("/{slug}", func(r chi.Router) {
					r.Use(s.RequireWorkspaceAccess)

					r.Get("/", s.handleGetWorkspace)
					r.Patch("/", s.handleUpdateWorkspace)
					r.Delete("/", s.handleDeleteWorkspace)
					r.Get("/export", s.handleExportWorkspace)
					// Import a single playbook/convention artifact (Markdown
					// + YAML frontmatter) into this workspace. Editor+ gate
					// is enforced inside the handler against the destination
					// collection.
					r.Post("/import-artifact", s.handleImportArtifact)

					// Activity (workspace level)
					r.Get("/activity", s.handleListWorkspaceActivity)

					// Claim-code generation + smart suppression (PLAN-1519
					// / TASK-1525 / IDEA-1517 §4). Inherits
					// RequireWorkspaceAccess so any member can pull a code
					// for any workspace they belong to — membership IS
					// the consent. See handlers_claim_code.go.
					r.Get("/claim-code", s.handleWorkspaceClaimCode)

					// Documents (v1 — will be replaced by items in Phase 2)
					r.Route("/documents", func(r chi.Router) {
						r.Get("/", s.handleListDocuments)
						r.Post("/", s.handleCreateDocument)

						r.Route("/{docID}", func(r chi.Router) {
							r.Get("/", s.handleGetDocument)
							r.Patch("/", s.handleUpdateDocument)
							r.Delete("/", s.handleDeleteDocument)
							r.Post("/restore", s.handleRestoreDocument)

							// Versions
							r.Get("/versions", s.handleListVersions)
							r.Get("/versions/{versionID}", s.handleGetVersion)

							// Activity (document level)
							r.Get("/activity", s.handleListDocumentActivity)
						})
					})

					// Collections (v2)
					r.Route("/collections", func(r chi.Router) {
						r.Get("/", s.handleListCollections)
						r.Post("/", s.handleCreateCollection)
						r.Route("/{collSlug}", func(r chi.Router) {
							r.Get("/", s.handleGetCollection)
							r.Patch("/", s.handleUpdateCollection)
							r.Delete("/", s.handleDeleteCollection)
							// Items within collection
							r.Get("/items", s.handleListCollectionItems)
							r.Post("/items", s.handleCreateItem)
							// Pairs with /items-index — server-side checkbox
							// progress so the collection page can render
							// list/board/table progress badges without
							// fetching item content (TASK-1349).
							r.Get("/checkbox-progress", s.handleCollectionCheckboxProgress)
							// Child-item completion progress for any collection
							// (BUG-1509). Same visibility/guest-grant semantics
							// as /plans-progress but collection-generic.
							r.Get("/child-progress", s.handleCollectionChildrenProgress)
							// Collection grants
							r.Get("/grants", s.handleListCollectionGrants)
							r.Post("/grants", s.handleCreateCollectionGrant)
							r.Delete("/grants/{grantID}", s.handleDeleteCollectionGrant)
							r.Get("/share-links", s.handleListCollectionShareLinks)
							r.Post("/share-links", s.handleCreateCollectionShareLink)
							// Saved views within collection
							r.Get("/views", s.handleListViews)
							r.Post("/views", s.handleCreateView)
							r.Route("/views/{viewID}", func(r chi.Router) {
								r.Patch("/", s.handleUpdateView)
								r.Delete("/", s.handleDeleteView)
							})
						})
					})

					// Plans progress
					r.Get("/plans-progress", s.handlePlansProgress)

					// Skinny-projection cross-collection items list for the
					// local-first read model bootstrap (PLAN-1343 / TASK-1344).
					// Lives at workspace level — sibling to /plans-progress
					// and /starred — so the path can't ever collide with an
					// item slug under /items/{itemSlug}.
					r.Get("/items-index", s.handleListItemsIndex)

					// Delta-fetch sibling of /items-index: returns rows
					// where seq > since, including tombstones, so a
					// local-first read-model client can resume without
					// re-downloading the whole index (PLAN-1343 / TASK-1354).
					r.Get("/items-changes", s.handleListItemsChanges)

					// User grants (all grants for a specific user in this workspace)
					r.Get("/users/{userID}/grants", s.handleListUserGrants)

					// Starred items
					r.Get("/starred", s.handleListStarredItems)

					// Distinct tags across the workspace (with item counts)
					r.Get("/tags", s.handleListTags)

					// Items (cross-collection, v2)
					r.Get("/items", s.handleListItems)
					// Bulk mutation (TASK-1668). Static segment must be
					// registered before the /items/{itemSlug} param route
					// so "bulk" isn't captured as an item slug.
					r.Post("/items/bulk", s.handleBulkItems)
					r.Route("/items/{itemSlug}", func(r chi.Router) {
						r.Get("/", s.handleGetItem)
						r.Patch("/", s.handleUpdateItem)
						r.Delete("/", s.handleDeleteItem)
						r.Post("/restore", s.handleRestoreItem)
						r.Post("/move", s.handleMoveItem)
						// Cross-workspace copy PREFLIGHT (PLAN-2357 /
						// TASK-2364). Reports what a copy into another
						// workspace would carry, drop and need, and
						// leaves no trace a copy would have left — see
						// handlers_items_copy_preflight.go for the exact
						// scope of that guarantee. POST because the
						// request carries a body (destination + override
						// map), not because it mutates. The mutating
						// sibling lands at /copy in TASK-2365.
						r.Post("/copy/preflight", s.handleCopyItemPreflight)
						// Cross-workspace copy, the MUTATION (PLAN-2357 /
						// TASK-2365). Same request shape as the preflight
						// above; with archive_source it is the move. Post-
						// commit fanout is asymmetric — see
						// handlers_items_copy.go. Registered after the more
						// specific /copy/preflight, though chi's trie makes
						// the order immaterial.
						r.Post("/copy", s.handleCopyItem)
						// Export a single playbook/convention item as a
						// portable artifact (Markdown + YAML frontmatter).
						// Gated by per-item visibility, not the workspace-
						// export owner gate — a viewer who can see the item
						// may export it.
						r.Get("/export", s.handleExportItemArtifact)
						r.Get("/versions", s.handleListItemVersions)
						r.Get("/versions/{versionID}", s.handleGetItemVersion)
						r.Post("/versions/{versionID}/restore", s.handleRestoreItemVersion)
						r.Get("/activity", s.handleListItemActivity)
						r.Get("/links", s.handleGetItemLinks)
						r.Post("/links", s.handleCreateItemLink)
						r.Get("/comments", s.handleListComments)
						r.Post("/comments", s.handleCreateComment)
						r.Get("/timeline", s.handleListItemTimeline)
						r.Get("/children", s.handleGetItemChildren)
						r.Get("/progress", s.handleGetItemProgress)
						r.Get("/backlinks", s.handleGetItemBacklinks)
						r.Get("/tasks", s.handleGetItemChildren) // deprecated alias
						r.Get("/grants", s.handleListItemGrants)
						r.Post("/grants", s.handleCreateItemGrant)
						r.Delete("/grants/{grantID}", s.handleDeleteItemGrant)
						r.Get("/share-links", s.handleListItemShareLinks)
						r.Post("/share-links", s.handleCreateItemShareLink)
						// Stars
						r.Get("/star", s.handleGetItemStarStatus)
						r.Post("/star", s.handleStarItem)
						r.Delete("/star", s.handleUnstarItem)
						// Execution lease (#1221): atomic claim/checkout so
						// concurrent pollers can't both "start" an item.
						r.Post("/claim", s.handleClaimItem)
						r.Post("/release", s.handleReleaseItem)
						// Watches (TASK-2533): durable per-item subscriptions
						// for the padd event-stream / plugin-monitor nudge
						// pipeline. `pad watch <ref>` / `pad watch remove <ref>`.
						r.Post("/watch", s.handleCreateWatch)
						r.Delete("/watch", s.handleDeleteWatch)
						// Push (IDEA-2544 Phase 1): transient, self-addressed
						// human→harness dispatch over the SAME watch-events
						// bus/stream — no durable row, see handlePushToItem's
						// doc comment. `pad push <ref> -m "message"`.
						r.Post("/push", s.handlePushToItem)
					})

					// Links (v2)
					r.Delete("/links/{linkID}", s.handleDeleteItemLink)

					// Share links (workspace-scoped management)
					r.Delete("/share-links/{linkID}", s.handleDeleteShareLink)
					r.Get("/share-links/{linkID}/views", s.handleShareLinkViews)

					// Comments (v2)
					r.Route("/comments/{commentID}", func(r chi.Router) {
						r.Patch("/", s.handleUpdateComment)
						r.Delete("/", s.handleDeleteComment)
						r.Post("/replies", s.handleCreateReply)
						r.Post("/reactions", s.handleAddReaction)
						r.Delete("/reactions/{emoji}", s.handleRemoveReaction)
					})

					// Role Board (cross-collection role-based view)
					r.Get("/roles/board", s.handleRoleBoard)
					r.Put("/roles/board/reorder", s.handleRoleBoardReorder)
					r.Put("/roles/board/lane-order", s.handleRoleBoardLaneReorder)

					// Agent Roles
					r.Route("/agent-roles", func(r chi.Router) {
						r.Get("/", s.handleListAgentRoles)
						r.Post("/", s.handleCreateAgentRole)
						r.Route("/{roleID}", func(r chi.Router) {
							r.Get("/", s.handleGetAgentRole)
							r.Patch("/", s.handleUpdateAgentRole)
							r.Delete("/", s.handleDeleteAgentRole)
						})
					})

					// Attachments
					//   POST   /attachments                          — upload (TASK-871)
					//   GET    /attachments/{attachmentID}           — serve blob (TASK-872, supports ?variant=)
					//   HEAD   /attachments/{attachmentID}           — metadata only (TASK-877 file-chip enrichment)
					//   POST   /attachments/{attachmentID}/transform — server-side rotate/crop (TASK-879/880)
					//
					// chi does not auto-route HEAD to the GET handler, so the
					// editor's HEAD probe for size + MIME has to be registered
					// explicitly. The handler short-circuits the streaming
					// path on HEAD; http.ServeContent already strips the body
					// on the seekable path.
					r.Post("/attachments", s.handleUploadAttachment)
					r.Get("/attachments", s.handleListWorkspaceAttachments)
					r.Get("/attachments/{attachmentID}", s.handleGetAttachment)
					r.Head("/attachments/{attachmentID}", s.handleGetAttachment)
					r.Post("/attachments/{attachmentID}/transform", s.handleTransformAttachment)
					r.Delete("/attachments/{attachmentID}", s.handleDeleteWorkspaceAttachment)

					// Storage usage summary for Settings → Storage and other
					// quota-aware UI surfaces (TASK-881). Cached behind a
					// short TTL — see handleGetWorkspaceStorageUsage.
					r.Get("/storage/usage", s.handleGetWorkspaceStorageUsage)

					// Webhooks
					r.Route("/webhooks", func(r chi.Router) {
						r.Get("/", s.handleListWebhooks)
						r.Post("/", s.handleCreateWebhook)
						r.Route("/{webhookID}", func(r chi.Router) {
							r.Delete("/", s.handleDeleteWebhook)
							r.Post("/test", s.handleTestWebhook)
						})
					})

					// API Tokens
					r.Route("/tokens", func(r chi.Router) {
						r.Get("/", s.handleListTokens)
						r.Post("/", s.handleCreateToken)
						r.Delete("/{tokenID}", s.handleDeleteToken)
					})

					// Members
					r.Route("/members", func(r chi.Router) {
						r.Get("/", s.handleListMembers)
						r.Post("/invite", s.handleInviteMember)
						r.Delete("/invitations/{invID}", s.handleCancelInvitation)
						r.Delete("/{userID}", s.handleRemoveMember)
						r.Patch("/{userID}", s.handleUpdateMemberRole)
						r.Get("/{userID}/collection-access", s.handleGetMemberCollectionAccess)
						r.Put("/{userID}/collection-access", s.handleSetMemberCollectionAccess)
					})

					// Me — current user's effective workspace context (role,
					// collection access, grants). Open to any principal admitted
					// by RequireWorkspaceAccess (members + guests).
					r.Get("/me", s.handleGetMe)

					// Dashboard (v2)
					r.Get("/dashboard", s.handleGetDashboard)

					// Workspace graph — {nodes, edges} for the 3D
					// graph view (PLAN-1730 / TASK-1731). Active
					// items by default; ?include_terminal=true for
					// the full history.
					r.Get("/graph", s.handleGetWorkspaceGraph)

					// Project report — windowed throughput/flow/status
					// stats (PLAN-1628 / TASK-1630).
					r.Get("/report", s.handleGetReport)
					// Per-user Insights layout prefs (PLAN-1628 / TASK-1634).
					r.Get("/report/layout", s.handleGetReportLayout)
					r.Put("/report/layout", s.handleSaveReportLayout)

					// Project intelligence reads — next/standup/changelog
					// (PLAN-1888 / TASK-1894). Mirror `pad project
					// next|standup|changelog` (cmd/pad/main.go) — KEEP IN
					// SYNC, see handlers_project_intel.go's doc comments.
					// The MCP HTTP transport's dispatchProjectNext/Standup/
					// Changelog (internal/mcp/dispatch_http_project.go)
					// proxy directly to these three handlers (TASK-1916),
					// so they need no separate sync-keeping.
					r.Get("/next", s.handleGetProjectNext)
					r.Get("/standup", s.handleGetProjectStandup)
					r.Get("/changelog", s.handleGetProjectChangelog)

					// Agent bootstrap (PLAN-1377 / TASK-1379) — single
					// round-trip that returns workspace + user +
					// collections + always-on conventions + roles +
					// playbook metadata + dashboard + recent activity.
					// Replaces the four /pad context-loading calls the
					// skill used to make. Same shape via the MCP
					// surfaces in TASK-1380.
					r.Get("/agent/bootstrap", s.handleGetBootstrap)

					// Playbook surface (PLAN-1377 / TASK-1382) — list /
					// show / run for first-class invokable procedures.
					// run is side-effect-free: it parses args per the
					// playbook's declared spec and returns the body +
					// bound args. The agent (skill or MCP-driven)
					// executes the body; the server does not.
					r.Get("/playbooks", s.handleListPlaybooks)
					r.Get("/playbooks/{ref}", s.handleShowPlaybook)
					r.Post("/playbooks/{ref}/run", s.handleRunPlaybook)

					// Incremental sync — returns items changed since a timestamp
					r.Get("/changes", s.handleGetChanges)
				})
			})

			// Search
			r.Get("/search", s.handleSearch)

			// My watches (TASK-2533), cross-workspace — mirrors
			// /auth/tokens' shape for a user-scoped-not-workspace-scoped
			// resource. `pad watch list`. Create/delete are per-item and
			// live under /workspaces/{ws}/items/{itemSlug}/watch instead
			// (they need the item's workspace context to resolve the
			// ref/slug the CLI's positional arg names).
			r.Get("/watches", s.handleListWatches)

			// MCP tool-surface descriptor (PLAN-1888 / TASK-1891). Serves
			// the catalog JSON (the nine env.Catalog tools + per-action
			// read_only flags) for the browser-side WebMCP layer to build
			// tool descriptors. Inside the authed group so it inherits
			// TokenAuth/SessionAuth/CSRFProtect/RequireAuth — same-origin
			// session/token only, NOT the bearer-gated /mcp infra path.
			// The handler nil-checks toolSurfaceJSON: 404 when the
			// serializer hasn't been injected (mirrors the SetMCPTransport
			// gating). Exposes only catalog descriptors — no route table,
			// handler internals, or other server state.
			r.Get("/mcp/tool-surface", s.handleMCPToolSurface)
		})

		// Cross-workspace wiki-link resolver (IDEA-1492). Resolves
		// `[[workspace::REF]]` links emitted by the markdown renderer to
		// the canonical item URL via a 302 redirect. Lives outside /api/v1
		// because rendered HTML hrefs target user-facing paths, not API
		// endpoints. Registered at the outer group level so chi matches
		// these URLs ahead of the catch-all SPA handler. ACL check matches
		// existing workspace-access semantics — 404 (not 403) on no-access
		// so we don't leak whether a workspace exists.
		//
		// URL shape: `/-/r/{workspace}/{ref}` — the leading `-/r/` prefix
		// is structurally impossible to collide with any user-namespace
		// URL because username slugs require a leading letter (slugify
		// rule), so no existing or future page route under
		// /{username}/... can shadow this resolver, and no collection
		// slug under /{u}/{ws}/{coll}/... can intercept it
		// (slug grammar also requires letter-led). This replaces the
		// earlier `/{username}/{workspace}/ref/{ref}` shape that risked
		// collision with collection slugs named "ref" on pre-existing
		// data (Codex round-2 P1.4 — picked Option B over a migration
		// because the feature is unshipped, the new shape is more
		// defensive, and the only cost is a frontend emit-shape change).
		r.Get("/-/r/{workspace}/{ref}", s.handleResolveCrossWorkspaceRef)
	}) // end r.Group (full middleware stack)

	s.router = r
}

// SetWebUI sets the embedded web UI filesystem for serving the SPA.
func (s *Server) SetWebUI(fsys fs.FS) {
	s.webFS = fsys
	s.ensureRouter()
	s.router.Handle("/*", s.spaHandler())
}

func (s *Server) spaHandler() http.Handler {
	fileServer := http.FileServer(http.FS(s.webFS))
	indexHTML, err := fs.ReadFile(s.webFS, "index.html")
	if err != nil {
		// Embedded web UI is missing — fail fast instead of silently
		// serving blank HTML to every request. This indicates a broken
		// build, so the server should refuse to start.
		panic(fmt.Sprintf("spaHandler: failed to read embedded index.html: %v", err))
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, "/api/") {
			http.NotFound(w, r)
			return
		}

		cleanPath := strings.TrimPrefix(path, "/")
		if cleanPath != "" {
			if _, err := fs.Stat(s.webFS, cleanPath); err == nil {
				if strings.Contains(path, "/immutable/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "no-cache")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		// Generate per-request nonce for inline script CSP
		nonce := generateCSPNonce()

		// Inject nonce into inline <script> tags (SvelteKit bootstrap)
		html := bytes.Replace(indexHTML, []byte("<script>"), []byte(fmt.Sprintf(`<script nonce="%s">`, nonce)), -1)

		// Set nonce-based CSP (overrides the strict default from SecurityHeaders).
		// - 'nonce-<N>' authorizes the SvelteKit bootstrap <script> we inject below.
		// - 'strict-dynamic' lets that trusted script dynamically import() the
		//   SvelteKit runtime chunks without listing every build-hashed path. In
		//   browsers that honor CSP L3, 'strict-dynamic' supersedes the 'self'
		//   host-list, so an XSS gap that injects <script src="//evil.com"> is
		//   rejected even though 'self' is present. 'self' stays as a fallback
		//   for older browsers that don't implement strict-dynamic.
		// - script-src-attr 'none' blocks inline event handlers regardless of the
		//   script-src nonce — per CSP spec, event attributes bypass script-src.
		w.Header().Set("Content-Security-Policy", fmt.Sprintf(
			"default-src 'self'; script-src 'self' 'nonce-%s' 'strict-dynamic'; script-src-attr 'none'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'",
			nonce))

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.WriteHeader(http.StatusOK)
		w.Write(html)
	})
}

// ensureRouter lazily initializes the router on first use, so all Set*
// configuration is applied before the middleware chain is built.
func (s *Server) ensureRouter() {
	s.routerOnce.Do(func() {
		s.setupRouter()
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.ensureRouter()
	s.router.ServeHTTP(w, r)
}

// httpIdleTimeout caps the keep-alive idle window on every HTTP
// connection. Tracked here (not in handlers_events.go) because it
// applies to ALL connections, not just SSE — but it has a hard
// invariant relationship with sseKeepaliveInterval: an idle SSE
// stream is kept alive by periodic comment writes, and those must
// land more frequently than IdleTimeout or the connection will be
// closed mid-stream by the http.Server. The guard in
// handlers_events.go's init() enforces 3 × sseKeepaliveInterval <
// httpIdleTimeout so we tolerate one or two missed/dropped writes
// (network blip, scheduler hiccup) before tripping the deadline.
const httpIdleTimeout = 120 * time.Second

func (s *Server) ListenAndServe(addr string) error {
	s.ensureRouter()

	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           s.router,
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       httpIdleTimeout,
		// Cap total header bytes (default 1 MB) to 64 KB — well above any
		// legitimate request (cookies, auth, content-type, a few CSRF/CORS
		// headers) and tight enough to cheaply reject header-flood DoS.
		MaxHeaderBytes: 64 * 1024,
		// WriteTimeout left at 0 — SSE connections are long-lived.
		// Non-SSE handlers should use per-request context deadlines.
	}

	slog.Info("Pad server listening", "addr", addr)
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully drains in-flight requests and stops the HTTP server.
// The provided context controls how long to wait for active connections.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

// Handler returns the configured HTTP handler (router).
// Useful for testing with httptest.NewServer.
func (s *Server) Handler() http.Handler {
	s.ensureRouter()
	return s.router
}

// --- helpers ---

func jsonContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 7 && r.URL.Path[:7] == "/api/v1" {
			w.Header().Set("Content-Type", "application/json")
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode JSON response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

// writeInternalError logs the real error server-side and sends a generic
// message to the client. This prevents leaking SQL errors, file paths,
// and other internal details.
func writeInternalError(w http.ResponseWriter, err error) {
	// The store's NUL refusal is a DECISION, not a fault, and it is mapped
	// here rather than at each handler for the same reason Layer A lives at
	// the driver: this is the one funnel every 500 already passes through, so
	// mapping it once covers every handler that exists and every one that will
	// (DOC-2823 S1). Enumerating error blocks is what the item-title unit had
	// to do three times in one function before a structural test caught the
	// third.
	//
	// 400, matching what the HTTP gate answers for the SAME value refused at
	// the door. Two statuses for one rule would be the layers disagreeing
	// again, in the response this time instead of in the predicate.
	//
	// The honest residual: a value can reach the store from something the
	// SERVER composed rather than the caller supplied — that is BUG-2814's
	// re-emit population — and for those a 400 tells the caller their request
	// was bad when it was our stored data. It is still the better answer than
	// 500, because the request is understood and will be refused identically
	// on retry, and the log line below keeps the detail. If the re-emit case
	// ever needs its own status, it needs its own error type first.
	if reason, ok := nulRefusalReason(err); ok {
		slog.Warn("write refused: invalid text parameter", "error", err)
		writeError(w, http.StatusBadRequest, "bad_request", reason)
		return
	}
	// The outbox row cap is likewise a decision, not a fault, and is mapped in
	// the same funnel for the same reason (BUG-2827). 413 with a *_too_large
	// code follows the house precedent set by rename_cascade_too_large rather
	// than inventing a second spelling for "this operation produces more than
	// the server will process in one go".
	//
	// The literal reading of 413 is that the REQUEST entity is too large, and
	// here the request is small while the event it derives is not. Taken
	// alone that argues for 422. It loses to consistency: an operator watching
	// for cascade refusals should not have to know which of two size bounds
	// fired to know which status to grep for, and the message says plainly
	// what was actually too big.
	//
	// Composed from the TYPED fields, never by splicing err.Error(), so no
	// wrapper the call path added is published to the caller.
	var oversized *store.OversizedOutboxPayloadError
	if errors.As(err, &oversized) {
		slog.Warn("write refused: outbox payload over the size limit",
			"event_type", oversized.EventType, "bytes", oversized.Bytes, "limit", oversized.Limit)
		// The message names WHAT was measured, because the store refuses on two
		// different measurements — the member content before marshalling, and
		// the row as stored — and a caller told only "%d bytes" for both
		// cannot reconcile two different numbers for one mutation (codex
		// round 3).
		writeError(w, http.StatusRequestEntityTooLarge, "event_payload_too_large",
			fmt.Sprintf("This change would record a %s event larger than the server will store in one "+
				"row: %s is %d bytes, and the limit is %d. Split the change into smaller ones and try again.",
				oversized.EventType, oversized.Measured, oversized.Bytes, oversized.Limit))
		return
	}
	slog.Error("internal server error", "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "An internal error occurred")
}

// nulRefusalReason classifies the store's NUL refusal and returns the sentence
// a caller should be given.
//
// ONE classifier, SEVERAL envelopes — and the several is deliberate rather than
// an omission (codex round 1, finding 4). writeInternalError covers every
// handler that lets an error reach its generic arm, but three paths carry their
// own error shape for their own reasons: createItemChecked returns a typed
// *itemCreateError for its caller to write, a bulk op reports per-item failures
// inside an otherwise successful response, and the cross-workspace copy answers
// with a deliberately retry-DISCOURAGING message because its failure may have
// committed (PLAN-2357 DR-13).
//
// Those envelopes should stay different. What must not differ is the
// CLASSIFICATION and the wording, which is what this function makes shared —
// the same discipline as the item-title unit's writeInvalidItemTitle, learned
// there by getting it wrong in three error blocks of one function.
func nulRefusalReason(err error) (string, bool) {
	var badText *store.InvalidTextParameterError
	if errors.As(err, &badText) {
		return badText.Reason, true
	}
	return "", false
}

// defaultJSONBodyLimit is the default cap applied to JSON request bodies
// by decodeJSON. Every /api/* POST/PATCH is comfortably small in practice
// (items, collections, auth payloads — all well under 100 KB), so the
// 2 MB cap is several orders of magnitude above real traffic while still
// cheap to hold in memory per request. Callers who legitimately need
// more — bulk imports — should call decodeJSONWithLimit explicitly.
const defaultJSONBodyLimit = 2 << 20 // 2 MiB

// decodeJSON reads and unmarshals the JSON body into v. Wraps the body in
// http.MaxBytesReader so an attacker can't exhaust memory by POSTing a
// multi-GB JSON blob — without this, json.NewDecoder.Decode happily
// streams the whole body into a single allocation.
func decodeJSON(r *http.Request, v interface{}) error {
	return decodeJSONWithLimit(r, v, defaultJSONBodyLimit)
}

// decodeJSONWithLimit is the size-configurable variant. Use this for
// endpoints that accept large payloads (e.g. bulk-import) where the
// default cap is too small — but always pass an explicit cap, never
// remove the wrapper.
func decodeJSONWithLimit(r *http.Request, v interface{}, maxBytes int64) error {
	raw, err := readBodyForDecode(r, maxBytes)
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return decodeJSONBytes(raw, v)
}

// decodeJSONRepairingNUL is decodeJSONWithLimit with exactly one difference,
// and the difference is deliberately NOT a bypass (DOC-2823 S3, BUG-2810).
//
// The workspace import's --repair-nul flag exists because a self-hoster whose
// database predates the enforcement can EXPORT a workspace and then not import
// it back: the server emits a payload it will refuse. Dave's day-54 ruling
// ships the flag with the default staying strict.
//
// What the flag does is repair the body and then run the SAME gate on the
// repaired bytes. It does not skip the gate, and it must not: a decode path
// that does is precisely the door BUG-2803 spent thirty rounds closing, and it
// would be reachable from the endpoint carrying the largest attacker-controlled
// body in the product. So a value the repair cannot fix is still refused, by
// the same function, with the same error.
//
// A body that is not valid JSON is left alone, so its own decode error is what
// the caller reports: a raw NUL BYTE inside a JSON string makes the document
// invalid, and replacing one would turn a body the decoder rejects into one it
// accepts. Widening what parses is not this flag's job.
//
// The count of rewritten values, and any reason the repair declined to act,
// are recorded on the tally the caller passes in.
func decodeJSONRepairingNUL(r *http.Request, v interface{}, maxBytes int64, t *nulRepairTally) error {
	raw, err := readBodyForDecode(r, maxBytes)
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	// The tally owns the repair, so the count and the "could not repair, and
	// why" both come back through one object rather than through a return
	// value the caller has to remember to record. The first version returned
	// the count and one caller dropped it, which is how the header reported 0
	// for an import that had rewritten a value.
	return decodeJSONBytes(t.Apply(raw), v)
}

// repairBodyNULEscapes repairs a JSON body the way the GATE reads it, and
// returns how many values it changed.
//
// IT MIRRORS bodyDecodesNUL's WALK, and the first version did not — it scanned
// the raw bytes for a live escape, which is right for a value the gate reads at
// the top level and wrong for the one that actually matters. An item's `fields`
// blob travels through an export as a STRING: the stored text
// `{"a":"x\u0000y"}` is marshalled into the body as `"{\"a\":\"x\\u0000y\"}"`,
// with a DOUBLED backslash, which a raw scan must leave alone because at that
// layer it is literal text. The gate refuses it anyway, because it decodes the
// body first and re-parses that string as the document it is. So a raw-byte
// repair left `--repair-nul` unable to fix the single most common carrier in a
// real export, while passing every test whose fixture put the NUL in `content`
// (codex round 1).
//
// The walk below is therefore the same traversal, with the same classing, one
// verb changed: where bodyDecodesNUL asks textguard whether a value decodes to
// a NUL, this asks textguard to repair it. Two walks of one shape in one
// package is a risk, and the mitigation is that they are measured against the
// same corpus in both directions rather than reviewed for similarity.
//
// A body that is not valid JSON is returned untouched, so its own decode error
// is what the caller reports and a malformed body cannot be made to parse.
// A body with nothing to repair is returned BYTE-IDENTICAL — the re-encode
// happens only when something actually changed.
func repairBodyNULEscapes(raw []byte) (out []byte, replaced int, declined string) {
	if !json.Valid(raw) {
		return raw, 0, ""
	}

	// DUPLICATE MEMBERS ARE A REFUSAL, NOT A REPAIR (codex round 4).
	//
	// The walk below decodes into map[string]any, where a repeated key keeps
	// only the LAST value. The typed decode that follows does not agree: it
	// unmarshals members in order into the same struct field, so two
	// `"workspace"` objects MERGE there and collapse here. Repairing such a
	// body would therefore change what gets imported, which is outside what
	// this flag is allowed to do — the contract is "replace the NULs and
	// nothing else".
	//
	// Refusing to act is the safe half of that: the body is returned untouched
	// and the gate judges it exactly as it would without the flag. A real
	// export cannot contain duplicate members (json.Marshal does not emit
	// them), so this costs nothing an operator will meet by accident. Rewriting
	// such a body faithfully needs a token-preserving pass, which is BUG-2812's
	// token-walk and not a rider on this.
	if key, dup := firstDuplicateJSONKey(raw); dup {
		return raw, 0, "the payload repeats the member " + strconv.Quote(key) +
			", and repairing it would change which value is imported"
	}

	// UseNumber, so a number wider than float64 is not silently re-emitted in
	// scientific notation on its way back out.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return raw, 0, ""
	}

	repaired, n := repairDecodedNULs(decoded, false)
	if n == 0 {
		return raw, 0, ""
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(repaired); err != nil {
		// Re-encoding a value that came out of a decode should not fail. If it
		// somehow does, returning the ORIGINAL leaves the gate to refuse it,
		// which is the safe direction.
		return raw, 0, ""
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), n, ""
}

// firstDuplicateJSONKey reports the first object member name that appears twice
// in the same object, at any depth.
//
// A token walk rather than a decode, because a decode is exactly what loses the
// information: by the time there is a map, the duplicate is gone.
//
// Malformed input answers false — the caller has already checked json.Valid,
// and a decode error there is the caller's to report, not this function's to
// duplicate.
func firstDuplicateJSONKey(raw []byte) (string, bool) {
	type frame struct {
		isObject  bool
		seen      map[string]bool
		expectKey bool
	}
	var stack []*frame
	top := func() *frame {
		if len(stack) == 0 {
			return nil
		}
		return stack[len(stack)-1]
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false // EOF, or malformed — nothing to report either way.
		}
		if d, isDelim := tok.(json.Delim); isDelim {
			switch d {
			case '{':
				stack = append(stack, &frame{isObject: true, seen: map[string]bool{}, expectKey: true})
			case '[':
				stack = append(stack, &frame{})
			case '}', ']':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				// The container that just closed WAS a value of its parent, so
				// the parent's next token is a key again.
				if f := top(); f != nil && f.isObject {
					f.expectKey = true
				}
			}
			continue
		}
		f := top()
		if f == nil || !f.isObject {
			continue
		}
		if f.expectKey {
			if name, isString := tok.(string); isString {
				if f.seen[name] {
					return name, true
				}
				f.seen[name] = true
			}
			f.expectKey = false
			continue
		}
		// A scalar value; the next token in this object is a key.
		f.expectKey = true
	}
}

// repairDecodedNULs walks a decoded body, repairing every string the gate would
// refuse, and counts the VALUES it changed.
//
// The count is values rather than escapes because at this layer an escape is
// not a thing that exists any more — the outer decode has already resolved it,
// and a nested document may carry several. "Three values were rewritten" is
// also the sentence an operator can check against the report.
//
// inUserData carries the same meaning as in bodyDecodesNUL: below a
// JSON-encoded field key nothing re-parses the strings, so they are ordinary
// text and only a raw NUL matters.
func repairDecodedNULs(v any, inUserData bool) (any, int) {
	switch t := v.(type) {
	case string:
		// Both arms of the gate check exactly ContainsNUL here, so both repair
		// exactly raw NULs. The escape form is only meaningful one level up,
		// where a string is re-parsed as a document.
		repaired := textguard.Repair(t, false)
		if repaired == t {
			return t, 0
		}
		return repaired, 1

	case map[string]any:
		out := make(map[string]any, len(t))
		count := 0
		for k, sub := range t {
			// KEYS TOO. The gate refuses a NUL in a key, so a repair that only
			// touched values would leave the body refused with nothing to show
			// for it.
			key := textguard.Repair(k, false)
			if key != k {
				count++
			}

			if !inUserData && isJSONEncodedFieldKey(k) {
				if str, isString := sub.(string); isString {
					// The one place the ESCAPE form is repaired: this string is
					// re-parsed as a document, so it gets both checks, exactly
					// as the gate gives it both.
					repaired := textguard.Repair(str, true)
					if repaired != str {
						count++
					}
					out[key] = repaired
					continue
				}
				// The field's natural shape — an object or array the server
				// marshals itself. Everything below is caller data.
				sr, n := repairDecodedNULs(sub, true)
				out[key] = sr
				count += n
				continue
			}

			sr, n := repairDecodedNULs(sub, inUserData)
			out[key] = sr
			count += n
		}
		return out, count

	case []any:
		out := make([]any, len(t))
		count := 0
		for i, sub := range t {
			sr, n := repairDecodedNULs(sub, inUserData)
			out[i] = sr
			count += n
		}
		return out, count

	default:
		// Numbers, booleans, null. json.Number is deliberately carried through
		// untouched so it re-encodes as the literal it arrived as.
		return v, 0
	}
}

// decodeJSONBytes is everything decodeJSONWithLimit does once the body has been
// read: the empty-body contract, the NUL gate, and the unmarshal.
//
// Extracted so the --repair-nul path can insert a repair between the read and
// the gate WITHOUT reimplementing any of the three, which is what keeps the
// gate the single decider.
func decodeJSONBytes(raw []byte, v interface{}) error {
	// An EMPTY (or whitespace-only) body must keep returning a wrapped
	// io.EOF. json.Decoder.Decode answered io.EOF there and at least one
	// caller depends on it — handlers_playbooks.go treats
	// errors.Is(err, io.EOF) as "no arguments supplied" and runs anyway —
	// while json.Unmarshal answers a SyntaxError instead, which that check
	// cannot see. Found by TestPlaybookRunAcceptsEmptyBody, which is exactly
	// the wiring a helper-level change is blind to.
	// Trim only the four bytes JSON itself calls whitespace. bytes.TrimSpace
	// uses unicode.IsSpace, which also strips \v, \f, U+00A0 and friends —
	// none of which encoding/json accepts. With TrimSpace a body of just
	// "\v" looked EMPTY here and returned io.EOF, so an EOF-tolerant caller
	// (playbook run, share links) treated a syntactically invalid body as an
	// ABSENT one and proceeded. Same Go-versus-spec whitespace divergence
	// that bites when a Go trim stands in for another grammar's definition
	// (codex round 22).
	if len(bytes.Trim(raw, " \t\r\n")) == 0 {
		return fmt.Errorf("invalid JSON: %w", io.EOF)
	}
	// Refuse a decoded NUL BEFORE unmarshalling, so the value never exists
	// in a Go string that a handler could hand to the store. See
	// bodyDecodesNUL for why the body needs its own rule and why the check
	// cannot be a substring search. BUG-2803.
	if bodyDecodesNUL(raw) {
		return errJSONBodyNUL
	}
	// json.Unmarshal rather than a Decoder over the buffer: it is the
	// cheaper of the two by ~2x in total allocation (see readBodyForDecode's
	// measurement), and it REFUSES trailing non-whitespace after the JSON
	// value where Decode silently ignores it. That second difference is a
	// deliberate behaviour change in the same direction as this fix —
	// malformed input is refused at the door rather than partly consumed —
	// and it is the only compatibility change in BUG-2803. Trailing
	// whitespace, which real clients do send, is still accepted.
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// errJSONBodyNUL is returned by decodeJSON when a string in the request body
// decodes to a value containing a NUL. Every decodeJSON caller already turns
// a decode error into a 400, so the refusal reaches the client as a client
// error at every call site without touching any of them.
//
// NOT every caller shows this message, and the earlier version of this
// comment claimed otherwise: many substitute a generic string of their own
// ("Invalid JSON body"). The status is uniform; the wording is not. Where a
// caller's own message would actively mislead — the OAuth registration
// endpoint's "Request body must be JSON", for a body that IS valid JSON —
// that caller distinguishes the two cases explicitly (codex round 8).
//
// The wording avoids writing the escape sequence literally: the message is
// rendered in terminals, logs and a browser, and a literal escape in an error
// string is the kind of thing an intermediate layer transforms.
var errJSONBodyNUL = errors.New(
	"request body contains a NUL character in a JSON string (a u0000 escape); text values cannot contain NUL")

// getWorkspaceID resolves workspace slug/ID from the request.
// If RequireWorkspaceAccess already resolved the workspace, reads from context.
// Otherwise falls back to direct resolution (for unauthenticated paths).
func (s *Server) getWorkspaceID(w http.ResponseWriter, r *http.Request) (string, bool) {
	// Fast path: already resolved by RequireWorkspaceAccess middleware
	if wsID, ok := r.Context().Value(ctxResolvedWorkspaceID).(string); ok && wsID != "" {
		return wsID, true
	}

	// Slow path: resolve directly (should rarely happen — only for routes
	// that don't go through RequireWorkspaceAccess)
	slugOrID := chi.URLParam(r, "slug")
	ws, err := s.resolveWorkspace(slugOrID, currentUser(r))
	if err != nil {
		writeInternalError(w, err)
		return "", false
	}
	if ws == nil {
		writeError(w, http.StatusNotFound, "not_found", "Workspace not found")
		return "", false
	}
	return ws.ID, true
}

// getWorkspace returns the full workspace object resolved by middleware.
// Falls back to direct resolution for routes without RequireWorkspaceAccess.
func (s *Server) getWorkspace(w http.ResponseWriter, r *http.Request) (*models.Workspace, bool) {
	// Fast path: use middleware-resolved ID
	if wsID, ok := r.Context().Value(ctxResolvedWorkspaceID).(string); ok && wsID != "" {
		ws, err := s.store.GetWorkspaceByID(wsID)
		if err != nil {
			writeInternalError(w, err)
			return nil, false
		}
		if ws != nil {
			return ws, true
		}
	}

	// Slow path: resolve from URL param
	slugOrID := chi.URLParam(r, "slug")
	ws, err := s.resolveWorkspace(slugOrID, currentUser(r))
	if err != nil {
		writeInternalError(w, err)
		return nil, false
	}
	if ws == nil {
		writeError(w, http.StatusNotFound, "not_found", "Workspace not found")
		return nil, false
	}
	return ws, true
}

// visibleCollectionIDs returns the set of collection IDs the current user can
// see in the given workspace. Returns nil if the user has "all" access (no
// filtering needed), or a non-nil slice for "specific" access. Unauthenticated
// users (fresh install) always get nil (all access), as do platform admins —
// but ONLY over a cookie session. Bearer-authed admins (CLI / PAT / MCP —
// detected via isBearerAuth) fall through to the store lookup and are scoped
// to their actual membership, matching RequireWorkspaceAccess's admin-bypass
// suppression for bearer auth (BUG-1616/1617) and reportVisibleCollections'
// gate (handlers_reports.go). Fixes BUG-1917 — this was the last of the
// bearer-gate consumers (buildDashboardResponse, handleListItems, the graph
// handler, handleCreateItem's collection-visibility check, and every other
// direct caller) still granting a bearer admin an unrestricted view.
func (s *Server) visibleCollectionIDs(r *http.Request, workspaceID string) ([]string, error) {
	user := currentUser(r)
	if user == nil || (user.Role == "admin" && !isBearerAuth(r)) {
		return nil, nil // No filtering for admins (cookie session) or unauthenticated
	}
	return s.store.VisibleCollectionIDs(workspaceID, user.ID)
}

// requireCollectionFullyVisible checks that the collection is visible to the
// requesting user under FULL-collection-access semantics (BUG-1920 —
// codex R2 follow-up). This is deliberately STRICTER than
// handleGetCollection's inline visibleCollectionIDs + isCollectionVisible
// check: VisibleCollectionIDs (workspace_members.go) intentionally folds in
// collections that are visible ONLY via an item-level grant, "so the
// collection appears in navigation" — item-level filtering is left to the
// handlers. That nav-lenient shape is correct for handleGetCollection
// (viewing collection metadata), but WRONG here: this helper's only callers
// are the four handlers that mint or list a collection-wide share link or
// grant, where passing would hand out (or reveal) access to the ENTIRE
// collection — an item grant on a single item inside it must NOT qualify.
//
// Mirrors reportVisibleCollections' fullCollIDs narrowing
// (handlers_reports.go): when the caller holds any item-level grants, the
// acceptable set narrows from the nav-lenient VisibleCollectionIDs set to
// the full-access-only set (guestResourceFilter's fullCollIDs — collection
// grants + member_collection_access + system collections, excluding
// item-grant-only collections).
//
// Writes a 404 and returns false if not visible; callers should invoke this
// immediately after resolving a collection by slug/ID.
func (s *Server) requireCollectionFullyVisible(w http.ResponseWriter, r *http.Request, workspaceID string, coll *models.Collection) bool {
	visible, err := s.checkCollectionFullyVisible(r, workspaceID, coll.ID)
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	if !visible {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return false
	}
	return true
}

// requireItemVisible checks that the item's collection is visible to the
// requesting user. For guests with item-level grants, also verifies that the
// specific item is granted (not just the collection). Writes a 404 and returns
// false if not. Callers should invoke this immediately after resolving an item
// by slug/ID.
//
// Thin shim over checkItemVisible — see that helper for the rules. This
// wrapper exists for the legacy call-sites that already hold a *http.Request
// pre-populated by RequireWorkspaceAccess; new callers without that middleware
// (e.g. handlers_ref_resolver.go) should use checkItemVisible directly with
// a manually-derived role.
func (s *Server) requireItemVisible(w http.ResponseWriter, r *http.Request, workspaceID string, item *models.Item) bool {
	visible, err := s.checkItemVisible(workspaceID, item, currentUser(r), workspaceRole(r), isBearerAuth(r))
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	if !visible {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return false
	}
	return true
}

// checkItemVisible is the context-free visibility decision. Returns (true,
// nil) when the (user, role) pair can see `item` under the same rules
// `requireItemVisible` enforces. Centralizes the rule set so the
// resolver route (IDEA-1492) and the middleware-gated handlers can't drift.
//
// Inputs:
//
//   - workspaceID — the resolved workspace's UUID.
//   - item — already-loaded item (so the helper doesn't re-resolve and
//     accidentally apply a different lookup path).
//   - user — currentUser(r) at the call site; nil for unauthenticated.
//   - role — workspaceRole(r) at the call site, or the role derived
//     manually by callers operating outside RequireWorkspaceAccess.
//   - isBearer — isBearerAuth(r) at the call site (BUG-1918). Narrows
//     rule 3 below the same way BUG-1616/1617 narrowed the analogous
//     bypasses in visibleCollectionIDs, resolverWorkspaceRole, and
//     guestResourceFilterCore: a platform admin's global read access is
//     a cookie-session / web-UI affordance only. A bearer-borne admin
//     (CLI / PAT / MCP) who is a restricted workspace member must fall
//     through to the same per-collection filter every other member
//     faces — otherwise BUG-1917's list-level scoping is bypassable by
//     guessing a ref and hitting the single-item endpoints directly.
//
// Rules (in order):
//
//  1. Tokenized-nil-user bypass: when currentUser == nil AND role is one
//     of the synthesized-by-middleware roles ("owner" for fresh-install,
//     "editor" for legacy workspace-scoped API tokens),
//     RequireWorkspaceAccess has already authorized the request — there
//     is no per-user filter to apply, and the user-nil rejection at
//     rule 2 would false-404 these callers. The bypass is SCOPED TO
//     user == nil — real authenticated members with role "owner" /
//     "editor" must still fall through to the per-collection filter,
//     otherwise a restricted editor member would bypass their own
//     collection_access="specific" gate (Codex round-3 regression of
//     the round-2 P1.1 fix).
//  2. nil user past rule 1 → not visible. Anonymous viewers without a
//     tokenized role have no item-read access (share links own the
//     public-read surface via /s/{token}).
//  3. Admin user via cookie session (user.Role == "admin" && !isBearer)
//     → always visible. Bearer-borne admins fall through to rule 4.
//  4. Otherwise: replay the guestResourceFilterCore + member-collection-
//     access logic that requireItemVisible used to inline, with a system-
//     collections union added to the item-grants branch (Codex round-2
//     P1.2 — restricted members with conventions/playbooks access plus
//     an unrelated item grant previously 404'd on system-collection items
//     because the item-grants branch only checked direct grants + the
//     member's explicit collection-access list).
func (s *Server) checkItemVisible(workspaceID string, item *models.Item, user *models.User, role string, isBearer bool) (bool, error) {
	return s.checkItemVisibleQ(s.store.Q(), workspaceID, item, user, role, isBearer)
}

// checkItemVisibleQ is checkItemVisible parameterized over its executor, so
// the cross-workspace copy's attachment authorizer can run the same
// visibility rule on the copy transaction's own connection instead of the
// pool while the transaction holds both workspace advisory locks
// (BUG-2409). Every other caller goes through the pool wrapper above; the
// decision logic is identical by construction — one body, two executors.
func (s *Server) checkItemVisibleQ(q store.Queryer, workspaceID string, item *models.Item, user *models.User, role string, isBearer bool) (bool, error) {
	// Tokenized-nil-user bypass. RequireWorkspaceAccess synthesizes
	// "owner" on fresh installs (UserCount == 0, currentUser == nil) and
	// "editor" for legacy workspace-scoped API tokens (currentUser ==
	// nil but tokenWorkspaceID matches). Both are authorized by the
	// middleware already. Real authenticated users with these roles
	// (workspace owners, member.Role=="editor", …) must NOT short-circuit
	// here — they have to walk the per-collection filter so
	// collection_access="specific" + member_collection_access actually
	// gates them (Codex round-3 — the round-2 fix dropped the
	// `user == nil` qualifier and accidentally disabled the gate for
	// every real editor too).
	if user == nil && (role == "owner" || role == "editor") {
		return true, nil
	}
	if user == nil {
		return false, nil
	}
	// Admin sees everything, but only for cookie-session auth (matches
	// visibleCollectionIDs's nil-filter shape). Bearer-borne admins
	// (BUG-1918) fall through to the same per-collection filter every
	// other member faces.
	if user.Role == "admin" && !isBearer {
		return true, nil
	}

	// Visibility filter: nil = unrestricted; non-nil = restricted to the slice.
	visibleIDs, err := s.store.VisibleCollectionIDsQ(q, workspaceID, user.ID)
	if err != nil {
		return false, err
	}
	if !isCollectionVisible(item.CollectionID, visibleIDs) {
		return false, nil
	}

	// Replay guestResourceFilterCore's logic without the *http.Request
	// dependency. Member-with-all-access short-circuits to "no item-level
	// filter"; guests + restricted members get the grant filter.
	if role != "guest" {
		member, err := s.store.GetWorkspaceMemberQ(q, workspaceID, user.ID)
		if err != nil {
			return false, err
		}
		if member != nil && (member.CollectionAccess == "all" || member.CollectionAccess == "") {
			// Full collection access — visibleIDs filter already passed.
			return true, nil
		}
	}

	grantCollIDs, grantedItemIDs, err := s.store.GuestVisibleResourcesQ(q, workspaceID, user.ID)
	if err != nil {
		return false, err
	}
	if len(grantedItemIDs) == 0 {
		// No item-level grants in play. visibleCollectionIDs already
		// determined the collection is reachable; visibility stands.
		return true, nil
	}

	// Item-level grants are active. The item is visible when:
	//   a) the collection itself has a full grant (any item passes), OR
	//   b) for restricted members: the collection is in member_collection_access
	//      (the member's explicit collection-access list), OR
	//   c) the item's collection is a system collection — restricted
	//      members always retain access to system collections (conventions,
	//      playbooks, …); pre-round-2 this branch missed the system-
	//      collections union that guestResourceFilterCore performed, so a
	//      restricted member with an item grant in a non-system collection
	//      was 404'd on a system-collection item they were entitled to see.
	//   d) the specific item is in the granted-items list.
	for _, id := range grantCollIDs {
		if id == item.CollectionID {
			return true, nil
		}
	}
	if role != "guest" {
		// member_collection_access path — restricted members see their
		// explicit collection-access list as full grants alongside any
		// item-level grants.
		memberColls, err := s.store.GetMemberCollectionAccessQ(q, workspaceID, user.ID)
		if err != nil {
			return false, err
		}
		for _, id := range memberColls {
			if id == item.CollectionID {
				return true, nil
			}
		}
		// System-collections union — mirror guestResourceFilterCore's
		// pre-round-2 behavior. ListSystemCollectionIDs is a workspace-
		// scoped lookup (no per-user filter), so the same call is correct
		// for every restricted member in the workspace.
		sysColls, err := s.store.ListSystemCollectionIDsQ(q, workspaceID)
		if err != nil {
			return false, err
		}
		for _, id := range sysColls {
			if id == item.CollectionID {
				return true, nil
			}
		}
	}
	for _, id := range grantedItemIDs {
		if id == item.ID {
			return true, nil
		}
	}
	return false, nil
}

// isItemVisibleToGuest checks if an item is visible given grant-based access,
// considering both full-collection grants and individual item grants.
// When fullCollIDs and grantedItemIDs are both nil, always returns true (no grant filtering).
func (s *Server) isItemVisibleToGuest(r *http.Request, workspaceID string, item *models.Item, fullCollIDs, grantedItemIDs []string) bool {
	if fullCollIDs == nil && grantedItemIDs == nil {
		return true
	}
	// Full collection grant covers all items in the collection
	for _, id := range fullCollIDs {
		if id == item.CollectionID {
			return true
		}
	}
	// Otherwise, the specific item must be in the granted items list
	for _, id := range grantedItemIDs {
		if id == item.ID {
			return true
		}
	}
	return false
}

// guestResourceFilter returns the full-collection IDs and granted item IDs for
// the current user if they need item-level grant filtering. Returns nil/nil for:
// - unauthenticated users
// - admin users
// - members with "all" collection access (grants should merge, not replace)
// For guests: returns direct collection grants as fullCollIDs + item grants.
// For restricted members: returns member_collection_access + system collections
// + direct collection grants as fullCollIDs, plus item grants as grantedItemIDs.
// This ensures item grants are additive to the member's existing access.
func (s *Server) guestResourceFilter(r *http.Request, workspaceID string) (fullCollIDs, grantedItemIDs []string, err error) {
	return s.guestResourceFilterCore(r, workspaceID, false)
}

// guestResourceFilterIncludeDeletedItems is the delta-sync variant
// of guestResourceFilter. It uses GuestVisibleResourcesIncludeDeleted
// under the hood so soft-deleted granted items still surface in the
// resulting ID set. Used by /items-changes (TASK-1354) so a guest /
// restricted member with an item-level grant still receives the
// `deleted:true` row when their granted item is soft-deleted —
// without this variant the grant ID vanishes before the delta
// query runs and the client keeps the stale entry forever (Codex
// review of TASK-1354 round 1 [P1]).
func (s *Server) guestResourceFilterIncludeDeletedItems(r *http.Request, workspaceID string) (fullCollIDs, grantedItemIDs []string, err error) {
	return s.guestResourceFilterCore(r, workspaceID, true)
}

// guestResourceFilterCore is the request-scoped wrapper around
// Store.ResolveBacklinksVisibility. Delegates the role-determination
// + merge logic to the store helper so cross-workspace backlinks
// callers can reuse the same code path without a request context
// (PLAN-1593 / TASK-1597).
//
// The wrapper still exists for two reasons:
//   - Admin bypass uses currentUser(r).Role rather than re-fetching
//     the user (saves one DB roundtrip per request on the hot path).
//   - The signature `(r *http.Request, workspaceID, includeDeleted) →
//     (fullCollIDs, grantedItemIDs, err)` is established across many
//     handlers — keeping it stable avoids a sprawling refactor.
//
// Admin bypass policy (BUG-1617 — companion to BUG-1616): the
// short-circuit only fires for cookie session auth. Bearer-borne
// admins (CLI / PAT / MCP — detected via isBearerAuth) fall through
// to the store helper which runs the regular member/grants pipeline
// against the platform-admin's actual workspace_members row. Without
// this, an admin's MCP token could pass RequireWorkspaceAccess's
// bearer gate (BUG-1616) on the URL's workspace but still see
// unrestricted visibility filters in any downstream backlinks /
// activity / delta-sync query that ran for the same workspace.
//
// The includeDeletedItems flag swaps the underlying grant query.
func (s *Server) guestResourceFilterCore(r *http.Request, workspaceID string, includeDeletedItems bool) (fullCollIDs, grantedItemIDs []string, err error) {
	return s.guestResourceFilterCoreQ(s.store.Q(), r, workspaceID, includeDeletedItems)
}

// guestResourceFilterCoreQ is guestResourceFilterCore parameterized over its
// executor — the cross-workspace copy's attachment authorizer runs it on the
// copy transaction's connection (BUG-2409); everything else uses the pool
// wrapper above.
func (s *Server) guestResourceFilterCoreQ(q store.Queryer, r *http.Request, workspaceID string, includeDeletedItems bool) (fullCollIDs, grantedItemIDs []string, err error) {
	user := currentUser(r)
	if user == nil {
		return nil, nil, nil
	}
	authIsBearer := isBearerAuth(r)
	if user.Role == "admin" && !authIsBearer {
		return nil, nil, nil
	}
	// Delegate to the request-independent helper. The store-side
	// helper duplicates the admin check via GetUser, but for the
	// request hot path we short-circuit above (cookie admin only)
	// so the duplicate lookup never fires for the common case.
	return s.store.ResolveBacklinksVisibilityQ(q, user.ID, workspaceID, includeDeletedItems, authIsBearer)
}

// isCollectionVisible checks if a collection ID is in the visible set.
// If visibleIDs is nil, all collections are visible.
func isCollectionVisible(collectionID string, visibleIDs []string) bool {
	if visibleIDs == nil {
		return true
	}
	for _, id := range visibleIDs {
		if id == collectionID {
			return true
		}
	}
	return false
}

// filterUserGrantsForCaller narrows collGrants/itemGrants — the TARGET
// user's grants, already loaded by the caller — down to what the CALLER can
// see. Only meaningful when caller != target; handleListUserGrants skips
// calling this for self-queries (a user can always see their own grants).
//
// BUG-1928: handleListUserGrants returned the target's raw grants
// (including collection_id/item_id) to any workspace owner unconditionally.
// A restricted owner (collection_access="specific") could enumerate
// hidden-resource IDs this way — the disclosure half of the primitive
// BUG-1923's handlers fixed the action half of (know-the-ID → operate-on-it).
//
// Reuses the existing guestResourceFilter/isCollectionVisible/
// isItemVisibleToGuest helpers rather than a bespoke visibility pass:
// guestResourceFilter's fullCollIDs is already the STRICT full-access set
// (member_collection_access ∪ system collections ∪ direct collection
// grants, excluding item-grant-only collections) — the same strict set
// requireCollectionFullyVisible narrows to — so collection grants are
// filtered directly against it with no extra narrowing step.
//
// Filtering is pure ID-set membership: no parent collection/item lookup is
// needed to decide visibility, so a grant on a soft- (or even hard-)
// deleted parent is filtered the same as any other grant, matching #798's
// "still revocable/inspectable" precedent for grants on archived resources.
// The one exception is item grants, which only carry an item_id — those are
// resolved to their collection_id via a single bulk GetItemCollectionRefs
// call (state-agnostic; no deleted_at filter) rather than N per-grant
// lookups.
func (s *Server) filterUserGrantsForCaller(r *http.Request, workspaceID string, collGrants []models.CollectionGrant, itemGrants []models.ItemGrant) ([]models.CollectionGrant, []models.ItemGrant, error) {
	fullCollIDs, grantedItemIDs, err := s.guestResourceFilter(r, workspaceID)
	if err != nil {
		return nil, nil, err
	}
	if fullCollIDs == nil && grantedItemIDs == nil {
		// Unrestricted caller (admin/cookie session, or a member with
		// full collection access) — no filtering, and no further store
		// calls needed.
		return collGrants, itemGrants, nil
	}

	filteredColl := make([]models.CollectionGrant, 0, len(collGrants))
	for _, g := range collGrants {
		if isCollectionVisible(g.CollectionID, fullCollIDs) {
			filteredColl = append(filteredColl, g)
		}
	}

	filteredItem := make([]models.ItemGrant, 0, len(itemGrants))
	if len(itemGrants) > 0 {
		itemIDs := make([]string, len(itemGrants))
		for i, g := range itemGrants {
			itemIDs[i] = g.ItemID
		}
		refs, err := s.store.GetItemCollectionRefs(workspaceID, itemIDs)
		if err != nil {
			return nil, nil, err
		}
		collByItem := make(map[string]string, len(refs))
		for _, ref := range refs {
			collByItem[ref.ID] = ref.CollectionID
		}
		for _, g := range itemGrants {
			collID, ok := collByItem[g.ItemID]
			if !ok {
				// item_grants.item_id is ON DELETE CASCADE, so a grant
				// row can't outlive its item — this should be
				// unreachable. Exclude defensively rather than show a
				// grant with no resolvable parent.
				continue
			}
			item := &models.Item{ID: g.ItemID, CollectionID: collID}
			if s.isItemVisibleToGuest(r, workspaceID, item, fullCollIDs, grantedItemIDs) {
				filteredItem = append(filteredItem, g)
			}
		}
	}

	return filteredColl, filteredItem, nil
}

// requireEditPermission checks if the user has edit access to the given item.
// For regular members (editor/owner), this uses the standard role check.
// For members with insufficient roles (e.g., viewers), it falls back to
// grant-based permissions so grants can override the base role.
// For guests, it resolves the effective permission from grants directly.
// Returns true if the request should continue, false if it was rejected with a 403.
//
// NEVER call this with a workspace ID other than the one the current
// request's URL resolved to. The `workspaceID` parameter makes it look
// reusable for a second workspace; it is not. The editor/owner fast path
// below reads workspaceRole(r), which RequireWorkspaceAccess populates only
// for the URL's workspace, so passing workspace B's ID applies workspace A's
// role — privilege escalation. Use AuthorizeCrossWorkspaceEdit
// (authz_cross_workspace.go) for any other workspace; it also checks the
// OAuth/MCP consent allow-list, which this helper does not (DR-10 of
// PLAN-2357).
func (s *Server) requireEditPermission(w http.ResponseWriter, r *http.Request, workspaceID string, itemID, collectionID string) bool {
	role := workspaceRole(r)

	// Editors and owners always have edit access
	if role != "guest" && requireRole(r, "editor") {
		return true
	}

	// For guests and members with insufficient role (e.g., viewers),
	// check grant-based permissions as an override.
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusForbidden, "forbidden", "Insufficient permissions")
		return false
	}

	perm, err := s.store.ResolveUserPermission(workspaceID, user.ID, itemID, collectionID)
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	if permissionLevel(perm) < permissionLevel("edit") {
		writeError(w, http.StatusForbidden, "forbidden", "Insufficient permissions")
		return false
	}
	return true
}

// resolveWorkspace resolves a workspace by slug or UUID, scoped to the
// authenticated user's accessible workspaces when a user context is present.
// Returns nil (not an error) if no workspace is found.
func (s *Server) resolveWorkspace(slugOrID string, user *models.User) (*models.Workspace, error) {
	// 1. Is it a UUID? Try resolving by ID first, then fall back to slug.
	//    A workspace slug could be UUID-shaped (e.g. imported data), so we
	//    can't short-circuit here.
	if isUUID(slugOrID) {
		ws, err := s.store.GetWorkspaceByID(slugOrID)
		if ws != nil || err != nil {
			return ws, err
		}
		// Not found by ID — fall through to slug-based resolution
	}

	// 2. No authenticated user — fall back to global slug lookup
	//    (fresh install, or pre-auth paths)
	if user == nil {
		return s.store.GetWorkspaceBySlug(slugOrID)
	}

	// 3. Admin users — global slug lookup (admins can see all workspaces)
	if user.Role == "admin" {
		return s.store.GetWorkspaceBySlug(slugOrID)
	}

	// 4. Auth-scoped slug resolution: find workspaces where user is owner or member
	workspaces, err := s.store.GetWorkspacesBySlugForUser(slugOrID, user.ID)
	if err != nil {
		return nil, err
	}

	if len(workspaces) == 1 {
		return &workspaces[0], nil
	}
	if len(workspaces) == 0 {
		return nil, nil
	}

	// Ambiguous: multiple workspaces match — this should be rare.
	// For now, return the first one. The 409 disambiguation is only needed
	// when we actually have per-owner slug uniqueness (after the unique
	// constraint is changed). Currently slugs are globally unique.
	return &workspaces[0], nil
}

// isUUID is defined in handlers_items.go

// getWorkspaceDocument resolves workspace slug and document ID from URL params.
func (s *Server) getWorkspaceDocument(w http.ResponseWriter, r *http.Request) (string, *models.Document, bool) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return "", nil, false
	}

	docID := chi.URLParam(r, "docID")
	doc, err := s.store.GetDocument(docID)
	if err != nil {
		writeInternalError(w, err)
		return "", nil, false
	}
	if doc == nil || doc.WorkspaceID != workspaceID {
		writeError(w, http.StatusNotFound, "not_found", "Document not found")
		return "", nil, false
	}
	return workspaceID, doc, true
}
