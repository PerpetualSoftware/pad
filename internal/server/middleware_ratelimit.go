package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
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
	// MaxEntries caps how many keys the limiter holds at once. Zero means
	// defaultMaxEntries. See evictLocked for what happens at the cap.
	MaxEntries int
}

// defaultRetention is the minimum retention for a limiter whose config
// doesn't specify one. Suitable for sub-minute windows like per-IP login
// limiting; longer windows must set Retention explicitly.
const defaultRetention = 30 * time.Minute

// defaultMaxEntries bounds one limiter's map (BUG-3308). Retention alone
// bounds it only by how many distinct keys arrive inside the retention
// window, which a caller controlling many source addresses (or, for
// AuthEmail, typing many email strings) sets. Measured at 170-192 bytes a
// key (BUG-3308's trail), so this is about 12 MiB a limiter at the cap.
// Reaching it never refuses anyone: it evicts, see evictLocked.
const defaultMaxEntries = 1 << 16

// evictLevels is how finely evictLocked ranks buckets by their tokens.
const evictLevels = 1024

// ipRateLimiter tracks per-key rate limiters with automatic cleanup.
type ipRateLimiter struct {
	mu         sync.Mutex
	limiters   map[string]*rateLimiterEntry
	config     rateLimitConfig
	retention  time.Duration
	maxEntries int

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
	maxEntries := cfg.MaxEntries
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}
	rl := &ipRateLimiter{
		limiters:   make(map[string]*rateLimiterEntry),
		config:     cfg,
		retention:  retention,
		maxEntries: maxEntries,
		stopCh:     make(chan struct{}),
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

// allow charges one token from key's bucket and reports whether the
// request may proceed. The lookup and the charge happen under one lock
// hold: a caller that took the pointer and charged it later could spend a
// bucket the sweep had already evicted and replaced (BUG-3308, codex
// round 1), getting both the old bucket's tokens and the new one's burst.
// Every production charge goes through here.
func (rl *ipRateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.limiterLocked(key).Allow()
}

// getLimiter returns key's bucket, creating it if absent, without
// charging it. Tests read a bucket through it; production code charges
// through allow instead.
func (rl *ipRateLimiter) getLimiter(key string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.limiterLocked(key)
}

// limiterLocked is the lookup behind allow and getLimiter. Caller holds
// rl.mu.
func (rl *ipRateLimiter) limiterLocked(key string) *rate.Limiter {
	now := time.Now()
	entry, exists := rl.limiters[key]
	if !exists {
		if len(rl.limiters) >= rl.maxEntries {
			rl.evictLocked(now)
		}
		limiter := rate.NewLimiter(rl.config.Rate, rl.config.Burst)
		rl.limiters[key] = &rateLimiterEntry{
			limiter:  limiter,
			lastSeen: now,
		}
		return limiter
	}
	entry.lastSeen = now
	return entry.limiter
}

// evictLocked shrinks a full map to its low-water mark (7/8 of the cap),
// so the sweep runs once per cap/8 new keys rather than on every one.
// Caller holds rl.mu.
//
// Evicting a key hands its owner a fresh bucket on their next request,
// so the order is chosen by what that gift is worth (BUG-3308):
//
//  1. Keys idle past retention, which cleanup would drop anyway, and
//     keys whose bucket has refilled to burst. A full bucket is
//     indistinguishable from a new one, so dropping it gives nothing.
//  2. If that is not enough, the fullest buckets first: the owner gains
//     burst minus the tokens they already had, the smallest gift on
//     offer.
//
// What that buys: step 2 evicts a bucket only when at least the
// low-water mark's worth of OTHER buckets (57,344 at the default cap)
// hold no more tokens than it does at that moment, to within
// burst/evictLevels. So to get a drained bucket evicted, an attacker has
// to hold that many other buckets drained as far at once. Where they can
// mint keys (any address-keyed bucket) a fresh key is cheaper than that;
// where they cannot (AuthEmail's sprayed address, a share link's
// link-wide bucket), it is what the reset costs.
//
// The sweep holds rl.mu, so it delays every request on this limiter for
// its length (about 18 ms at the default cap, BUG-3308's trail), once per
// cap/8 new keys.
func (rl *ipRateLimiter) evictLocked(now time.Time) {
	low := rl.maxEntries - rl.maxEntries/8
	if low >= rl.maxEntries {
		low = rl.maxEntries - 1 // a cap under 8 still has to make room
	}
	burst := float64(rl.config.Burst)
	// A bucket that is not full holds between 0 and burst tokens; its
	// level is that fraction of burst in evictLevels steps. Levels rather
	// than a sort keep the sweep linear, and a fixed count keeps the
	// histogram's size off the config: two buckets share a level only
	// when they differ by under burst/evictLevels tokens (codex round 1:
	// whole-token levels put a drained bucket beside ones at 0.99).
	type candidate struct {
		key   string
		level int
	}
	rest := make([]candidate, 0, len(rl.limiters))
	var perLevel [evictLevels]int
	for key, entry := range rl.limiters {
		tokens := entry.limiter.TokensAt(now)
		if now.Sub(entry.lastSeen) > rl.retention || tokens >= burst {
			delete(rl.limiters, key)
			continue
		}
		// burst > tokens >= 0 here, so burst is positive and the level
		// is in [0, evictLevels).
		level := int(math.Max(tokens, 0) / burst * evictLevels)
		if level >= evictLevels {
			level = evictLevels - 1
		}
		rest = append(rest, candidate{key, level})
		perLevel[level]++
	}
	excess := len(rl.limiters) - low
	if excess <= 0 {
		return
	}
	// Fullest first: every bucket above the cut level goes, and as many
	// on the cut level as are still needed.
	cut, above := len(perLevel)-1, 0
	for ; cut > 0 && above+perLevel[cut] < excess; cut-- {
		above += perLevel[cut]
	}
	onCut := excess - above
	for _, c := range rest {
		switch {
		case c.level > cut:
			delete(rl.limiters, c.key)
		case c.level == cut && onCut > 0:
			delete(rl.limiters, c.key)
			onCut--
		}
	}
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
	// Per address (not link-wide) so one caller exhausting their own bucket
	// can't lock every legitimate viewer out, only those sharing their
	// address: an IPv4 address, or an IPv6 /64 (BUG-3308), which is the
	// IPv6 counterpart of one NATed IPv4 address. See handleResolveShareLink.
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
	// DecisionProvider caps SYNCHRONOUS typed-decision provider calls per
	// user (IP when anonymous): every one costs the instance admin money,
	// and the endpoints that make them (today POST /playbooks/match) need
	// only viewer access, so the general API bucket's 600/min let a single
	// viewer spend 600 calls a minute (TASK-3141). It is charged in the
	// handler, immediately before the provider call, through
	// allowDecisionProviderCall, so a request answered by an earlier check
	// consumes no token. A refusal raised INSIDE the provider call (the
	// decision package's pre-send ErrRequestTooLarge) has already been
	// charged. The async decision_jobs runner is not charged here; it has
	// its own rail.
	DecisionProvider *ipRateLimiter
	// CollabDial caps WebSocket dials to /api/v1/collab/{itemID} per user (IP
	// when anonymous), in place of the general API bucket (BUG-1308). A dial
	// used to spend the same token as a REST call, so a user's own reconnects
	// and page loads competed for one burst of 60: 100 dials from one user got
	// 60 through and 40 refused, and a server bounce re-dials every open
	// socket inside ~0.3s. Sized from that measurement (BUG-1308 checkpoint 2):
	// burst 50 covers every socket a heavy user re-dials at once (20 at 20
	// tabs, 2.5x headroom), 5/s covers a flapping network on the 1-2-4s
	// backoff. How many sockets a user may HOLD is the separate collab
	// admission gate (PAD_COLLAB_MAX_PER_USER).
	CollabDial *ipRateLimiter

	// The MCP and OAuth security floor (PLAN-2310 DR-9). Starting values,
	// reasoned rather than measured; revise them from the audit data DR-9
	// adds, not by guesswork. Each is keyed per client address (the
	// address TrustedProxyRealIP resolves) except OAuthClaim, which is per
	// authenticated caller. They apply on cloud too: one code path.

	// MCPPreAuth caps /mcp 401s (missing, malformed or invalid bearer) per
	// address: 1/s, burst 120. An MCP client's first unauthenticated
	// request draws one 401 (that is how it discovers resource_metadata),
	// and so does each reconnect. 100 agents behind one office NAT
	// restarting in the same second fit the burst; sustained, a
	// second-by-second restart loop of one client never exceeds 1/s. A
	// valid token never draws from it. Deliberately per address and not
	// global: a global cap lets one abuser lock every client out of a
	// small deployment.
	MCPPreAuth *ipRateLimiter
	// OAuthToken caps /oauth/token per address: 1/s, burst 120. A client
	// calls it once per code exchange when it connects, then to refresh.
	// Access tokens live 1h (internal/oauth/server.go), so 100 clients
	// refresh about 100 times an hour against a refill of 3,600 an hour,
	// 36x headroom, and the burst covers 100 clients connecting at once.
	OAuthToken *ipRateLimiter
	// OAuthDecide caps /oauth/authorize/decide per address: 10/min, burst
	// 20. It is a human clicking consent; 20 in a row is already abnormal.
	OAuthDecide *ipRateLimiter
	// OAuthRegister caps /oauth/register (RFC 7591 dynamic client
	// registration) per address: 5/hour, burst 5. The rate is what DCR
	// always had, but the bucket is its own: it used to share Register
	// with account signup, so five client registrations locked the
	// address out of signing up and the reverse.
	OAuthRegister *ipRateLimiter
	// OAuthClaim caps POST /api/v1/oauth/claim per AUTHENTICATED CALLER:
	// 10/min, burst 10. Keyed by user, not address, because the route
	// requires auth and rotating addresses must not raise the cap. A code
	// is 6 digits and verifies against the current and previous 300 s
	// bucket (claim_codes.go), so it stays guessable for about 600 s; in
	// that window this allows at most 10 + 100 = 110 guesses against 10^6,
	// about 1.1e-4 per code window. A human redeeming a code they were
	// shown needs one attempt, perhaps two.
	OAuthClaim *ipRateLimiter
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
		// Collab WebSocket dials: 5 per second per user/IP, burst 50. See
		// the CollabDial field for the measurement behind both numbers.
		CollabDial: newIPRateLimiter(rateLimitConfig{
			Rate:  rate.Limit(5),
			Burst: 50,
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
		// Decision provider: 30 per minute per user/IP, burst 5 (TASK-3141).
		// Deliberately not an env knob: no bucket here has one (the only
		// rate-limit env is PAD_DISABLE_RATE_LIMITS, for E2E), and if this
		// ever needs tuning its home is the instance-admin decision-provider
		// setting (TASK-3121), not the environment.
		DecisionProvider: newIPRateLimiter(rateLimitConfig{
			Rate:  rate.Limit(30.0 / 60.0),
			Burst: 5,
		}),
		// PLAN-2310 DR-9; the rationale for each is on its field.
		MCPPreAuth: newIPRateLimiter(rateLimitConfig{
			Rate:  rate.Limit(1.0),
			Burst: 120,
		}),
		OAuthToken: newIPRateLimiter(rateLimitConfig{
			Rate:  rate.Limit(1.0),
			Burst: 120,
		}),
		OAuthDecide: newIPRateLimiter(rateLimitConfig{
			Rate:  rate.Limit(10.0 / 60.0),
			Burst: 20,
		}),
		// Retention ≥ the refill window (5 ÷ (5/hour) = 1 h), or cleanup
		// could evict a bucket between registrations; 2 h gives margin.
		OAuthRegister: newIPRateLimiter(rateLimitConfig{
			Rate:      rate.Limit(5.0 / 3600.0),
			Burst:     5,
			Retention: 2 * time.Hour,
		}),
		OAuthClaim: newIPRateLimiter(rateLimitConfig{
			Rate:  rate.Limit(10.0 / 60.0),
			Burst: 10,
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
		rls.DecisionProvider,
		rls.CollabDial,
		rls.MCPPreAuth,
		rls.OAuthToken,
		rls.OAuthDecide,
		rls.OAuthRegister,
		rls.OAuthClaim,
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
		addr := rateLimitAddr(ip) // the bucket key; ip stays whole for the log lines

		// The OAuth flow endpoints (PLAN-2310 DR-9). /oauth/register is
		// open by RFC 7591 design, /oauth/token is PKCE-bound and
		// /oauth/authorize/decide is a human clicking consent; each has
		// its own per-address bucket (rationale on the RateLimiters
		// fields). /oauth/authorize, /revoke and /introspect are not
		// limited here.
		var oauthLimiter *ipRateLimiter
		var oauthLabel string
		switch path {
		case "/oauth/register":
			oauthLimiter, oauthLabel = s.rateLimiters.OAuthRegister, "oauth_register"
		case "/oauth/token":
			oauthLimiter, oauthLabel = s.rateLimiters.OAuthToken, "oauth_token"
		case "/oauth/authorize/decide":
			oauthLimiter, oauthLabel = s.rateLimiters.OAuthDecide, "oauth_decide"
		}
		if oauthLimiter != nil {
			if !oauthLimiter.allow(addr) {
				slog.Warn("rate limited", "ip", ip, "path", path, "limiter", oauthLabel)
				writeRateLimitResponse(w, oauthLimiter.config)
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
				path == "/api/v1/auth/verify-email" || path == "/api/v1/auth/verify-email/claim" || path == "/api/v1/auth/resend-verification":
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
			// The auth buckets are per address. The general API bucket is
			// keyed as the general arm below keys it (user, else address):
			// charging it under the bare address here gave an anonymous
			// caller a second API bucket, and a signed-in one an address
			// bucket beside their user bucket (BUG-3310).
			key := addr
			if limiter == s.rateLimiters.API {
				key = rateLimitKey(r, addr)
			}

			if limiter != nil {
				if !limiter.allow(key) {
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
				if !s.rateLimiters.CloudAdmin.allow(addr) {
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
			if !s.rateLimiters.InvitationPreview.allow(addr) {
				slog.Warn("rate limited", "ip", ip, "path", path, "limiter", "invitation_preview")
				writeRateLimitResponse(w, s.rateLimiters.InvitationPreview.config)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Search endpoint
		// Collab WebSocket dials draw on their own bucket, never the general
		// API one (BUG-1308): a socket dial and a REST call are different
		// units, and sharing one burst let a user's own reconnects refuse
		// their page loads and the reverse. Only an actual upgrade request
		// counts as a dial; anything else under the prefix (a plain GET, a
		// POST, a future REST route) is an ordinary API request and pays the
		// API bucket (codex round 1).
		if strings.HasPrefix(path, "/api/v1/collab/") && websocket.IsWebSocketUpgrade(r) {
			key := rateLimitKey(r, addr)
			if !s.rateLimiters.CollabDial.allow(key) {
				slog.Warn("rate limited", "key", key, "path", path, "limiter", "collab_dial")
				writeRateLimitResponse(w, s.rateLimiters.CollabDial.config)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Claim-code redemption (PLAN-2310 DR-9): per authenticated
		// caller, in place of the general bucket, so rotating addresses
		// cannot raise the guess rate. TokenAuth and SessionAuth have run
		// by now; an unauthenticated request falls through to the
		// general bucket and RequireAuth answers it 401.
		if path == "/api/v1/oauth/claim" && r.Method == http.MethodPost {
			if user := currentUser(r); user != nil {
				key := "user:" + user.ID
				if !s.rateLimiters.OAuthClaim.allow(key) {
					slog.Warn("rate limited", "key", key, "path", path, "limiter", "oauth_claim")
					writeRateLimitResponse(w, s.rateLimiters.OAuthClaim.config)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
		}

		if path == "/api/v1/search" {
			key := rateLimitKey(r, addr)
			if !s.rateLimiters.Search.allow(key) {
				slog.Warn("rate limited", "key", key, "path", path, "limiter", "search")
				writeRateLimitResponse(w, s.rateLimiters.Search.config)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// General API rate limit
		key := rateLimitKey(r, addr)
		if !s.rateLimiters.API.allow(key) {
			slog.Warn("rate limited", "key", key, "path", path, "limiter", "api")
			writeRateLimitResponse(w, s.rateLimiters.API.config)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// allowDecisionProviderCall charges one token from the DecisionProvider
// bucket and reports whether the caller may make a synchronous provider call.
// On refusal it has already written the 429 (with Retry-After) and the
// handler must return. Call it IMMEDIATELY before the provider call, after
// every check that can answer without spending, so a request that would not
// spend never consumes a token. A nil limiter set (PAD_DISABLE_RATE_LIMITS,
// testServer) allows, like checkMCPRateLimit.
func (s *Server) allowDecisionProviderCall(w http.ResponseWriter, r *http.Request) bool {
	if s.rateLimiters == nil || s.rateLimiters.DecisionProvider == nil {
		return true
	}
	key := rateLimitKey(r, rateLimitAddr(clientIP(r)))
	if !s.rateLimiters.DecisionProvider.allow(key) {
		slog.Warn("rate limited", "key", key, "path", r.URL.Path, "limiter", "decision_provider")
		writeRateLimitResponse(w, s.rateLimiters.DecisionProvider.config)
		return false
	}
	return true
}

// rateLimitKey returns a key for rate limiting: user ID if authenticated,
// the address otherwise. addr is rateLimitAddr's form, not the raw client IP.
func rateLimitKey(r *http.Request, addr string) string {
	if user := currentUser(r); user != nil {
		return "user:" + user.ID
	}
	return "ip:" + addr
}

// rateLimitAddr is the part of a client address a per-address bucket is
// keyed on (BUG-3308): an IPv6 address counts as its /64, because a
// single subscriber is routinely handed a whole /64 (and often a /56 or
// /48), so per-/128 keying gave one host 2^64 fresh buckets. IPv4, and
// IPv6-mapped IPv4, key on the address itself. Anything that does not
// parse is keyed as given.
//
// Only rate limiting uses this. clientIP stays the full address for
// sessions, audit rows and the 2FA challenge binding.
func rateLimitAddr(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String()
	}
	prefix, err := addr.WithZone("").Prefix(64)
	if err != nil {
		return ip
	}
	return prefix.String()
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
	if !s.rateLimiters.MCPPerToken.allow(key) {
		slog.Warn("mcp rate limited", "path", r.URL.Path, "limiter", "mcp_per_token")
		writeMCPRateLimit(w, r, s.rateLimiters.MCPPerToken.config)
		return false
	}
	return true
}

// chargeMCPPreAuth draws one token from the per-address MCPPreAuth bucket
// for an /mcp refusal made before a caller was identified (PLAN-2310
// DR-9). It returns false when the bucket is empty, having written the
// 429 and counted it; the caller must return. A nil limiter set
// (PAD_DISABLE_RATE_LIMITS, testServer) allows.
func (s *Server) chargeMCPPreAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.rateLimiters == nil || s.rateLimiters.MCPPreAuth == nil {
		return true
	}
	ip := clientIP(r)
	addr := rateLimitAddr(ip)
	if s.rateLimiters.MCPPreAuth.allow(addr) {
		return true
	}
	s.recordMCPPreAuthDenied("rate_limited")
	// The counter carries the volume; this line names the address, once
	// per address and rate-limited overall, so an operator can find a
	// prober without enabling debug logging. No database write (the DR-9
	// amendment: unauthenticated traffic causes none).
	if s.mcpPreAuthLimited.allow(ip) {
		slog.Warn("mcp: an address exhausted the pre-auth limit on /mcp (1/s, burst 120) with missing or invalid tokens; answering 429",
			"ip", ip, "limiter", "mcp_pre_auth")
	}
	writeMCPRateLimit(w, r, s.rateLimiters.MCPPreAuth.config)
	return false
}

// recordMCPPreAuthDenied counts a pre-auth /mcp refusal. The reason label
// is a closed set, so a caller-supplied code can never mint a series.
func (s *Server) recordMCPPreAuthDenied(reason string) {
	if s.metrics == nil {
		return
	}
	switch reason {
	case "missing_token", "rate_limited":
	default:
		reason = "invalid_token"
	}
	s.metrics.MCPPreAuthDeniedTotal.WithLabelValues(reason).Inc()
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
