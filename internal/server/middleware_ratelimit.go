package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// rateLimitConfig holds the rate and burst for a limiter.
type rateLimitConfig struct {
	Rate  rate.Limit // events per second
	Burst int        // max burst
	// Retention is how long an inactive key stays in memory before the
	// background cleanup evicts it. Must be at least as long as the rate
	// window (≈ burst / rate) or premature eviction lets an attacker reset
	// their bucket by waiting — defeating "N per hour" limits that pause
	// naturally between bursts. Zero means "use the default".
	Retention time.Duration
}

// defaultRetention is the minimum retention for a limiter whose config
// doesn't specify one. Suitable for sub-minute windows like per-IP login
// limiting; longer windows must set Retention explicitly.
const defaultRetention = 30 * time.Minute

// ipRateLimiter tracks per-key rate limiters with automatic cleanup.
type ipRateLimiter struct {
	mu        sync.Mutex
	limiters  map[string]*rateLimiterEntry
	config    rateLimitConfig
	retention time.Duration

	// stopCh / stopOnce / stopWg let Server.Stop() shut the cleanup
	// goroutine down. Without this, every call to NewRateLimiters spawned
	// 9 forever-sleeping goroutines that never exited — under -race the
	// accumulation pushed the test runtime past the 10-minute timeout.
	// See BUG-851.
	stopCh   chan struct{}
	stopOnce sync.Once
	stopWg   sync.WaitGroup
}

type rateLimiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newIPRateLimiter(cfg rateLimitConfig) *ipRateLimiter {
	retention := cfg.Retention
	if retention <= 0 {
		retention = defaultRetention
	}
	rl := &ipRateLimiter{
		limiters:  make(map[string]*rateLimiterEntry),
		config:    cfg,
		retention: retention,
		stopCh:    make(chan struct{}),
	}
	// Background cleanup of stale entries every 5 minutes. Tracked via
	// stopWg so Stop() can drain it before the surrounding Server is torn
	// down (BUG-851).
	rl.stopWg.Add(1)
	go rl.cleanup()
	return rl
}

// Stop signals the cleanup goroutine to exit and blocks until it does.
// Safe to call multiple times — stopOnce guards the channel close.
func (rl *ipRateLimiter) Stop() {
	if rl == nil {
		return
	}
	rl.stopOnce.Do(func() { close(rl.stopCh) })
	rl.stopWg.Wait()
}

func (rl *ipRateLimiter) getLimiter(key string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	entry, exists := rl.limiters[key]
	if !exists {
		limiter := rate.NewLimiter(rl.config.Rate, rl.config.Burst)
		rl.limiters[key] = &rateLimiterEntry{
			limiter:  limiter,
			lastSeen: time.Now(),
		}
		return limiter
	}
	entry.lastSeen = time.Now()
	return entry.limiter
}

func (rl *ipRateLimiter) cleanup() {
	defer rl.stopWg.Done()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stopCh:
			return
		case <-ticker.C:
			rl.mu.Lock()
			for key, entry := range rl.limiters {
				if time.Since(entry.lastSeen) > rl.retention {
					delete(rl.limiters, key)
				}
			}
			rl.mu.Unlock()
		}
	}
}

