package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestRedisHealthProbesAndReportsTransitions pins the three things the
// prober is FOR: it answers before the first tick, it notices Redis going
// away, and it notices it coming back.
//
// The reachable→unreachable leg is what a test of this could most easily
// skip, and it is the only leg the type exists for.
func TestRedisHealthProbesAndReportsTransitions(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
		// Bound the dial for the unreachable leg. Without this the probe
		// against a closed miniredis waits out go-redis's 5s DialTimeout
		// default — a context on the command does NOT cover connection
		// establishment (measured on BUG-2698: a 150ms ctx took 5.0s).
		DialTimeout: 200 * time.Millisecond,
	})
	t.Cleanup(func() { _ = client.Close() })

	var probes []bool
	probeCh := make(chan bool, 16)
	h := NewRedisHealth(client, func(ok bool) { probeCh <- ok })
	h.interval = 20 * time.Millisecond
	h.timeout = 200 * time.Millisecond

	// PREMISE: nothing is reported before Start, so the assertions below
	// are about probing rather than about a zero value that happens to
	// read the same way.
	if got := h.Status(); got.Probed {
		t.Fatalf("premise failed: Status().Probed is true before Start (%+v)", got)
	}

	h.Start()
	t.Cleanup(h.Stop)

	// Start probes synchronously, so a healthy answer is available with
	// no waiting at all — that is the property that closes the
	// "unmeasured" window at boot.
	got := h.Status()
	if !got.Probed || !got.Reachable {
		t.Fatalf("after Start, want probed+reachable, got %+v", got)
	}
	if got.LastCheck.IsZero() {
		t.Fatalf("after Start, LastCheck is zero (%+v)", got)
	}
	probes = append(probes, <-probeCh)
	if !probes[0] {
		t.Fatalf("first probe callback reported %v, want true", probes[0])
	}

	// Redis goes away.
	mr.Close()
	waitForStatus(t, h, func(s RedisHealthStatus) bool { return s.Probed && !s.Reachable },
		"the prober to notice Redis is unreachable")
	if got := h.Status(); got.Error == "" {
		t.Fatalf("an unreachable probe reported no error text (%+v)", got)
	}

	// And comes back on the same address.
	if err := mr.Restart(); err != nil {
		t.Fatalf("restart miniredis: %v", err)
	}
	waitForStatus(t, h, func(s RedisHealthStatus) bool { return s.Reachable },
		"the prober to notice Redis is reachable again")
	if got := h.Status(); got.Error != "" {
		t.Fatalf("a recovered probe left stale error text %q (%+v)", got.Error, got)
	}
}

// TestRedisHealthStopIsSafeOnNil and the zero-value Status path: a
// deployment without Redis holds a nil *RedisHealth, and every caller
// reaches it through the same methods.
func TestRedisHealthNilIsInert(t *testing.T) {
	t.Parallel()

	var h *RedisHealth
	h.Start()
	h.Stop()
	if got := h.Status(); got.Probed || got.Reachable {
		t.Fatalf("nil prober reported %+v, want the zero status", got)
	}
}

func waitForStatus(t *testing.T, h *RedisHealth, cond func(RedisHealthStatus) bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond(h.Status()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s (last status: %+v)", what, h.Status())
}

// TestHealthEndpointsAreMountedWhereDocumented pins the three health
// URLs. docs/deployment.md, deploy/k8s/deployment.yaml and every
// operator runbook name these paths; a route that moves silently turns
// every one of them into a 404, and nothing else in the suite would
// notice.
func TestHealthEndpointsAreMountedWhereDocumented(t *testing.T) {
	t.Parallel()

	srv := testServer(t)
	for _, path := range []string{
		"/api/v1/health",
		"/api/v1/health/live",
		"/api/v1/health/ready",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s is not mounted (404) — operator docs and k8s probes point at it", path)
		}
	}

	// The negative control: a path that must NOT exist, so a router that
	// answered everything could not pass the loop above.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/api/v1/health/ready", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("/api/v1/api/v1/health/ready answered %d — the route is double-prefixed", rec.Code)
	}
}

