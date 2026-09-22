package server

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/decision"
	"golang.org/x/time/rate"
)

// TASK-3141: synchronous provider calls get their own bucket, charged in the
// handler immediately before the provider call.

// pinDecisionBucket replaces the DecisionProvider bucket with one of the same
// burst that effectively never refills, so "the 6th call inside the window"
// does not depend on how fast five requests run on a loaded runner. The
// production numbers are pinned separately in
// TestDecisionProviderBucketDefaults.
func pinDecisionBucket(t *testing.T, srv *Server) {
	t.Helper()
	if srv.rateLimiters == nil {
		t.Skip("rate limiting disabled in this environment (PAD_DISABLE_RATE_LIMITS)")
	}
	srv.rateLimiters.DecisionProvider.Stop()
	srv.rateLimiters.DecisionProvider = newIPRateLimiter(rateLimitConfig{
		Rate:  rate.Limit(1.0 / 3600.0),
		Burst: srv.rateLimiters.DecisionProvider.config.Burst,
	})
	t.Cleanup(srv.rateLimiters.DecisionProvider.Stop)
}

func matchNoneProvider() *fakeMatchProvider {
	return &fakeMatchProvider{askFn: func(_ any, _ map[string]decision.Question) (map[string]decision.Answer, decision.Usage, error) {
		return map[string]decision.Answer{"match": {Kind: decision.KindChoice, Choice: "none", Confidence: ptrFloat(0.9)}}, decision.Usage{}, nil
	}}
}

func TestDecisionProviderBucketDefaults(t *testing.T) {
	rl := NewRateLimiters()
	t.Cleanup(rl.Stop)
	if got, want := rl.DecisionProvider.config.Rate, rate.Limit(30.0/60.0); got != want {
		t.Errorf("rate = %v, want %v (30/min)", got, want)
	}
	if got := rl.DecisionProvider.config.Burst; got != 5 {
		t.Errorf("burst = %d, want 5", got)
	}
}

// The burst is spent by calls that reach the provider; the next one is a 429
// carrying Retry-After, and the provider is not called for it.
func TestPlaybookMatch_DecisionRateLimitIs429WithRetryAfter(t *testing.T) {
	srv := testServer(t)
	pinDecisionBucket(t, srv)
	slug := createWSWithCollections(t, srv)
	createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":  "Ship",
		"fields": `{"status":"active"}`,
	})
	p := matchNoneProvider()
	attachMatchRunner(srv, p)

	path := "/api/v1/workspaces/" + slug + "/playbooks/match"
	burst := srv.rateLimiters.DecisionProvider.config.Burst
	for i := 0; i < burst; i++ {
		if rr := doRequest(srv, "POST", path, matchRequestBody("ship it")); rr.Code != http.StatusOK {
			t.Fatalf("call %d: expected 200, got %d: %s", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := doRequest(srv, "POST", path, matchRequestBody("ship it"))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("call %d: expected 429, got %d: %s", burst+1, rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("429 carries no Retry-After header")
	}
	if got := p.callCount(); got != burst {
		t.Errorf("provider called %d times, want %d: the refused request must not reach it", got, burst)
	}

	// Per caller: another client's bucket is untouched.
	other := doRequestFromRemoteAddr(srv, "POST", path, matchRequestBody("ship it"), "198.51.100.7:1234")
	if other.Code != http.StatusOK {
		t.Errorf("a different caller got %d, want 200: the bucket must be per caller", other.Code)
	}
}

// Requests that answer without a provider call consume nothing: an
// unconfigured provider's 404, a 400 for bad text, and zero active playbooks.
// After many of each, the full burst is still available.
func TestPlaybookMatch_NonSpendingRequestsConsumeNoToken(t *testing.T) {
	srv := testServer(t)
	pinDecisionBucket(t, srv)
	slug := createWSWithCollections(t, srv)
	path := "/api/v1/workspaces/" + slug + "/playbooks/match"
	burst := srv.rateLimiters.DecisionProvider.config.Burst

	// No provider configured: 404, repeatedly.
	for i := 0; i < burst*2; i++ {
		if rr := doRequest(srv, "POST", path, matchRequestBody("ship it")); rr.Code != http.StatusNotFound {
			t.Fatalf("unconfigured call %d: expected 404, got %d: %s", i+1, rr.Code, rr.Body.String())
		}
	}

	p := matchNoneProvider()
	attachMatchRunner(srv, p)
	// Zero active playbooks: 200 with no provider call, repeatedly.
	for i := 0; i < burst*2; i++ {
		if rr := doRequest(srv, "POST", path, matchRequestBody("ship it")); rr.Code != http.StatusOK {
			t.Fatalf("zero-playbook call %d: expected 200, got %d: %s", i+1, rr.Code, rr.Body.String())
		}
	}
	// Bad text: 400, repeatedly.
	for i := 0; i < burst*2; i++ {
		if rr := doRequest(srv, "POST", path, matchRequestBody("   ")); rr.Code != http.StatusBadRequest {
			t.Fatalf("bad-text call %d: expected 400, got %d: %s", i+1, rr.Code, rr.Body.String())
		}
	}
	if got := p.callCount(); got != 0 {
		t.Fatalf("fixture: provider called %d times before any spending request", got)
	}

	createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":  "Ship",
		"fields": `{"status":"active"}`,
	})
	for i := 0; i < burst; i++ {
		if rr := doRequest(srv, "POST", path, matchRequestBody("ship it")); rr.Code != http.StatusOK {
			t.Fatalf("spending call %d of %d: expected 200, got %d: a non-spending request consumed a token", i+1, burst, rr.Code)
		}
	}
	if rr := doRequest(srv, "POST", path, matchRequestBody("ship it")); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("control: call %d should exhaust the bucket, got %d", burst+1, rr.Code)
	}
}

// RateLimiters.Stop documents that every limiter must be in its list or its
// cleanup goroutine leaks (BUG-851). Enforced here rather than trusted.
func TestRateLimitersStopCoversEveryLimiter(t *testing.T) {
	rl := NewRateLimiters()
	rl.Stop()
	v := reflect.ValueOf(rl).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		lim, ok := f.Interface().(*ipRateLimiter)
		if !ok || lim == nil {
			continue
		}
		select {
		case <-lim.stopCh:
		default:
			t.Errorf("RateLimiters.%s is not stopped by Stop(); add it to the list", v.Type().Field(i).Name)
		}
	}
}
