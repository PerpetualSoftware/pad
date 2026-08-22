package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
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

	t.Run("anonymous callers count globally but not per user", func(t *testing.T) {
		a := newStreamAdmission(3, 1)
		// An empty user id is the legacy workspace-token and fresh-install
		// case on /api/v1/events. Bucketing them together under "" would
		// make unrelated anonymous callers evict each other at maxUser=1.
		for i := 0; i < 3; i++ {
			if _, refusal := a.acquire(""); refusal != admissionRefusalNone {
				t.Fatalf("anonymous acquire %d refused by %q, want admitted up to the global bound", i, refusal)
			}
		}
		if _, refusal := a.acquire(""); refusal != admissionRefusalGlobal {
			t.Fatalf("fourth anonymous acquire refused by %q, want the global bound", refusal)
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
