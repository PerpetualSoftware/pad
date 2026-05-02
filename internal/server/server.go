package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/billing"
	"github.com/PerpetualSoftware/pad/internal/email"
	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/metrics"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/webhooks"
)

type Server struct {
	store                 *store.Store
	router                *chi.Mux
	routerOnce            sync.Once            // ensures setupRouter runs once, after all config
	httpServer            *http.Server         // underlying HTTP server (set during ListenAndServe)
	webFS                 fs.FS                // embedded web UI static files (optional)
	events                events.EventBus      // real-time event bus (optional)
	webhooks              *webhooks.Dispatcher // webhook dispatcher (optional)
	email                 *email.Sender        // transactional email sender (optional)
	emailAPIKey           string               // Maileroo API key (used for unsubscribe HMAC)
	rateLimiters          *RateLimiters        // per-endpoint rate limiters
	baseURL               string               // public base URL for generating links (e.g. invite URLs)
	corsOrigins           string               // comma-separated CORS origins (empty = localhost defaults)
	secureCookies         bool                 // set Secure flag on cookies (for TLS deployments)
	metrics               *metrics.Metrics     // Prometheus metrics (optional)
	metricsToken          string               // shared bearer token for /metrics scrapes ("" = loopback-only)
	trustedProxyCIDRs     []*net.IPNet         // CIDRs allowed to set X-Forwarded-For (nil = proxy headers untrusted)
	ipChangeEnforceStrict bool                 // when true, reject sessions whose client IP differs from the one recorded at session creation
	sseMaxConnections     int                  // global SSE connection limit (0 = unlimited)
	sseMaxPerWorkspace    int                  // per-workspace SSE connection limit (0 = unlimited)
	cloudMode             bool                 // true when running as Pad Cloud (PAD_CLOUD=true or PAD_MODE=cloud)
	cloudSecrets          []string             // shared secrets for sidecar ↔ pad communication (supports rotation)
	cloudSidecar          CloudSidecar         // reverse pad → pad-cloud client (e.g. Stripe cancel on account delete); nil = not configured
	version               string               // release version (e.g. "dev", "1.2.3")
	commit                string               // git commit hash
	buildTime             string               // build timestamp
	twoFAChallengeSecret  []byte               // HMAC key for 2FA challenge tokens

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

	// storageInfoCache memoizes per-workspace storage usage summaries
	// behind a short TTL (storageInfoTTL). Reduces DB load on the
	// Settings → Storage page and quota-aware UI surfaces. Initialized
	// in newServer; never nil so handlers can call get/set without
	// guarding.
	storageInfoCache *storageInfoCache

	// importBundleMaxBytes caps a single workspace import bundle.
	// 0 → defaultImportBundleMaxBytes (2 GiB). Set via
	// SetImportBundleMaxBytes from cmd/pad/main.go using the
	// PAD_IMPORT_BUNDLE_MAX_BYTES env var so operators with larger
	// exports can opt in without recompiling.
	importBundleMaxBytes int64

	// orphanGC holds the periodic-sweep config + lifecycle for the
	// attachment orphan garbage collector (TASK-886). Configured via
	// SetOrphanGCConfig and started via StartOrphanGC. Stop() signals
	// the loop to exit and waits for it via the bg WaitGroup.
	orphanGC orphanGCConfig

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

	// bg tracks fire-and-forget goroutines spawned by request handlers
	// (TouchUserActivity in middleware_auth, async email sends, etc.) so
	// the server can drain them before shutdown / test cleanup. Without
	// this, tests using t.TempDir() race the still-running goroutine's
	// SQLite WAL write against TempDir RemoveAll, leaving "directory not
	// empty" cleanup errors in CI. See BUG-842.
	bg sync.WaitGroup
}

// goAsync spawns fn in a goroutine that's tracked by s.bg, so Stop() can
// wait for in-flight background work to finish. Use this for any
// fire-and-forget work that touches the database, filesystem, or external
// services from inside a request handler — never bare `go func() {...}()`.
func (s *Server) goAsync(fn func()) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		fn()
	}()
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
	s.bg.Wait()
	s.rateLimiters.Stop() // nil-safe via the RateLimiters receiver guard
}

