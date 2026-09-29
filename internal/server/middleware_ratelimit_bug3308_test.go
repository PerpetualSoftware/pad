package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3308: per-address buckets key an IPv6 client on its /64, and every
// limiter's map has a size cap whose eviction cannot hand an attacker back
// their own drained bucket.

func TestRateLimitAddr(t *testing.T) {
	cases := []struct{ in, want string }{
		{"192.0.2.7", "192.0.2.7"},
		{"::ffff:192.0.2.7", "192.0.2.7"},
		{"2001:db8:1:2::1", "2001:db8:1:2::/64"},
		{"2001:db8:1:2:ffff:ffff:ffff:ffff", "2001:db8:1:2::/64"},
		{"2001:db8:1:3::1", "2001:db8:1:3::/64"},
		{"fe80::1%eth0", "fe80::/64"},
		{"::1", "::/64"},
		{"not-an-address", "not-an-address"},
		{"", ""},
	}
	for _, c := range cases {
		if got := rateLimitAddr(c.in); got != c.want {
			t.Errorf("rateLimitAddr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The map never holds more than MaxEntries keys, however many distinct
// keys arrive inside the retention window. Caps under 8 are included:
// there the 7/8 low-water mark rounds to the cap itself.
func TestIPRateLimiter_CapBoundsTheMap(t *testing.T) {
	for _, maxEntries := range []int{1, 4, 64} {
		rl := newIPRateLimiter(rateLimitConfig{Rate: rate.Limit(1.0 / 3600), Burst: 5, MaxEntries: maxEntries})
		t.Cleanup(rl.Stop)
		for i := 0; i < 10_000; i++ {
			rl.getLimiter(fmt.Sprintf("ip:k%d", i)).Allow()
			if n := len(rl.limiters); n > maxEntries {
				t.Fatalf("after %d keys the map holds %d, over the cap of %d", i+1, n, maxEntries)
			}
		}
	}
}

// The attack the eviction order exists to refuse: drain your own bucket,
// then flood new keys until the sweep pushes it out and you get a fresh
// burst. A drained bucket is evicted last, so it survives every sweep the
// flood triggers and its owner stays refused.
func TestIPRateLimiter_FloodCannotEvictADrainedBucket(t *testing.T) {
	rl := newIPRateLimiter(rateLimitConfig{Rate: rate.Limit(1.0 / 3600), Burst: 5, MaxEntries: 16})
	t.Cleanup(rl.Stop)

	drained := rl.getLimiter("ip:attacker")
	for i := 0; i < 5; i++ {
		if !drained.Allow() {
			t.Fatalf("attempt %d inside the burst was refused", i+1)
		}
	}
	// Each flood key spends one token, as a request from a fresh address
	// does. 1,000 keys is 60+ sweeps at this cap.
	for i := 0; i < 1000; i++ {
		rl.getLimiter(fmt.Sprintf("ip:flood%d", i)).Allow()
	}
	if got := rl.getLimiter("ip:attacker"); got != drained {
		t.Fatal("the drained bucket was evicted; its owner now holds a fresh burst")
	}
	if drained.Allow() {
		t.Fatal("the drained bucket allowed a request")
	}
}

// Codex round 1: whole-token levels put a drained bucket on the same
// level as buckets holding 0.9 tokens, so the sweep chose among them at
// random. The ranking is finer than a token now, so the drained bucket
// is never the one evicted. 200 trials: at the old granularity it went
// in about one trial in eight.
func TestIPRateLimiter_SweepRanksBelowAToken(t *testing.T) {
	for trial := 0; trial < 200; trial++ {
		rl := newIPRateLimiter(rateLimitConfig{Rate: 1, Burst: 2, MaxEntries: 8})
		now := time.Now()
		rl.getLimiter("ip:attacker").AllowN(now, 2) // 0 tokens at now
		for i := 0; i < 7; i++ {
			// drained 0.9s earlier, so 0.9 tokens at now
			rl.getLimiter(fmt.Sprintf("ip:k%d", i)).AllowN(now.Add(-900*time.Millisecond), 2)
		}
		rl.mu.Lock()
		rl.evictLocked(now)
		_, kept := rl.limiters["ip:attacker"]
		rl.mu.Unlock()
		rl.Stop()
		if !kept {
			t.Fatalf("trial %d: the drained bucket was evicted ahead of buckets holding 0.9 tokens", trial)
		}
	}
}

// A burst of 0 or below has no level to rank by; the sweep must neither
// panic nor let the map grow.
func TestIPRateLimiter_NonPositiveBurstStaysBounded(t *testing.T) {
	for _, burst := range []int{0, -1, -5} {
		rl := newIPRateLimiter(rateLimitConfig{Rate: 1, Burst: burst, MaxEntries: 8})
		for i := 0; i < 100; i++ {
			rl.allow(fmt.Sprintf("ip:k%d", i))
		}
		if n := len(rl.limiters); n > 8 {
			t.Errorf("burst %d: map holds %d keys, over the cap of 8", burst, n)
		}
		rl.Stop()
	}
}

// Full buckets go first and all at once: dropping one gives nothing, since
// a fresh limiter starts full. A partly spent bucket survives a sweep that
// full ones can satisfy.
func TestIPRateLimiter_SweepDropsFullBucketsBeforeSpentOnes(t *testing.T) {
	rl := newIPRateLimiter(rateLimitConfig{Rate: rate.Limit(1.0 / 3600), Burst: 5, MaxEntries: 16})
	t.Cleanup(rl.Stop)

	spent := map[string]*rate.Limiter{}
	for i := 0; i < 8; i++ {
		key := fmt.Sprintf("ip:spent%d", i)
		l := rl.getLimiter(key)
		l.Allow()
		spent[key] = l
	}
	for i := 0; i < 8; i++ {
		rl.getLimiter(fmt.Sprintf("ip:full%d", i)) // never charged
	}
	rl.getLimiter("ip:new") // map is at 16: this sweeps

	// All eight full buckets are gone, not just the two the low-water
	// mark needed, so the next sweep is further off.
	if n := len(rl.limiters); n != 9 {
		t.Errorf("after the sweep the map holds %d keys, want 9 (8 spent + the new one)", n)
	}
	for key, l := range spent {
		if rl.limiters[key] == nil || rl.limiters[key].limiter != l {
			t.Errorf("%s (partly spent) was evicted while full buckets were available", key)
		}
	}
}

// Keys idle past retention are dropped by the sweep too, whatever their
// tokens, the same rule cleanup applies.
func TestIPRateLimiter_SweepDropsIdleKeys(t *testing.T) {
	rl := newIPRateLimiter(rateLimitConfig{Rate: rate.Limit(1.0 / 3600), Burst: 5, MaxEntries: 8, Retention: time.Minute})
	t.Cleanup(rl.Stop)
	for i := 0; i < 8; i++ {
		rl.getLimiter(fmt.Sprintf("ip:k%d", i)).Allow()
	}
	rl.mu.Lock()
	for _, e := range rl.limiters {
		e.lastSeen = e.lastSeen.Add(-2 * time.Minute)
	}
	rl.mu.Unlock()
	rl.getLimiter("ip:new")
	if n := len(rl.limiters); n != 1 {
		t.Errorf("after the sweep the map holds %d keys, want 1 (every old key was idle)", n)
	}
}

// Router-level binding (every address-keyed door): exhausting a bucket from
// one address in a /64 refuses a sibling address in the same /64, and does
// not refuse an address in the next /64. Driven through ServeHTTP so the
// test fails if any door keys on the raw address.
func TestRateLimit_IPv6KeysOnThe64AtEveryDoor(t *testing.T) {
	const (
		first   = "2001:db8:1:2::1"
		sibling = "2001:db8:1:2:ffff:ffff:ffff:fffe"
		other   = "2001:db8:1:3::1"
	)
	type door struct {
		name   string
		method string
		path   string
		body   string
		ws     bool // websocket upgrade headers
		ctype  string
		server func(t *testing.T) *Server // nil: testServer
	}
	oauthSrv := func(t *testing.T) *Server { s, _ := oauthEnabledTestServer(t); return s }
	form := "application/x-www-form-urlencoded"
	doors := []door{
		{name: "auth", method: http.MethodPost, path: "/api/v1/auth/login", body: `{"email":"x@test.com","password":"x"}`},
		{name: "password_reset", method: http.MethodPost, path: "/api/v1/auth/forgot-password", body: `{"email":"x@test.com"}`},
		{name: "register", method: http.MethodPost, path: "/api/v1/auth/register", body: `{}`},
		{name: "oauth_login", method: http.MethodPost, path: "/api/v1/auth/oauth-login", body: `{}`},
		{name: "auth_default_api", method: http.MethodGet, path: "/api/v1/auth/session"},
		{name: "cloud_admin", method: http.MethodPost, path: "/api/v1/admin/plan", body: `{}`},
		{name: "invitation_preview", method: http.MethodGet, path: "/api/v1/invitations/nope/preview"},
		{name: "collab_dial", method: http.MethodGet, path: "/api/v1/collab/nope", ws: true},
		{name: "search", method: http.MethodGet, path: "/api/v1/search?q=x"},
		{name: "api", method: http.MethodGet, path: "/api/v1/workspaces"},
		{name: "oauth_register", method: http.MethodPost, path: "/oauth/register", body: `{}`, server: oauthSrv},
		{name: "oauth_token", method: http.MethodPost, path: "/oauth/token", body: "grant_type=authorization_code", ctype: form, server: oauthSrv},
		{name: "oauth_decide", method: http.MethodPost, path: "/oauth/authorize/decide", body: "decision=approve", ctype: form, server: oauthSrv},
		{name: "mcp_pre_auth", method: http.MethodPost, path: "/mcp", body: `{}`, server: mcpEnabledTestServer},
	}
	for _, d := range doors {
		t.Run(d.name, func(t *testing.T) {
			newSrv := d.server
			if newSrv == nil {
				newSrv = testServer
			}
			srv := newSrv(t)
			ctype := d.ctype
			if ctype == "" {
				ctype = "application/json"
			}
			send := func(addr string) int {
				req := httptest.NewRequest(d.method, d.path, strings.NewReader(d.body))
				req.Header.Set("Content-Type", ctype)
				if d.ws {
					req.Header.Set("Connection", "Upgrade")
					req.Header.Set("Upgrade", "websocket")
					req.Header.Set("Sec-WebSocket-Version", "13")
					req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
				}
				req.RemoteAddr = "[" + addr + "]:1234"
				w := httptest.NewRecorder()
				srv.ServeHTTP(w, req)
				return w.Code
			}
			exhausted := false
			for i := 0; i < 200; i++ {
				if send(first) == http.StatusTooManyRequests {
					exhausted = true
					break
				}
			}
			if !exhausted {
				t.Fatalf("200 requests from %s never drew a 429; this door is not rate limited", first)
			}
			if code := send(sibling); code != http.StatusTooManyRequests {
				t.Errorf("%s (same /64) got %d, want 429: the door keys on the full address", sibling, code)
			}
			if code := send(other); code == http.StatusTooManyRequests {
				t.Errorf("%s (next /64) got 429: the /64s share a bucket", other)
			}
		})
	}
}

// The per-address charges that sit outside the middleware and are not
// reached by a plain request: the decision provider charge (called by a
// handler just before a provider call, which the test server has none of)
// and the share-password per-address bucket. The /mcp pre-auth charge is
// in the door table above, through the router.
func TestRateLimit_IPv6KeysOnThe64OutsideTheMiddleware(t *testing.T) {
	const (
		first   = "2001:db8:1:2::1"
		sibling = "2001:db8:1:2::2"
		other   = "2001:db8:1:3::1"
	)
	req := func(addr string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		r.RemoteAddr = "[" + addr + "]:1234"
		return r
	}
	charges := []struct {
		name   string
		charge func(srv *Server, r *http.Request) bool
	}{
		{"decision_provider", func(srv *Server, r *http.Request) bool {
			return srv.allowDecisionProviderCall(httptest.NewRecorder(), r)
		}},
	}
	for _, c := range charges {
		t.Run(c.name, func(t *testing.T) {
			srv := testServer(t)
			exhausted := false
			for i := 0; i < 500; i++ {
				if !c.charge(srv, req(first)) {
					exhausted = true
					break
				}
			}
			if !exhausted {
				t.Fatal("500 charges never refused")
			}
			if c.charge(srv, req(sibling)) {
				t.Errorf("%s (same /64) was allowed: the charge keys on the full address", sibling)
			}
			if !c.charge(srv, req(other)) {
				t.Errorf("%s (next /64) was refused: the /64s share a bucket", other)
			}
		})
	}

	t.Run("share_password_ip", func(t *testing.T) {
		srv := testServer(t)
		slug := createWSForTest(t, srv)
		cr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
			"name": "Shared", "prefix": "SHAR",
		})
		if cr.Code != http.StatusCreated {
			t.Fatalf("create collection: %d %s", cr.Code, cr.Body.String())
		}
		var coll models.Collection
		parseJSON(t, cr, &coll)
		owner, err := srv.store.CreateUser(models.UserCreate{Email: "o3308@test.com", Name: "O", Password: "pw-owner"})
		if err != nil {
			t.Fatal(err)
		}
		ws, err := srv.store.GetWorkspaceBySlug(slug)
		if err != nil || ws == nil {
			t.Fatalf("get workspace: %v", err)
		}
		link, err := srv.store.CreateShareLink(ws.ID, "collection", coll.ID, "view", owner.ID, &store.ShareLinkOptions{Password: "right"})
		if err != nil {
			t.Fatal(err)
		}
		exhausted := false
		for i := 0; i < 50; i++ {
			if resolveShareWithPassword(srv, link.Token, "wrong", "["+first+"]").Code == http.StatusTooManyRequests {
				exhausted = true
				break
			}
		}
		if !exhausted {
			t.Fatal("50 wrong guesses never drew a 429")
		}
		if code := resolveShareWithPassword(srv, link.Token, "wrong", "["+sibling+"]").Code; code != http.StatusTooManyRequests {
			t.Errorf("%s (same /64) got %d, want 429", sibling, code)
		}
		if code := resolveShareWithPassword(srv, link.Token, "wrong", "["+other+"]").Code; code == http.StatusTooManyRequests {
			t.Errorf("%s (next /64) got 429", other)
		}
	})
}
