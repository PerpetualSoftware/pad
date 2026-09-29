package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/PerpetualSoftware/pad/internal/metrics"
)

// PLAN-2310 DR-9 acceptance: the MCP/OAuth security floor.

func floorRequest(srv *Server, method, path, body, contentType, remote, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = remote
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// burstThenRefused sends burst requests that must not be 429, then one
// that must be, all from the same address inside one second.
func burstThenRefused(t *testing.T, name string, burst int, send func() *httptest.ResponseRecorder) {
	t.Helper()
	for i := 1; i <= burst; i++ {
		if rr := send(); rr.Code == http.StatusTooManyRequests {
			t.Fatalf("%s: request %d of the burst %d was refused", name, i, burst)
		}
	}
	if rr := send(); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("%s: request %d: status %d, want 429", name, burst+1, rr.Code)
	}
}

func TestDR9_BurstPassesAndBurstPlusOneIsRefused(t *testing.T) {
	t.Run("pre-auth /mcp 401", func(t *testing.T) {
		srv := mcpEnabledTestServer(t)
		burstThenRefused(t, "/mcp no token", 120, func() *httptest.ResponseRecorder {
			return floorRequest(srv, "POST", "/mcp", `{}`, "application/json", "192.0.2.21:1", "")
		})
		// Another address has its own bucket.
		if rr := floorRequest(srv, "POST", "/mcp", `{}`, "application/json", "192.0.2.22:1", ""); rr.Code != http.StatusUnauthorized {
			t.Fatalf("another address: status %d, want 401", rr.Code)
		}
	})
	t.Run("/oauth/token", func(t *testing.T) {
		srv, _ := oauthEnabledTestServer(t)
		burstThenRefused(t, "/oauth/token", 120, func() *httptest.ResponseRecorder {
			return floorRequest(srv, "POST", "/oauth/token", "grant_type=authorization_code", "application/x-www-form-urlencoded", "192.0.2.23:1", "")
		})
	})
	t.Run("/oauth/authorize/decide", func(t *testing.T) {
		srv, _ := oauthEnabledTestServer(t)
		burstThenRefused(t, "/oauth/authorize/decide", 20, func() *httptest.ResponseRecorder {
			return floorRequest(srv, "POST", "/oauth/authorize/decide", "decision=approve", "application/x-www-form-urlencoded", "192.0.2.24:1", "")
		})
	})
	t.Run("/oauth/register", func(t *testing.T) {
		srv, _ := oauthEnabledTestServer(t)
		burstThenRefused(t, "/oauth/register", 5, func() *httptest.ResponseRecorder {
			return floorRequest(srv, "POST", "/oauth/register", `{}`, "application/json", "192.0.2.25:1", "")
		})
	})
}

// The claim limit is per authenticated caller: its burst of 10 is shared
// across two client addresses, not doubled by rotating them.
func TestDR9_ClaimLimitFollowsTheCaller(t *testing.T) {
	env := newClaimTestEnv(t)
	send := func(remote string) *httptest.ResponseRecorder {
		return floorRequest(env.srv, "POST", "/api/v1/oauth/claim", `{}`, "application/json", remote, env.pat)
	}
	for i := 1; i <= 10; i++ {
		remote := "192.0.2.31:1"
		if i%2 == 0 {
			remote = "198.51.100.31:1"
		}
		if rr := send(remote); rr.Code == http.StatusTooManyRequests {
			t.Fatalf("claim %d of the burst was refused", i)
		}
	}
	for _, remote := range []string{"192.0.2.31:1", "198.51.100.31:1", "203.0.113.31:1"} {
		if rr := send(remote); rr.Code != http.StatusTooManyRequests {
			t.Errorf("claim 11 from %s: status %d, want 429 (the bucket is the caller's)", remote, rr.Code)
		}
	}
}

// DCR and account signup no longer share a bucket: exhausting either
// leaves the other untouched.
func TestDR9_DCRAndSignupBucketsAreIndependent(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	const remote = "192.0.2.41:1"
	for i := 0; i < 5; i++ {
		floorRequest(srv, "POST", "/oauth/register", `{}`, "application/json", remote, "")
	}
	if rr := floorRequest(srv, "POST", "/oauth/register", `{}`, "application/json", remote, ""); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("DCR not exhausted (status %d); the control is void", rr.Code)
	}
	if rr := floorRequest(srv, "POST", "/api/v1/auth/register", `{}`, "application/json", remote, ""); rr.Code == http.StatusTooManyRequests {
		t.Errorf("signup refused 429 after DCR was exhausted: the buckets are shared")
	}

	srv2, _ := oauthEnabledTestServer(t)
	for i := 0; i < 6; i++ {
		floorRequest(srv2, "POST", "/api/v1/auth/register", `{}`, "application/json", remote, "")
	}
	if rr := floorRequest(srv2, "POST", "/api/v1/auth/register", `{}`, "application/json", remote, ""); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("signup not exhausted (status %d); the control is void", rr.Code)
	}
	if rr := floorRequest(srv2, "POST", "/oauth/register", `{}`, "application/json", remote, ""); rr.Code == http.StatusTooManyRequests {
		t.Errorf("DCR refused 429 after signup was exhausted: the buckets are shared")
	}
}