// RateLimiters holds all the rate limiters used by the server.
type RateLimiters struct {
	// Auth endpoints: strict limits per IP
	Auth *ipRateLimiter
	// Login attempts per email: catches credential-spraying that bypasses
	// the per-IP limit by rotating through a botnet. Consumed inside
	// handleLogin on every login attempt (success or failure) — a
	// legitimate user who only mistypes a couple of times never notices.
	AuthEmail *ipRateLimiter
	// Password reset: per-IP
	PasswordReset *ipRateLimiter
	// Registration: per-IP
	Register *ipRateLimiter
	// OAuth login: per-IP (higher limit since pad-cloud sidecar calls this)
	OAuthLogin *ipRateLimiter
	// Cloud admin: per-IP for sidecar-to-pad admin endpoints (plan, stripe, user lookup)
	CloudAdmin *ipRateLimiter
	// API: per-user (authenticated)
	API *ipRateLimiter
	// Search: per-user or per-IP
	Search *ipRateLimiter
	// InvitationPreview: per-IP limiter for the public, pre-auth
	// GET /api/v1/invitations/{code}/preview endpoint (BUG-1934). The
	// endpoint is always-200 by design, so the status code can't be used
	// to distinguish valid from invalid codes — this limiter is the second
	// enumeration defense, capping how fast an attacker can probe the code
	// space. Invite codes are 128-bit random (see CreateInvitation) so brute
	// force is already infeasible; this is defense in depth. Per-IP because
	// the caller is unauthenticated.
	InvitationPreview *ipRateLimiter
	// RecoveryCode caps how many recovery codes can be tried against a
	// single 2FA challenge token. Without it an attacker who captures a
	// valid challenge_token can grind through the small recovery-code
	// space before the 5-minute challenge expires.
	RecoveryCode *ipRateLimiter
	// SharePasswordIP throttles password guesses on a single share link from a
	// single source IP. Keyed on SHA-256(share ID)+client IP and charged on
	// every attempt BEFORE the bcrypt compare, so a single grinder is capped
	// (defeating the offline-fast attack) and can't burn server bcrypt CPU.
	// Per-IP (not link-wide) so one caller exhausting their own bucket can't
	// lock every legitimate viewer out. See handleResolveShareLink.
	SharePasswordIP *ipRateLimiter
	// SharePasswordShare caps the AGGREGATE guess rate against a single share
	// link across all source IPs — the defense the per-IP bucket alone can't
	// provide, since a botnet rotating addresses gets a fresh per-IP burst
	// from each. Keyed on SHA-256(share ID) only and charged BEFORE the bcrypt
	// compare (like the per-email AuthEmail gate on login), so it caps both
	// distributed guessing AND bcrypt CPU, and once exhausted it blocks even a
	// would-be-correct guess (no password oracle). It's charged only AFTER the
	// per-IP gate passes, so a single IP — capped at its own small burst —
	// contributes just a few tokens and can't drain the link-wide bucket on
	// its own; exhausting this requires a genuine botnet, and the burst is
	// sized so ordinary multi-viewer traffic never trips it. This is the same
	// bounded-lockout tradeoff AuthEmail accepts: for an unauthenticated
	// shared-secret URL a hard link-wide cap and zero DoS exposure can't
	// coexist, so we cap the guess rate and keep the residual lockout to a
	// self-healing botnet-only case. See handleResolveShareLink.
	SharePasswordShare *ipRateLimiter
	// MCPPerToken caps requests per individual bearer token on /mcp.
	// PLAN-943 / TASK-959: per-token (not per-IP) buckets so that
	// office-NAT-shared users don't share a quota, and a runaway
	// agent on one token can't burn through a user's entire quota
	// for other tokens. Keyed by SHA-256(bearer) so the raw token
	// never lives in the limiter map.
	//
	// 60 requests / minute / token, burst 60 (post-BUG-1430; was
	// originally 20). The original burst was sized for chatty
	// interactive usage; agentic batch onboarding regularly fans
	// out 20-30 parallel tool calls (workspace setup, item-create
	// bursts), so the burst was raised to match the general API
	// limiter's burst-60-per-user cap. Sustained rate stays 60/min
	// — abuse still gets throttled, just after a roomier burst.
	//
	// Retention 5 minutes — long enough to remember a quiet token
	// between calls, short enough that the limiter doesn't hold
	// dead tokens forever after revocation.
	MCPPerToken *ipRateLimiter
}

