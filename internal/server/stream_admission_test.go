package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/metrics"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// rawWatchStreamStatus opens the watch stream and returns only the status
// code, closing immediately. Used for the refusal legs, where there is no
// stream to read.
func rawWatchStreamStatus(t *testing.T, baseURL, token string) int {
	t.Helper()
	req, err := http.NewRequest("GET", baseURL+"/api/v1/events/stream", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// holdAuthedSSE opens the workspace-scoped stream WITH a bearer token and
// holds it, returning a close func. connectSSE (handlers_events_test.go)
// sends no credentials, which is fine for its own tests but 401s here —
// setupWatchTestUser creates a user, and once one exists the endpoint
// requires auth, so an unauthenticated connection never reaches the
// admission check at all and the shared-budget assertion would be
// vacuous.
//
// It reads the first line before returning, so the caller knows the
// connection is actually established and holding a slot rather than
// merely dispatched.
func holdAuthedSSE(ctx context.Context, t *testing.T, baseURL, slug, token string) func() {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/api/v1/events?workspace="+slug, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("premise failed: authed SSE connect got %d, want 200", resp.StatusCode)
	}
	buf := make([]byte, 1)
	if _, err := resp.Body.Read(buf); err != nil {
		resp.Body.Close()
		t.Fatalf("premise failed: SSE stream produced no bytes: %v", err)
	}
	return func() { resp.Body.Close() }
}

// TestWatchStreamHasAConnectionLimit is BUG-2726's core claim made
// executable: this endpoint had NO connection bound, so the first
// assertion here fails on the unfixed code with a 200.
//
// The premise leg matters as much as the refusal: the first connection
// must succeed, or a handler that refused everything would pass the
// refusal assertion.
func TestWatchStreamHasAConnectionLimit(t *testing.T) {
	t.Parallel()

	srv := testServerWithWatchEvents(t)
	srv.SetSSELimits(1, 0, 0) // one streaming connection process-wide
	_, _, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// PREMISE: the first one connects.
	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	if ev := waitForWatchEvent(t, ch, 3*time.Second); ev.Type != "connected" {
		t.Fatalf("premise failed: first stream got %q, want connected", ev.Type)
	}

	if code := rawWatchStreamStatus(t, ts.URL, tok.Token); code != http.StatusTooManyRequests {
		t.Fatalf("second watch stream got %d, want 429 — the endpoint is unbounded", code)
	}
}

// TestStreamAdmissionIsSharedAcrossBothEndpoints is the ruling that
// PAD_SSE_MAX_CONNECTIONS covers both streams, made executable. It is the
// assertion a per-endpoint implementation would fail, and the one that
// distinguishes the shipped design from the rejected parallel-knob
// alternative.
func TestStreamAdmissionIsSharedAcrossBothEndpoints(t *testing.T) {
	t.Parallel()

	srv := testServerWithWatchEvents(t)
	srv.SetEventBus(events.New())
	srv.SetSSELimits(1, 0, 0)
	slug, _, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Hold the WORKSPACE stream...
	closeSSE := holdAuthedSSE(ctx, t, ts.URL, slug, tok.Token)
	defer closeSSE()

	// ...and the WATCH stream must be refused by the same budget.
	if code := rawWatchStreamStatus(t, ts.URL, tok.Token); code != http.StatusTooManyRequests {
		t.Fatalf("watch stream got %d while the global budget was spent by /api/v1/events, want 429", code)
	}
}

// TestStreamAdmissionReleasesOnDisconnect pins the other half of a
// bound: a limit that never released would refuse forever after the
// first N connections, which is a worse bug than the one being fixed and
// is invisible to a test that only opens connections.
func TestStreamAdmissionReleasesOnDisconnect(t *testing.T) {
	t.Parallel()

	srv := testServerWithWatchEvents(t)
	srv.SetSSELimits(1, 0, 0)
	_, _, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	if ev := waitForWatchEvent(t, ch, 3*time.Second); ev.Type != "connected" {
		t.Fatalf("premise failed: first stream got %q, want connected", ev.Type)
	}
	// PREMISE: the budget is genuinely spent while it is held.
	if code := rawWatchStreamStatus(t, ts.URL, tok.Token); code != http.StatusTooManyRequests {
		t.Fatalf("premise failed: second stream got %d while the first was held, want 429", code)
	}

	cancel() // client disconnects

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rawWatchStreamStatus(t, ts.URL, tok.Token) == http.StatusOK {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the slot was never released after the client disconnected")
}

// TestStreamAdmissionPerUserLimit covers the bound that exists because
// the global one alone lets a single user exhaust the process for
// everyone — the failure the per-workspace limit could not prevent, since
// the watch stream has no workspace to count against.
func TestStreamAdmissionPerUserLimit(t *testing.T) {
	t.Parallel()

	srv := testServerWithWatchEvents(t)
	srv.SetSSELimits(0, 0, 1) // no global bound at all; one per user
	_, _, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	if ev := waitForWatchEvent(t, ch, 3*time.Second); ev.Type != "connected" {
		t.Fatalf("premise failed: first stream got %q, want connected", ev.Type)
	}

	if code := rawWatchStreamStatus(t, ts.URL, tok.Token); code != http.StatusTooManyRequests {
		t.Fatalf("second stream for the same user got %d, want 429", code)
	}
}

// TestStreamAdmissionUnitSemantics covers the parts of the gate that are
// awkward to reach through HTTP: unlimited-when-zero, the anonymous
// caller that counts globally but not per-user, and release idempotence.
func TestStreamAdmissionUnitSemantics(t *testing.T) {
	t.Parallel()

	t.Run("zero means unlimited", func(t *testing.T) {
		a := newStreamAdmission(0, 0)
		for i := 0; i < 500; i++ {
			if _, refusal := a.acquire("u"); refusal != admissionRefusalNone {
				t.Fatalf("acquire %d refused by %q with both bounds at 0", i, refusal)
			}
		}
	})

	t.Run("workspace-derived principals are bounded like users", func(t *testing.T) {
		// Codex round 3 P2. A caller with no user — a legacy
		// workspace-scoped token, or the fresh-install no-auth window —
		// is bucketed by workspace rather than skipping the per-user
		// bound. Skipping it let one legacy-token holder fill the global
		// budget and 429 everyone else.
		a := newStreamAdmission(10, 1)
		if _, refusal := a.acquire("ws:workspace-a"); refusal != admissionRefusalNone {
			t.Fatalf("first workspace-principal acquire refused by %q", refusal)
		}
		if _, refusal := a.acquire("ws:workspace-a"); refusal != admissionRefusalUser {
			t.Fatalf("second acquire for the same workspace principal refused by %q, want the per-user bound", refusal)
		}
		// A DIFFERENT workspace is a different principal and unaffected —
		// the property that made the empty-string bucket wrong.
		if _, refusal := a.acquire("ws:workspace-b"); refusal != admissionRefusalNone {
			t.Fatalf("a different workspace principal was refused by %q", refusal)
		}
	})

	t.Run("an empty principal still skips the per-user bound", func(t *testing.T) {
		// Defensive: both call sites supply a principal, and this guard
		// exists so a future one that forgets cannot collapse every
		// connection into a single bucket and start evicting unrelated
		// callers. Asserted so the behaviour is deliberate rather than
		// incidental.
		a := newStreamAdmission(3, 1)
		for i := 0; i < 3; i++ {
			if _, refusal := a.acquire(""); refusal != admissionRefusalNone {
				t.Fatalf("empty-principal acquire %d refused by %q", i, refusal)
			}
		}
		if _, refusal := a.acquire(""); refusal != admissionRefusalGlobal {
			t.Fatalf("fourth empty-principal acquire refused by %q, want the global bound", refusal)
		}
	})

	t.Run("release is idempotent", func(t *testing.T) {
		a := newStreamAdmission(1, 0)
		release, refusal := a.acquire("u")
		if refusal != admissionRefusalNone {
			t.Fatalf("first acquire refused by %q", refusal)
		}
		release()
		release() // a double release must not mint a phantom slot
		if _, refusal := a.acquire("u"); refusal != admissionRefusalNone {
			t.Fatalf("acquire after release refused by %q", refusal)
		}
		if _, refusal := a.acquire("u"); refusal != admissionRefusalGlobal {
			t.Fatalf("a double release inflated the budget: second acquire refused by %q, want global", refusal)
		}
	})

	t.Run("per-user counters do not leak", func(t *testing.T) {
		a := newStreamAdmission(0, 5)
		for i := 0; i < 100; i++ {
			release, refusal := a.acquire("u")
			if refusal != admissionRefusalNone {
				t.Fatalf("acquire %d refused by %q", i, refusal)
			}
			release()
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if len(a.perUser) != 0 {
			t.Fatalf("perUser holds %d entries after every slot was released, want 0 — the map grows without bound", len(a.perUser))
		}
	})
}

// TestStreamAdmissionTracksItsTotal pins the number the gauge reads.
//
// This used to assert a push callback's argument sequence. The callback
// is gone — pad_stream_connections_active is a scrape-time collector now,
// because the pushed version cost two review rounds (a deadlock hazard,
// then a reordering that left the gauge below the truth) and reading the
// number on demand has neither failure mode. What is left to assert is
// that the number itself is right in both directions.
func TestStreamAdmissionTracksItsTotal(t *testing.T) {
	t.Parallel()

	a := newStreamAdmission(0, 0)
	if got := a.heldTotal(); got != 0 {
		t.Fatalf("premise failed: fresh gate reports %d held", got)
	}

	r1, _ := a.acquire("u1")
	if got := a.heldTotal(); got != 1 {
		t.Fatalf("after one admit: %d, want 1", got)
	}
	r2, _ := a.acquire("u2")
	if got := a.heldTotal(); got != 2 {
		t.Fatalf("after two admits: %d, want 2", got)
	}
	r1()
	if got := a.heldTotal(); got != 1 {
		t.Fatalf("after one release: %d, want 1", got)
	}
	r2()
	if got := a.heldTotal(); got != 0 {
		t.Fatalf("after both releases: %d, want 0", got)
	}
	r2() // idempotent release must not go negative
	if got := a.heldTotal(); got != 0 {
		t.Fatalf("after a double release: %d, want 0", got)
	}
}

// TestStreamGaugeReachesMetricsThroughEitherWiringOrder covers the seam
// that SetMetrics and SetSSELimits can be called in either order, plus
// the lazily-built gate a server that never sets limits ends up with. All
// three paths have to end with a gauge that moves, or the metric lies by
// staying at zero while streams are held.
func TestStreamGaugeReachesMetricsThroughEitherWiringOrder(t *testing.T) {
	t.Parallel()

	t.Run("limits then metrics", func(t *testing.T) {
		t.Parallel()
		srv := testServer(t)
		srv.SetSSELimits(0, 0, 0)
		m := metrics.New()
		srv.SetMetrics(m)
		assertGaugeTracksAdmission(t, srv, m)
	})

	t.Run("metrics then limits", func(t *testing.T) {
		t.Parallel()
		srv := testServer(t)
		m := metrics.New()
		srv.SetMetrics(m)
		srv.SetSSELimits(0, 0, 0)
		assertGaugeTracksAdmission(t, srv, m)
	})

	t.Run("metrics only, gate built lazily", func(t *testing.T) {
		t.Parallel()
		srv := testServer(t)
		m := metrics.New()
		srv.SetMetrics(m)
		assertGaugeTracksAdmission(t, srv, m)
	})
}

func assertGaugeTracksAdmission(t *testing.T, srv *Server, m *metrics.Metrics) {
	t.Helper()

	if got := gaugeValue(t, m); got != 0 {
		t.Fatalf("premise failed: gauge starts at %v, want 0", got)
	}
	release, refusal := srv.admission().acquire("u1")
	if refusal != admissionRefusalNone {
		t.Fatalf("acquire refused by %q with no limits set", refusal)
	}
	if got := gaugeValue(t, m); got != 1 {
		t.Fatalf("gauge = %v after one admit, want 1", got)
	}
	release()
	if got := gaugeValue(t, m); got != 0 {
		t.Fatalf("gauge = %v after release, want 0", got)
	}
}

func gaugeValue(t *testing.T, m *metrics.Metrics) float64 {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "pad_stream_connections_active" {
			continue
		}
		if len(f.GetMetric()) != 1 {
			t.Fatalf("pad_stream_connections_active has %d series, want 1", len(f.GetMetric()))
		}
		return f.GetMetric()[0].GetGauge().GetValue()
	}
	t.Fatal("pad_stream_connections_active is not exported")
	return 0
}

// TestStreamPrincipalFallsBackToWorkspace covers the resolution order
// itself, which is where the codex round 3 fix actually lives: a request
// with no user must still produce a bucket key, or the per-user bound is
// unreachable for exactly the principal class that could abuse it.
func TestStreamPrincipalFallsBackToWorkspace(t *testing.T) {
	t.Parallel()

	// No user, no token workspace — the fresh-install no-auth window.
	// The resolved workspace stands in.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace=w", nil)
	if got, want := streamPrincipal(req, "workspace-id"), "ws:workspace-id"; got != want {
		t.Errorf("streamPrincipal with no user = %q, want %q", got, want)
	}

	// Nothing at all to key on: empty, and the gate's guard handles it.
	if got := streamPrincipal(req, ""); got != "" {
		t.Errorf("streamPrincipal with no user and no workspace = %q, want empty", got)
	}

	// A user id wins over the workspace fallback — otherwise every
	// authenticated caller in a workspace would share one bucket, which
	// is a denial of service dressed as a limit.
	withUser := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace=w", nil)
	withUser = withUser.WithContext(WithCurrentUser(withUser.Context(), &models.User{ID: "user-1"}))
	if got, want := streamPrincipal(withUser, "workspace-id"), "user-1"; got != want {
		t.Errorf("streamPrincipal with a user = %q, want %q", got, want)
	}
}

// TestWorkspaceStreamBoundsAnonymousCallersPerWorkspace is the WIRING
// test for the principal fix, and it exists because the unit test above
// could not catch a handler that ignored streamPrincipal and passed
// currentUserID directly — mutation testing survived exactly that.
// Testing the helper proves the helper; the handler passing it is a
// separate claim and needs an instrument at the layer that owns the call.
//
// Drives the fresh-install no-auth window (no users exist, so
// currentUser is nil), which is the reachable no-user case in tests and
// takes the same code path a legacy workspace token does.
func TestWorkspaceStreamBoundsAnonymousCallersPerWorkspace(t *testing.T) {
	t.Parallel()

	srv := testServerWithEvents(t)
	srv.SetSSELimits(0, 0, 1) // no global bound, one per principal
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug1 := createTestWorkspace(t, ts.URL, "WS One")
	slug2 := createTestWorkspace(t, ts.URL, "WS Two")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// PREMISE: the first anonymous connection is admitted, so the refusal
	// below is the bound rather than a broken endpoint.
	ch := connectSSE(ctx, t, ts.URL, slug1)
	waitForEvent(t, ch, 3*time.Second)

	// With the bound skipped for no-user callers, this second one would be
	// admitted and one legacy-token holder could fill the whole budget.
	if code := rawSSEStatus(t, ts.URL, slug1); code != http.StatusTooManyRequests {
		t.Fatalf("second anonymous connection to the same workspace got %d, want 429", code)
	}

	// A DIFFERENT workspace is a different principal and must still be
	// admitted — otherwise the fix is a global bound wearing a per-user
	// label.
	ch2 := connectSSE(ctx, t, ts.URL, slug2)
	if ev := waitForEvent(t, ch2, 3*time.Second); ev.Type != "connected" {
		t.Fatalf("connection to a second workspace got %q, want connected", ev.Type)
	}
}

func rawSSEStatus(t *testing.T, baseURL, slug string) int {
	t.Helper()
	req, err := http.NewRequest("GET", baseURL+"/api/v1/events?workspace="+slug, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestSetSSELimitsDoesNotStrandHeldSlots covers reconfiguration while
// connections are held. Note the scope: SetSSELimits is config-time
// (see its doc comment — it writes Server fields handlers read), so this
// is about not corrupting the gate's own accounting, not about supporting
// live reconfiguration. Replacing the gate would leave the old one
// holding the count and the new one starting at zero, so every connection
// already open stops counting against the limit — the process silently
// over-grants capacity by however many streams were held at the moment
// limits were set.
//
// The assertion is that a HELD slot still counts after reconfiguration,
// which is what a replacement breaks. An earlier version of this test
// asserted the gauge instead and SURVIVED the mutation: the discarded
// gate keeps its observer, so its releases keep driving the gauge and
// the numbers look right while the budget is wrong. Worth recording —
// the wrong end state is the one nothing is watching.
func TestSetSSELimitsDoesNotStrandHeldSlots(t *testing.T) {
	t.Parallel()

	srv := testServer(t)
	m := metrics.New()
	srv.SetMetrics(m)
	srv.SetSSELimits(1, 0, 0) // one streaming connection process-wide

	release, refusal := srv.admission().acquire("u1")
	if refusal != admissionRefusalNone {
		t.Fatalf("premise failed: acquire refused by %q", refusal)
	}
	if _, refusal := srv.admission().acquire("u2"); refusal != admissionRefusalGlobal {
		t.Fatalf("premise failed: the budget was not spent — second acquire refused by %q, want global", refusal)
	}

	// Reconfigure to the SAME bound while the connection is still held.
	srv.SetSSELimits(1, 0, 0)

	if _, refusal := srv.admission().acquire("u3"); refusal != admissionRefusalGlobal {
		t.Fatalf("after reconfiguring, a second connection was admitted (refusal %q) under a limit of 1 — "+
			"the held slot stopped counting, so the process over-grants capacity", refusal)
	}

	// The gauge still tracks the one real connection, in both directions.
	if got := gaugeValue(t, m); got != 1 {
		t.Fatalf("gauge = %v with one connection held, want 1", got)
	}
	release()
	if got := gaugeValue(t, m); got != 0 {
		t.Fatalf("gauge = %v after release, want 0", got)
	}

	// And the NEW limits land on the same gate.
	srv.SetSSELimits(500, 100, 25)
	if a := srv.admission(); a.maxUser != 25 || a.maxTotal != 500 {
		t.Fatalf("gate holds maxTotal=%d maxUser=%d, want 500/25 — the update did not land", a.maxTotal, a.maxUser)
	}
}

// TestStreamGaugeIsCorrectUnderConcurrency drives the gate hard from many
// goroutines and asserts the SCRAPED value matches the gate's own total.
//
// The pushed-callback version of this metric needed a deliberately
// widened race window to be testable at all, and even then the first
// attempt survived five runs against a broken build. A collector has no
// window to widen: the scrape reads the same lock the mutations take, so
// this asserts the property directly.
func TestStreamGaugeIsCorrectUnderConcurrency(t *testing.T) {
	t.Parallel()

	const (
		workers = 16
		cycles  = 40
		held    = 8
	)

	m := metrics.New()
	a := newStreamAdmission(0, 0)
	m.RegisterStreamConnectionsCollector(a.heldTotal)

	// A steady population held for the whole run, so the final total is
	// non-zero and a collector stuck at 0 cannot pass.
	for i := 0; i < held; i++ {
		if _, refusal := a.acquire("holder"); refusal != admissionRefusalNone {
			t.Fatalf("premise failed: holder %d refused by %q", i, refusal)
		}
	}
	if got := gaugeValue(t, m); got != float64(held) {
		t.Fatalf("premise failed: gauge = %v with %d held", got, held)
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for c := 0; c < cycles; c++ {
				release, refusal := a.acquire(fmt.Sprintf("user-%d", w))
				if refusal != admissionRefusalNone {
					t.Errorf("worker %d cycle %d refused by %q with no limits", w, c, refusal)
					return
				}
				// Scrape WHILE the churn is running, so the collector is
				// exercised concurrently with the mutations rather than
				// only at rest.
				_ = gaugeValueQuiet(m)
				release()
			}
		}(w)
	}
	wg.Wait()

	if got, want := gaugeValue(t, m), float64(a.heldTotal()); got != want {
		t.Fatalf("gauge = %v, gate total = %v", got, want)
	}
	if got := gaugeValue(t, m); got != float64(held) {
		t.Fatalf("gauge = %v after every transient slot was released, want %d", got, held)
	}
}

// gaugeValueQuiet is gaugeValue without the testing hooks, safe to call
// from a worker goroutine (t.Fatalf from a non-test goroutine is an
// error in its own right).
func gaugeValueQuiet(m *metrics.Metrics) float64 {
	families, err := m.Registry.Gather()
	if err != nil {
		return -1
	}
	for _, f := range families {
		if f.GetName() == "pad_stream_connections_active" && len(f.GetMetric()) == 1 {
			return f.GetMetric()[0].GetGauge().GetValue()
		}
	}
	return -1
}

// TestRedisHealthConcurrentStartStop drives the lifecycle from many
// goroutines. The round-9 fix guarded the FIELDS; round 10 pointed out
// that Stop released the lock before cancelling and waiting, so a Start
// could interleave and install a loop that Stop would wait for but never
// cancel. The failure is a hang, so this bounds itself.
func TestRedisHealthConcurrentStartStop(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = client.Close() })

	h := NewRedisHealth(client, nil)
	h.interval = time.Millisecond
	h.timeout = 200 * time.Millisecond

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(2)
			go func() { defer wg.Done(); h.Start() }()
			go func() { defer wg.Done(); h.Stop() }()
		}
		wg.Wait()
		h.Stop() // and settle
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("concurrent Start/Stop hung — a Start interleaved with a Stop's cancel-and-wait")
	}
}