func New(s *store.Store) *Server {
	return &Server{
		store:            s,
		rateLimiters:     NewRateLimiters(),
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

// SetEventBus attaches an event bus for real-time SSE streaming.
func (s *Server) SetEventBus(bus events.EventBus) {
	s.events = bus
}

// SetWebhookDispatcher attaches a webhook dispatcher for outgoing notifications.
func (s *Server) SetWebhookDispatcher(d *webhooks.Dispatcher) {
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

// SetSecureCookies enables the Secure flag on all cookies.
func (s *Server) SetSecureCookies(secure bool) {
	s.secureCookies = secure
}

// SetMetrics attaches Prometheus metrics to the server.
// Must be called before the first request is served.
func (s *Server) SetMetrics(m *metrics.Metrics) {
	s.metrics = m
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

// SetSSELimits configures global and per-workspace SSE connection limits.
// A value of 0 means unlimited.
func (s *Server) SetSSELimits(global, perWorkspace int) {
	s.sseMaxConnections = global
	s.sseMaxPerWorkspace = perWorkspace
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
// session's client IP changes mid-lifetime:
//   - mode == "strict": reject the request (session treated as possibly stolen)
//   - anything else (default): log to the audit log, update the stored IP,
//     and let the request through. Strict mode breaks legitimate mobility
//     (mobile roaming, VPN toggles) so it is opt-in for high-sensitivity
//     deployments via the PAD_IP_CHANGE_ENFORCE env var.
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
		return // No API key — leave email as-is (may still have env var config)
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
		r.Use(cors.Handler(cors.Options{
			AllowedOrigins: parseCORSOrigins(s.corsOrigins),
			AllowedMethods: []string{"GET", "POST", "PATCH", "PUT", "DELETE", "OPTIONS"},
			AllowedHeaders: []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token", "X-Share-Password"},
			// Credentials flag is gated on an operator explicitly listing
			// PAD_CORS_ORIGINS. The CLI uses Bearer tokens so the default
			// "no CORS_ORIGINS set" path doesn't need credential sharing;
			// leaving it off by default prevents cross-origin fetches from
			// a browser on a different site from piggy-backing cookies
			// on the victim's session.
			AllowCredentials: corsAllowCredentials(s.corsOrigins),
			MaxAge:           300,
		}))
		r.Use(s.TokenAuth)
		r.Use(s.SessionAuth)
		r.Use(s.RateLimit)
		r.Use(s.CSRFProtect)
		r.Use(s.RequireAuth)
		r.Use(jsonContentType)

		// SSE endpoint (outside jsonContentType middleware — but inherits auth)
		r.Get("/api/v1/events", s.handleSSE)

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
				r.Post("/users/{userID}/disable", s.handleAdminDisableUser)
				r.Post("/users/{userID}/enable", s.handleAdminEnableUser)

				// Invitations
				r.Get("/invitations", s.handleAdminListInvitations)
				r.Post("/invitations/{invID}/resend", s.handleAdminResendInvitation)
				r.Delete("/invitations/{invID}", s.handleAdminDeleteInvitation)

				// Plan limits
				r.Get("/limits", s.handleAdminGetLimits)
				r.Patch("/limits", s.handleAdminUpdateLimits)

				// Platform stats
				r.Get("/stats", s.handleAdminStats)
			})

			// Audit log (admin-only)
			r.Get("/audit-log", s.handleAuditLog)

			// Templates
			r.Get("/templates", s.handleListTemplates)

			// Convention Library
			r.Get("/convention-library", s.handleConventionLibrary)

			// Playbook Library
			r.Get("/playbook-library", s.handlePlaybookLibrary)

			// Invitations (outside workspace scope)
			r.Post("/invitations/{code}/accept", s.handleAcceptInvitation)

			// Share link resolution (outside workspace scope, no auth required)
			r.Get("/s/{token}", s.handleResolveShareLink)

			// Workspaces
			r.Route("/workspaces", func(r chi.Router) {
				r.Get("/", s.handleListWorkspaces)
				r.Post("/", s.handleCreateWorkspace)
				r.Post("/import", s.handleImportWorkspace)
				r.Put("/reorder", s.handleReorderWorkspaces)

				r.Route("/{slug}", func(r chi.Router) {
					r.Use(s.RequireWorkspaceAccess)

					r.Get("/", s.handleGetWorkspace)
					r.Patch("/", s.handleUpdateWorkspace)
					r.Delete("/", s.handleDeleteWorkspace)
					r.Get("/export", s.handleExportWorkspace)

					// Activity (workspace level)
					r.Get("/activity", s.handleListWorkspaceActivity)

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

					// User grants (all grants for a specific user in this workspace)
					r.Get("/users/{userID}/grants", s.handleListUserGrants)

					// Starred items
					r.Get("/starred", s.handleListStarredItems)

					// Items (cross-collection, v2)
					r.Get("/items", s.handleListItems)
					r.Route("/items/{itemSlug}", func(r chi.Router) {
						r.Get("/", s.handleGetItem)
						r.Patch("/", s.handleUpdateItem)
						r.Delete("/", s.handleDeleteItem)
						r.Post("/restore", s.handleRestoreItem)
						r.Post("/move", s.handleMoveItem)
						r.Get("/versions", s.handleListItemVersions)
						r.Post("/versions/{versionID}/restore", s.handleRestoreItemVersion)
						r.Get("/activity", s.handleListItemActivity)
						r.Get("/links", s.handleGetItemLinks)
						r.Post("/links", s.handleCreateItemLink)
						r.Get("/comments", s.handleListComments)
						r.Post("/comments", s.handleCreateComment)
						r.Get("/timeline", s.handleListItemTimeline)
						r.Get("/children", s.handleGetItemChildren)
						r.Get("/progress", s.handleGetItemProgress)
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
					})

					// Links (v2)
					r.Delete("/links/{linkID}", s.handleDeleteItemLink)

					// Share links (workspace-scoped management)
					r.Delete("/share-links/{linkID}", s.handleDeleteShareLink)
					r.Get("/share-links/{linkID}/views", s.handleShareLinkViews)

					// Comments (v2)
					r.Route("/comments/{commentID}", func(r chi.Router) {
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

					// Dashboard (v2)
					r.Get("/dashboard", s.handleGetDashboard)

					// Incremental sync — returns items changed since a timestamp
					r.Get("/changes", s.handleGetChanges)
				})
			})

			// Search
			r.Get("/search", s.handleSearch)
		})
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

func (s *Server) ListenAndServe(addr string) error {
	s.ensureRouter()

	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           s.router,
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
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
	slog.Error("internal server error", "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "An internal error occurred")
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
	// http.MaxBytesReader.Close() is a no-op; the decoder leaves r.Body at
	// EOF anyway. Setting this here also lets the server return a 413
	// automatically via the error we wrap below.
	if r.Body != nil {
		r.Body = http.MaxBytesReader(nil, r.Body, maxBytes)
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

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
// filtering needed), or a non-nil slice for "specific" access. Admins and
// unauthenticated users (fresh install) always get nil (all access).
func (s *Server) visibleCollectionIDs(r *http.Request, workspaceID string) ([]string, error) {
	user := currentUser(r)
	if user == nil || user.Role == "admin" {
		return nil, nil // No filtering for admins or unauthenticated
	}
	return s.store.VisibleCollectionIDs(workspaceID, user.ID)
}

// requireItemVisible checks that the item's collection is visible to the
// requesting user. For guests with item-level grants, also verifies that the
// specific item is granted (not just the collection). Writes a 404 and returns
// false if not. Callers should invoke this immediately after resolving an item
// by slug/ID.
func (s *Server) requireItemVisible(w http.ResponseWriter, r *http.Request, workspaceID string, item *models.Item) bool {
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	if !isCollectionVisible(item.CollectionID, visibleIDs) {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return false
	}

	// For users with item-level grants (guests or restricted members),
	// collection visibility may come from item-level grants.
	// We need to verify the user actually has a grant on this specific item
	// (not just another item in the same collection).
	// Uses guestResourceFilter which skips this check for members with "all" access.
	fullCollIDs, grantedItemIDs, grantErr := s.guestResourceFilter(r, workspaceID)
	if grantErr != nil {
		writeInternalError(w, grantErr)
		return false
	}
	if len(grantedItemIDs) > 0 {
		// If the collection has a full collection grant, the item is visible
		for _, id := range fullCollIDs {
			if id == item.CollectionID {
				return true
			}
		}
		// Check if this collection came from member_collection_access (not grants)
		if workspaceRole(r) != "guest" {
			memberColls, _ := s.store.GetMemberCollectionAccess(workspaceID, currentUserID(r))
			for _, id := range memberColls {
				if id == item.CollectionID {
					return true
				}
			}
		}
		// Otherwise, the specific item must be in the granted items list
		for _, id := range grantedItemIDs {
			if id == item.ID {
				return true
			}
		}
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return false
	}

	return true
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
	user := currentUser(r)
	if user == nil || user.Role == "admin" {
		return nil, nil, nil
	}

	role := workspaceRole(r)

	// For workspace members with "all" collection access, item grants should
	// not restrict their existing full visibility.
	if role != "guest" {
		member, err := s.store.GetWorkspaceMember(workspaceID, user.ID)
		if err != nil {
			return nil, nil, err
		}
		if member != nil && (member.CollectionAccess == "all" || member.CollectionAccess == "") {
			return nil, nil, nil
		}
	}

	// Get grant-based resources
	grantCollIDs, grantedItemIDs, err := s.store.GuestVisibleResources(workspaceID, user.ID)
	if err != nil {
		return nil, nil, err
	}

	// For guests, grant resources are the only source of access
	if role == "guest" {
		return grantCollIDs, grantedItemIDs, nil
	}

	// For restricted members ("specific" access), merge their normal
	// member_collection_access + system collections into fullCollIDs so
	// item grants are additive, not a replacement. This is critical:
	// without this merge, a member with access to collection A plus one
	// item grant in collection B would lose collection A in cross-collection
	// queries that use these IDs.
	fullCollSet := make(map[string]bool)
	for _, id := range grantCollIDs {
		fullCollSet[id] = true
	}

	// Add member_collection_access collections
	memberColls, err := s.store.GetMemberCollectionAccess(workspaceID, user.ID)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range memberColls {
		fullCollSet[id] = true
	}

	// Add system collections (always visible to members)
	sysColls, err := s.store.ListSystemCollectionIDs(workspaceID)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range sysColls {
		fullCollSet[id] = true
	}

	fullCollIDs = make([]string, 0, len(fullCollSet))
	for id := range fullCollSet {
		fullCollIDs = append(fullCollIDs, id)
	}

	return fullCollIDs, grantedItemIDs, nil
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

// requireEditPermission checks if the user has edit access to the given item.
// For regular members (editor/owner), this uses the standard role check.
// For members with insufficient roles (e.g., viewers), it falls back to
// grant-based permissions so grants can override the base role.
// For guests, it resolves the effective permission from grants directly.
// Returns true if the request should continue, false if it was rejected with a 403.
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