// TestHealthReadyReportsRedisWithoutGatingOnIt is the ruling made
// executable: an unreachable Redis must be VISIBLE in the payload and
// must NOT change the status code. A test that only checked the payload
// would pass against a handler that also 503'd, which is the specific
// regression that would take healthy replicas out of a load balancer.
func TestHealthReadyReportsRedisWithoutGatingOnIt(t *testing.T) {
	t.Parallel()

	srv := testServer(t)

	// PREMISE: with no prober wired — a deployment with no Redis — there
	// is no redis block at all. "No Redis configured" and "Redis
	// configured and down" are different facts and a false would merge
	// them.
	body := readyBody(t, srv, http.StatusOK)
	if _, ok := body["redis"]; ok {
		t.Fatalf("premise failed: a server with no prober reported a redis block: %v", body["redis"])
	}

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{
		Addr:        mr.Addr(),
		DialTimeout: 200 * time.Millisecond,
	})
	t.Cleanup(func() { _ = client.Close() })
	h := NewRedisHealth(client, nil)
	h.interval = 20 * time.Millisecond
	h.timeout = 200 * time.Millisecond
	h.Start()
	t.Cleanup(h.Stop)
	srv.SetRedisHealth(h)

	body = readyBody(t, srv, http.StatusOK)
	redisBlock, ok := body["redis"].(map[string]interface{})
	if !ok {
		t.Fatalf("healthy: no redis block in %v", body)
	}
	if redisBlock["reachable"] != true {
		t.Fatalf("healthy: reachable = %v, want true", redisBlock["reachable"])
	}
	if _, hasDegrades := redisBlock["degrades"]; hasDegrades {
		t.Fatalf("healthy: reported degrades %v, want none", redisBlock["degrades"])
	}

	mr.Close()
	waitForStatus(t, h, func(s RedisHealthStatus) bool { return s.Probed && !s.Reachable },
		"the prober to notice Redis is unreachable")

	// THE ASSERTION THIS TEST EXISTS FOR: still 200.
	body = readyBody(t, srv, http.StatusOK)
	redisBlock, ok = body["redis"].(map[string]interface{})
	if !ok {
		t.Fatalf("degraded: no redis block in %v", body)
	}
	if redisBlock["reachable"] != false {
		t.Fatalf("degraded: reachable = %v, want false", redisBlock["reachable"])
	}
	if redisBlock["error"] == nil || redisBlock["error"] == "" {
		t.Fatalf("degraded: no error text in %v", redisBlock)
	}
	degrades, ok := redisBlock["degrades"].([]interface{})
	if !ok || len(degrades) == 0 {
		t.Fatalf("degraded: want a non-empty degrades list naming what is lost, got %v", redisBlock["degrades"])
	}
	// The status field itself must still say ready — a caller that reads
	// the body rather than the code must not be told otherwise either.
	if body["status"] != "ready" {
		t.Fatalf("degraded: status = %v, want \"ready\"", body["status"])
	}
}

// readyBody drives readiness THROUGH THE ROUTER, not by calling the
// handler directly.
//
// The direct call was the original shape and it could not see the path.
// A scripted comment edit later rewrote the route registration itself to
// "/api/v1/health/ready" INSIDE the /api/v1 group — so the real endpoint
// became /api/v1/api/v1/health/ready and readiness 404'd at the
// documented path — and this suite stayed green throughout, because a
// handler invoked by hand has no route to be wrong. Codex found it; the
// test could not.
//
// Going through srv.ServeHTTP costs nothing and makes the URL part of
// what is asserted.
func readyBody(t *testing.T, srv *Server, wantCode int) map[string]interface{} {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health/ready", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != wantCode {
		t.Fatalf("health/ready status = %d, want %d (body: %s)", rec.Code, wantCode, rec.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health/ready body: %v (raw: %s)", err, rec.Body.String())
	}
	return body
}