// NewRateLimiters creates rate limiters with sensible defaults.
func NewRateLimiters() *RateLimiters {
	return &RateLimiters{
		// Login: 5 attempts per minute per IP (= 5/60 per second, burst 5)
		Auth: newIPRateLimiter(rateLimitConfig{
			Rate:  rate.Limit(5.0 / 60.0),
			Burst: 5,
		}),
		// Per-email: 10 attempts per hour. Low enough to defeat credential
		// spraying from a botnet (which evades the per-IP limit by rotating
		// source addresses), high enough that a forgetful user mistyping
		// their own password never hits it under normal use.
		//
		// Retention must be ≥ the refill window (10 attempts / (10/hour) =
		// 60 min); otherwise the cleanup could evict the bucket between
		// bursts, letting an attacker pace their guesses to avoid the cap.
		// 2 hours gives plenty of margin.
		AuthEmail: newIPRateLimiter(rateLimitConfig{
			Rate:      rate.Limit(10.0 / 3600.0),
			Burst:     10,
			Retention: 2 * time.Hour,
		}),
		// Password reset: 3 per hour per IP (= 3/3600 per second, burst 3)
		PasswordReset: newIPRateLimiter(rateLimitConfig{
			Rate:  rate.Limit(3.0 / 3600.0),
			Burst: 3,
		}),
		// Registration: 5 per hour per IP (= 5/3600 per second, burst 5)
		Register: newIPRateLimiter(rateLimitConfig{
			Rate:  rate.Limit(5.0 / 3600.0),
			Burst: 5,
		}),
		// OAuth login/link: 20 per minute per IP (sidecar calls this — higher than regular auth)
		OAuthLogin: newIPRateLimiter(rateLimitConfig{
			Rate:  rate.Limit(20.0 / 60.0),
			Burst: 20,
		}),
		// Cloud admin: 30 per minute per IP for sidecar admin calls (plan changes, Stripe mapping)
		// These are cloud-secret gated but rate-limited for defense in depth.
		CloudAdmin: newIPRateLimiter(rateLimitConfig{
			Rate:  rate.Limit(30.0 / 60.0),
			Burst: 10,
		}),
		// API: 600 requests per minute per user/IP (= 10 per second, burst 60)
		// Local-first tool with SSE-driven UI needs headroom for cascading refreshes.
		API: newIPRateLimiter(rateLimitConfig{
			Rate:  rate.Limit(600.0 / 60.0),
			Burst: 60,
		}),
		// Search: 30 requests per minute per user/IP (= 30/60 per second, burst 10)
		Search: newIPRateLimiter(rateLimitConfig{
			Rate:  rate.Limit(30.0 / 60.0),
			Burst: 10,
		}),
		// InvitationPreview: 20 requests per minute per IP (= 20/60 per second,
		// burst 20). The /join page fetches this once on mount, so the ceiling
		// is generous enough for a shared-NAT team onboarding in a batch while
		// still capping code-enumeration probes at 20/min/IP (BUG-1934).
		InvitationPreview: newIPRateLimiter(rateLimitConfig{
			Rate:  rate.Limit(20.0 / 60.0),
			Burst: 20,
		}),
		// RecoveryCode: up to 6 attempts per challenge token before lockout.
		// Challenge tokens live for 5 minutes, so we only need the limiter to
		// remember that long — but retention defaults to 30 minutes so we
		// pick up a couple of wall-clock minutes of slop. Rate is effectively
		// "no refill over the window" since burst = 6 and the limiter won't
		// meaningfully refill in 5 min at 6/hour.
		RecoveryCode: newIPRateLimiter(rateLimitConfig{
			Rate:  rate.Limit(6.0 / 3600.0),
			Burst: 6,
		}),
		// SharePasswordIP: up to 5 guesses per share+IP, refilling at 10/hour. A
		// share password is entered by people who already know it, so a
		// legitimate viewer needs only a try or two — burst 5 leaves slop for
		// a mistype. The burst is tighter than the login limiter's (5/min,
		// which refills to full in a minute) and the slow 10/hour refill makes
		// the sustained budget far tighter still: it turns a would-be
		// offline-fast grind into a handful of guesses an hour, which no
		// non-trivial password survives being cracked at.
		//
		// Retention must be ≥ the refill window (burst / rate = 5 ÷ (10/hour)
		// = 30 min); otherwise cleanup could evict the bucket between guesses,
		// letting an attacker pace their probes to dodge the cap. 1 hour gives
		// margin.
		SharePasswordIP: newIPRateLimiter(rateLimitConfig{
			Rate:      rate.Limit(10.0 / 3600.0),
			Burst:     5,
			Retention: time.Hour,
		}),
		// SharePasswordShare: up to 60 guesses per link aggregated over all IPs,
		// refilling at 60/hour. This is the anti-botnet ceiling — a distributed
		// attacker rotating source addresses gets a fresh per-IP burst from
		// each, so without a link-wide cap they could still grind the password
		// fast. Charged before the compare (after the per-IP gate), so it caps
		// bcrypt CPU and the guess rate at 60/hour link-wide — no real password
		// survives that. Burst 60 is generous enough that ordinary multi-viewer
		// traffic (a team all opening a link after an announcement) never trips
		// it, and because the per-IP gate caps each address at 5 first, ~12
		// distinct IPs are needed to exhaust this — a single IP can't DoS the
		// link, and a botnet lockout self-heals at 1/min.
		//
		// Retention ≥ refill window (60 ÷ (60/hour) = 1 h); 2 h gives margin.
		SharePasswordShare: newIPRateLimiter(rateLimitConfig{
			Rate:      rate.Limit(60.0 / 3600.0),
			Burst:     60,
			Retention: 2 * time.Hour,
		}),
		// MCP per-token: 60 req/min sustained, burst 60. PLAN-943
		// TASK-959, bumped under BUG-1430. 60/60 = 1 req/sec —
		// written with explicit math rather than `rate.Limit(1)`
		// so adjacent limiters' "X / 60" idiom stays consistent at
		// a glance, but staticcheck SA4000 flags identical-
		// numerator-denominator division — hence the explicit
		// literal.
		//
		// Burst was originally 20, sized for "chatty interactive
		// use (Claude Desktop sends tools/list + a handful of tool
		// calls per session)." Agentic batch onboarding workloads
		// regularly exceed that — a fresh-workspace setup may fan
		// out 20-30 parallel `pad_item create` tool calls, and the
		// 21st+ failing with rate_limited (HTTP 429) on a brand-new
		// connection is a hostile first impression. Raising to 60
		// matches the general API limiter's burst (per-user,
		// 600/min, burst 60), so the MCP path doesn't impose a
		// tighter ceiling than the equivalent /api/v1 path. The
		// sustained 60/min rate stays unchanged — abuse still gets
		// throttled, just after a roomier burst.
		//
		// The 5-minute retention lets the limiter forget dead
		// tokens reasonably quickly after revocation while still
		// surviving idle periods between tool calls.
		MCPPerToken: newIPRateLimiter(rateLimitConfig{
			Rate:      rate.Limit(1.0), // 60 req/min = 1 req/sec
			Burst:     60,
			Retention: 5 * time.Minute,
		}),
	}
}