// A 100-client schedule against /oauth/token's configured bucket, on a
// simulated clock: 100 code exchanges in one second, then 100 refreshes
// spread over each hour for three hours. None may be refused.
func TestDR9_TokenBucketFitsA100ClientSchedule(t *testing.T) {
	rls := NewRateLimiters()
	defer rls.Stop()
	cfg := rls.OAuthToken.config
	l := rate.NewLimiter(cfg.Rate, cfg.Burst)
	t0 := time.Unix(0, 0)
	for i := 0; i < 100; i++ {
		if !l.AllowN(t0.Add(time.Duration(i)*10*time.Millisecond), 1) {
			t.Fatalf("code exchange %d of 100 in the first second was refused", i+1)
		}
	}
	for hour := 0; hour < 3; hour++ {
		for i := 0; i < 100; i++ {
			at := t0.Add(time.Second + time.Duration(hour)*time.Hour + time.Duration(i)*36*time.Second)
			if !l.AllowN(at, 1) {
				t.Fatalf("refresh %d in hour %d was refused", i+1, hour+1)
			}
		}
	}
}

// Every pre-auth /mcp refusal is counted by reason, and none writes an
// audit row (the DR-9 amendment: unauthenticated traffic causes no DB
// writes). The rate-limited refusal is counted under its own reason.
func TestDR9_PreAuthDenialsAreCountedNotAudited(t *testing.T) {
	srv := mcpEnabledTestServer(t)
	srv.metrics = metrics.New()
	floorRequest(srv, "POST", "/mcp", `{}`, "application/json", "192.0.2.51:1", "")
	floorRequest(srv, "POST", "/mcp", `{}`, "application/json", "192.0.2.51:1", "not-a-pad-token")
	floorRequest(srv, "POST", "/mcp", `{}`, "application/json", "192.0.2.51:1", "pad_"+strings.Repeat("0", 64))
	if got := counterValue(t, srv.metrics.MCPPreAuthDeniedTotal.WithLabelValues("missing_token")); got != 1 {
		t.Errorf("missing_token = %v, want 1", got)
	}
	if got := counterValue(t, srv.metrics.MCPPreAuthDeniedTotal.WithLabelValues("invalid_token")); got != 2 {
		t.Errorf("invalid_token = %v, want 2", got)
	}
	for i := 0; i < 120; i++ {
		floorRequest(srv, "POST", "/mcp", `{}`, "application/json", "192.0.2.52:1", "")
	}
	if rr := floorRequest(srv, "POST", "/mcp", `{}`, "application/json", "192.0.2.52:1", ""); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("121st pre-auth refusal: status %d, want 429", rr.Code)
	}
	if got := counterValue(t, srv.metrics.MCPPreAuthDeniedTotal.WithLabelValues("rate_limited")); got != 1 {
		t.Errorf("rate_limited = %v, want 1", got)
	}
	time.Sleep(200 * time.Millisecond) // the audit writer is asynchronous
	rows, err := srv.store.ListAllMCPAudit(10, 0)
	if err != nil {
		t.Fatalf("ListMCPAudit: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("pre-auth refusals wrote %d audit rows, want 0", len(rows))
	}
}

// On cloud the limits key on the forwarded client address, resolved
// through PAD_TRUSTED_PROXIES (pad-cloud sets 172.28.0.0/16). If that
// resolution failed, every request would share the proxy's address and
// these limits would throttle all of cloud.
func TestDR9_CloudKeysOnTheForwardedAddress(t *testing.T) {
	srv := mcpEnabledTestServer(t)
	srv.SetTrustedProxies("172.28.0.0/16")
	send := func(xff string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
		req.RemoteAddr = "172.28.0.5:40000"
		req.Header.Set("X-Forwarded-For", xff)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}
	for i := 0; i < 120; i++ {
		send("203.0.113.9")
	}
	if rr := send("203.0.113.9"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("client 203.0.113.9 after its burst: status %d, want 429", rr.Code)
	}
	if rr := send("198.51.100.7"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("another client behind the same proxy: status %d, want 401 (own bucket)", rr.Code)
	}
}

// A proxied request that resolves to an address inside the trusted range
// (the proxy forwarded no client address) is warned about once, loudly;
// a proxy that forwards the client address is not.
func TestDR9_WarnsWhenResolutionStaysInsideTheTrustedRange(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	warnResolvedInTrustedRange = sync.Once{}

	mw := TrustedProxyRealIP(ParseTrustedProxyCIDRs("172.28.0.0/16"))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	serve := func(xff string) {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "172.28.0.5:40000"
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		mw.ServeHTTP(httptest.NewRecorder(), req)
	}
	serve("203.0.113.9")
	if strings.Contains(buf.String(), "PAD_TRUSTED_PROXIES") {
		t.Fatalf("warned for a correctly forwarded client address: %s", buf.String())
	}
	serve("")
	serve("")
	if n := strings.Count(buf.String(), "inside PAD_TRUSTED_PROXIES"); n != 1 {
		t.Fatalf("resolution inside the trusted range twice: %d warnings, want 1", n)
	}
}

// No zero-user bypass: on a fresh install with no users, /mcp stays
// fail-closed, although the regular API falls open at zero users.
func TestDR9_ZeroUserMCPIsUnauthorized(t *testing.T) {
	srv := mcpEnabledTestServer(t)
	if n, err := srv.store.UserCount(); err != nil || n != 0 {
		t.Fatalf("fixture must have no users: n=%d err=%v", n, err)
	}
	for _, bearer := range []string{"", "pad_" + strings.Repeat("a", 64), "opaque-oauth-looking-token"} {
		rr := floorRequest(srv, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, "application/json", "192.0.2.61:1", bearer)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("zero users, bearer %q: status %d, want 401", bearer, rr.Code)
		}
	}
}
