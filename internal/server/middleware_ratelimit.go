package server

import (
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
	}
	// Background cleanup of stale entries every 5 minutes
	go rl.cleanup()
	return rl
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
	for {
		time.Sleep(5 * time.Minute)
		rl.mu.Lock()
		for key, entry := range rl.limiters {
			if time.Since(entry.lastSeen) > rl.retention {
				delete(rl.limiters, key)
			}
		}
		rl.mu.Unlock()
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
	// RecoveryCode caps how many recovery codes can be tried against a
	// single 2FA challenge token. Without it an attacker who captures a
	// valid challenge_token can grind through the small recovery-code
	// space before the 5-minute challenge expires.
	RecoveryCode *ipRateLimiter
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

		// Only rate-limit API endpoints
		if !strings.HasPrefix(path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		ip := clientIP(r)

		// Auth-specific rate limits
		if strings.HasPrefix(path, "/api/v1/auth/") {
			var limiter *ipRateLimiter
			switch {
			case path == "/api/v1/auth/login" || path == "/api/v1/auth/bootstrap" || path == "/api/v1/auth/2fa/login-verify":
				limiter = s.rateLimiters.Auth
			case path == "/api/v1/auth/forgot-password" || path == "/api/v1/auth/reset-password":
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
			case "/api/v1/admin/plan", "/api/v1/admin/stripe-customer-id", "/api/v1/admin/user-by-customer", "/api/v1/admin/stripe-event-processed", "/api/v1/admin/stripe-event-unmark":
				l := s.rateLimiters.CloudAdmin.getLimiter(ip)
				if !l.Allow() {
					slog.Warn("rate limited", "ip", ip, "path", path, "limiter", "cloud_admin")
					writeRateLimitResponse(w, s.rateLimiters.CloudAdmin.config)
					return
				}
			}
			// Other admin endpoints fall through to general API limit below
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

// writeTooManyRequests sends a basic 429 response (for backward compatibility in tests).
func writeTooManyRequests(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(60))
	writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Please try again later.")
}