// Stop drains the cleanup goroutine of every limiter in the bundle. Called
// from Server.Stop() so test cleanup (and graceful shutdown) doesn't leak
// the forever-sleeping goroutines NewRateLimiters spawns. Safe to call
// on a nil receiver and idempotent per-limiter via stopOnce. See BUG-851.
//
// New limiters added to RateLimiters MUST be added to this list too —
// otherwise their cleanup goroutine leaks across Server lifetimes,
// reproducing BUG-851 the first time a test runner exhausts its
// goroutine quota.
func (rls *RateLimiters) Stop() {
	if rls == nil {
		return
	}
	for _, rl := range []*ipRateLimiter{
		rls.Auth,
		rls.AuthEmail,
		rls.PasswordReset,
		rls.Register,
		rls.OAuthLogin,
		rls.CloudAdmin,
		rls.API,
		rls.Search,
		rls.InvitationPreview,
		rls.RecoveryCode,
		rls.SharePasswordIP,
		rls.SharePasswordShare,
		rls.MCPPerToken,
	} {
		rl.Stop() // nil-safe via the receiver guard in (*ipRateLimiter).Stop
	}
}

// RateLimit is the general-purpose rate limiting middleware.
// It applies different limits based on the endpoint being hit.
func (s *Server) RateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.rateLimiters == nil {
			next.ServeHTTP(w, r)
			return
		}

		path := r.URL.Path
		ip := clientIP(r)

		// OAuth 2.1 registration endpoint (PLAN-943 TASK-1025).
		// /oauth/register is open by RFC 7591 design — Claude
		// Desktop / Cursor self-register without prior auth — but
		// without a limiter an attacker can flood the oauth_clients
		// table. Reuse the Register limiter (5/min/IP), same shape
		// as /api/v1/auth/register's protection. Codex review #372
		// round 2.
		//
		// Other /oauth/* endpoints (authorize, token, decide) ride
		// session cookies (authorize) or are PKCE-bound to a stored
		// code (token), so flooding them just spends CPU. They go
		// through fosite's own internal protections + the future
		// TASK-959 /mcp limiter; explicit /oauth/* limits beyond
		// /register can land alongside that work.
		if path == "/oauth/register" {
			l := s.rateLimiters.Register.getLimiter(ip)
			if !l.Allow() {
				slog.Warn("rate limited", "ip", ip, "path", path, "limiter", "oauth_register")
				writeRateLimitResponse(w, s.rateLimiters.Register.config)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Only rate-limit API endpoints below this point — the rest
		// of the OAuth surface + the SPA static files don't ride
		// the /api/* path.
		if !strings.HasPrefix(path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		// Auth-specific rate limits
		if strings.HasPrefix(path, "/api/v1/auth/") {
			var limiter *ipRateLimiter
			switch {
			case path == "/api/v1/auth/login" || path == "/api/v1/auth/bootstrap" || path == "/api/v1/auth/2fa/login-verify":
				limiter = s.rateLimiters.Auth
			case path == "/api/v1/auth/forgot-password" || path == "/api/v1/auth/reset-password" || path == "/api/v1/auth/local-reset" ||
				path == "/api/v1/auth/verify-email" || path == "/api/v1/auth/resend-verification":
				// Email-verification endpoints (PLAN-1933 DR-5) reuse the
				// PasswordReset bucket — same low-frequency, enumeration-safe
				// shape as forgot/reset-password. Without an entry here they'd
				// fall through to the looser default API limiter.
				limiter = s.rateLimiters.PasswordReset
			case path == "/api/v1/auth/register":
				limiter = s.rateLimiters.Register
			case path == "/api/v1/auth/oauth-login" || path == "/api/v1/auth/oauth-link":
				limiter = s.rateLimiters.OAuthLogin
			case path == "/api/v1/auth/oauth-unlink":
				limiter = s.rateLimiters.Auth // Same as login — 5/min, user-initiated
			default:
				// Other auth endpoints (session check, logout) — use general API limit
				limiter = s.rateLimiters.API
			}

			if limiter != nil {
				l := limiter.getLimiter(ip)
				if !l.Allow() {
					slog.Warn("rate limited", "ip", ip, "path", path, "limiter", "auth")
					writeRateLimitResponse(w, limiter.config)
					return
				}
			}
			next.ServeHTTP(w, r)
			return
		}

		// Cloud admin endpoints (sidecar → pad): plan changes, Stripe mapping, user lookup
		if strings.HasPrefix(path, "/api/v1/admin/") {
			switch path {
			case "/api/v1/admin/plan", "/api/v1/admin/stripe-customer-id", "/api/v1/admin/user-by-customer", "/api/v1/admin/stripe-event-processed", "/api/v1/admin/stripe-event-unmark", "/api/v1/admin/payment-failed":
				l := s.rateLimiters.CloudAdmin.getLimiter(ip)
				if !l.Allow() {
					slog.Warn("rate limited", "ip", ip, "path", path, "limiter", "cloud_admin")
					writeRateLimitResponse(w, s.rateLimiters.CloudAdmin.config)
					return
				}
			}
			// Other admin endpoints fall through to general API limit below
		}

		// Invitation preview (BUG-1934): public, pre-auth,
		// GET /api/v1/invitations/{code}/preview. Rate-limit per IP on a
		// dedicated strict bucket so it can't be used to enumerate invite
		// codes — the endpoint is always-200 so the status can't leak
		// validity, making the rate cap the primary volume defense. Matches
		// only the trailing /preview segment; /invitations/{code}/accept is
		// authenticated and falls through to the general API limit.
		if strings.HasPrefix(path, "/api/v1/invitations/") && strings.HasSuffix(path, "/preview") {
			if !s.rateLimiters.InvitationPreview.getLimiter(ip).Allow() {
				slog.Warn("rate limited", "ip", ip, "path", path, "limiter", "invitation_preview")
				writeRateLimitResponse(w, s.rateLimiters.InvitationPreview.config)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Search endpoint
		if path == "/api/v1/search" {
			key := rateLimitKey(r, ip)
			if !s.rateLimiters.Search.getLimiter(key).Allow() {
				slog.Warn("rate limited", "key", key, "path", path, "limiter", "search")
				writeRateLimitResponse(w, s.rateLimiters.Search.config)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// General API rate limit
		key := rateLimitKey(r, ip)
		if !s.rateLimiters.API.getLimiter(key).Allow() {
			slog.Warn("rate limited", "key", key, "path", path, "limiter", "api")
			writeRateLimitResponse(w, s.rateLimiters.API.config)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// rateLimitKey returns a key for rate limiting: user ID if authenticated, IP otherwise.
func rateLimitKey(r *http.Request, ip string) string {
	if user := currentUser(r); user != nil {
		return "user:" + user.ID
	}
	return "ip:" + ip
}

// clientIP extracts the client IP from RemoteAddr. This is safe because
// TrustedProxyRealIP runs earlier in the chain and — when a trusted
// proxy is configured — overwrites RemoteAddr with the trusted value
// from X-Real-IP / X-Forwarded-For. We deliberately do NOT read proxy
// headers here to prevent clients from spoofing their IP to bypass
// rate limits.
//
// Uses net.SplitHostPort so IPv6 addresses are handled correctly.
// A naive LastIndex(":") strips the final hextet of a bare IPv6 address
// like "2001:db8::1" — TrustedProxyRealIP writes the X-Forwarded-For
// value verbatim (no port, no brackets), so a LastIndex-based parse
// would mangle it. For bare IPs without a port SplitHostPort returns
// an error and we return the address as-is.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// checkMCPRateLimit applies the per-token bucket on /mcp requests
// (PLAN-943 TASK-959). Returns true if the request is allowed
// through; false if rate-limited (in which case the 429 response
// has already been written and the caller MUST return immediately).
//
// bearer is the raw Authorization Bearer value extracted by
// extractBearer. We hash it with SHA-256 before using it as a
// limiter map key so the raw token never lives in the limiter's
// memory across requests.
//
// Behaviour:
//
//   - Empty bearer (caller bug; the auth path should have rejected
//     before reaching here) → allow through to keep the limiter
//     from masking a real bug.
//   - Limiters not initialized (testServer with no NewRateLimiters
//     call) → allow through.
//   - Bucket exhausted → 429 with Retry-After + MCP error envelope.
//
// Per-token (not per-IP / per-user) keying matches the task spec:
// office-NAT'd users don't share a quota, and a runaway agent on
// one token can't burn the user's quota for other tokens.
func (s *Server) checkMCPRateLimit(w http.ResponseWriter, r *http.Request, bearer string) bool {
	if s.rateLimiters == nil || s.rateLimiters.MCPPerToken == nil || bearer == "" {
		return true
	}
	key := hashTokenForLimiter(bearer)
	l := s.rateLimiters.MCPPerToken.getLimiter(key)
	if !l.Allow() {
		slog.Warn("mcp rate limited", "path", r.URL.Path, "limiter", "mcp_per_token")
		writeMCPRateLimit(w, r, s.rateLimiters.MCPPerToken.config)
		return false
	}
	return true
}

// hashTokenForLimiter returns a SHA-256 hex digest of the bearer
// token, suitable for use as a rate-limiter map key. The hash means
// the limiter's in-memory map never holds the raw token even though
// it persists for the bucket's retention window. Hex (not base64)
// because the limiter's other keys are IP strings and a uniform
// hex encoding makes log scrapers' life easier.
func hashTokenForLimiter(bearer string) string {
	sum := sha256.Sum256([]byte(bearer))
	return hex.EncodeToString(sum[:])
}

// writeMCPRateLimit emits a 429 response with the MCP-shaped JSON
// envelope plus the standard rate-limit headers. Mirrors
// writeRateLimitResponse's headers but uses the MCP error envelope
// instead of the API one — MCP clients (Claude Desktop, Cursor, …)
// expect `{error: {code, message}}` and the standard envelope's
// `{error: {...}}` happens to match, but emitting via the MCP path
// keeps the contract clearer if either side ever diverges.
//
// Retry-After is computed from the limiter's refill rate (the same
// math writeRateLimitResponse uses) so a client doing exponential
// backoff hits a sane window.
func writeMCPRateLimit(w http.ResponseWriter, _ *http.Request, cfg rateLimitConfig) {
	retryAfter := int(math.Ceil(1.0 / float64(cfg.Rate)))
	if retryAfter < 1 {
		retryAfter = 1
	}
	if retryAfter > 3600 {
		retryAfter = 3600
	}
	limitPerMinute := int(math.Ceil(float64(cfg.Rate) * 60))

	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limitPerMinute))
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    "rate_limited",
			"message": "Too many requests. Please try again later.",
		},
	})
}

// writeRateLimitResponse sends a 429 response with Retry-After and X-RateLimit-* headers.
func writeRateLimitResponse(w http.ResponseWriter, cfg rateLimitConfig) {
	// Calculate retry-after from the rate (seconds until one token is available)
	retryAfter := int(math.Ceil(1.0 / float64(cfg.Rate)))
	if retryAfter < 1 {
		retryAfter = 1
	}
	if retryAfter > 3600 {
		retryAfter = 3600
	}

	// Calculate requests per minute for the limit header
	limitPerMinute := int(math.Ceil(float64(cfg.Rate) * 60))

	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limitPerMinute))
	w.Header().Set("X-RateLimit-Remaining", "0")
	writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Please try again later.")
}
