package server

import (
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/metrics"
)

// Tests for the MCP session tracker (PLAN-943 TASK-1120). The tracker
// replaces the naive +1/-1 active-sessions accounting with a map
// keyed by Mcp-Session-Id plus a periodic TTL sweeper. These tests
// exercise the tracker in isolation; the trackMCPSession seam that
// the audit middleware uses gets its own tests below so the
// integration contract is also pinned.

// TestMCPSessionTracker_TouchInsertsAndDeduplicates pins the touch
// semantics: first insert fires onChange (gauge bump), repeat
// touches refresh lastSeen but do NOT fire onChange (no gauge churn
// per per-session tool call).
func TestMCPSessionTracker_TouchInsertsAndDeduplicates(t *testing.T) {
	var fired int
	var lastCount int
	tr := newMCPSessionTracker(time.Minute, func(n int) {
		fired++
		lastCount = n
	})

	tr.touch("sess-a")
	if fired != 1 || lastCount != 1 {
		t.Errorf("after first touch: fired=%d count=%d, want fired=1 count=1", fired, lastCount)
	}

	tr.touch("sess-a") // refresh — must NOT bump
	if fired != 1 {
		t.Errorf("repeat touch must not fire onChange; fired=%d, want 1", fired)
	}

	tr.touch("sess-b")
	if fired != 2 || lastCount != 2 {
		t.Errorf("after second distinct touch: fired=%d count=%d, want fired=2 count=2", fired, lastCount)
	}

	if got := tr.size(); got != 2 {
		t.Errorf("size: got %d, want 2", got)
	}
}

// TestMCPSessionTracker_TouchEmptyIDIsNoOp guards against a stray
// empty header dropping a phantom entry into the map.
func TestMCPSessionTracker_TouchEmptyIDIsNoOp(t *testing.T) {
	var fired int
	tr := newMCPSessionTracker(time.Minute, func(int) { fired++ })

	tr.touch("")
	if tr.size() != 0 {
		t.Errorf("empty id must not insert; size=%d", tr.size())
	}
	if fired != 0 {
		t.Errorf("empty id must not fire onChange; fired=%d", fired)
	}
}

// TestMCPSessionTracker_EvictRemovesAndFires verifies evict bumps
// onChange ONLY when an entry was actually present.
func TestMCPSessionTracker_EvictRemovesAndFires(t *testing.T) {
	var fired int
	var lastCount int
	tr := newMCPSessionTracker(time.Minute, func(n int) {
		fired++
		lastCount = n
	})

	tr.touch("sess-a") // fires once
	tr.evict("sess-a") // fires again
	if fired != 2 || lastCount != 0 {
		t.Errorf("after touch+evict: fired=%d count=%d, want fired=2 count=0", fired, lastCount)
	}

	// Evict an unknown id — must NOT fire (would create false-positive
	// gauge churn on benign DELETEs from clients that already lost
	// their session record).
	tr.evict("never-existed")
	if fired != 2 {
		t.Errorf("evict of unknown id must not fire onChange; fired=%d", fired)
	}
}

// TestMCPSessionTracker_SweepEvictsStale exercises the TTL sweeper:
// entries older than ttl are removed, recent entries survive, and a
// single onChange observation fires per sweep regardless of how many
// entries are evicted (avoids spurious gauge oscillation).
func TestMCPSessionTracker_SweepEvictsStale(t *testing.T) {
	var fired int
	tr := newMCPSessionTracker(50*time.Millisecond, func(int) { fired++ })

	// Three "old" entries directly written into the map below the
	// touch path so we control the lastSeen exactly. Touch first to
	// register, then rewind their timestamps.
	tr.touch("old-1")
	tr.touch("old-2")
	tr.touch("old-3")
	tr.touch("fresh")
	firedAfterTouches := fired

	rewind := time.Now().UTC().Add(-time.Hour)
	tr.mu.Lock()
	tr.sessions["old-1"] = rewind
	tr.sessions["old-2"] = rewind
	tr.sessions["old-3"] = rewind
	tr.mu.Unlock()

	evicted := tr.sweep()
	if evicted != 3 {
		t.Errorf("sweep evicted %d, want 3", evicted)
	}
	if got := tr.size(); got != 1 {
		t.Errorf("size after sweep: got %d, want 1 (only 'fresh' should remain)", got)
	}
	// Single onChange observation per sweep — fired exactly once on top
	// of the touch-time observations.
	if fired != firedAfterTouches+1 {
		t.Errorf("sweep onChange: got %d total fires (was %d after touches), want exactly one more",
			fired, firedAfterTouches)
	}

	// Sweep again with nothing stale → no fire, no eviction.
	prev := fired
	if got := tr.sweep(); got != 0 {
		t.Errorf("idle sweep evicted %d, want 0", got)
	}
	if fired != prev {
		t.Errorf("idle sweep must not fire onChange; got %d additional fires", fired-prev)
	}
}

// TestMCPSessionTracker_NilOnChangeIsSafe verifies the tracker works
// without an onChange callback (used by tests / non-metrics builds).
func TestMCPSessionTracker_NilOnChangeIsSafe(t *testing.T) {
	tr := newMCPSessionTracker(time.Minute, nil)
	tr.touch("a")
	tr.touch("b")
	tr.evict("a")
	tr.sweep() // no panic
	if got := tr.size(); got != 1 {
		t.Errorf("size: got %d, want 1", got)
	}
}

// TestMCPSessionTracker_ConcurrentTouchEvict exercises the mutex
// under concurrent load. Run with -race in CI; the assertion here
// is just that the final size is what the call counts say it should
// be.
func TestMCPSessionTracker_ConcurrentTouchEvict(t *testing.T) {
	tr := newMCPSessionTracker(time.Minute, nil)

	const goroutines = 16
	const iterations = 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			id := "sess-" + string(rune('A'+g))
			for i := 0; i < iterations; i++ {
				tr.touch(id)
				if i%4 == 0 {
					tr.evict(id)
				}
			}
		}(g)
	}
	wg.Wait()

	// Every goroutine ends with at least one final touch (i loops
	// to 199; 199 % 4 != 0), so each id should be present.
	if got := tr.size(); got != goroutines {
		t.Errorf("size after concurrent ops: got %d, want %d", got, goroutines)
	}
}

// TestMCPSessionTracker_OnChangeUnderLock pins the Codex round-1 fix
// for the race where onChange ran AFTER the mutex was released:
// concurrent touches could compute (n=1, n=2) under the lock then
// race the callback writes, leaving the gauge at 1 while the map
// holds 2 entries (last writer wins on the gauge but loses the
// observation order).
//
// We assert by recording (size, observation) pairs from inside the
// callback. With the lock held across onChange, every observation
// matches the actual map size at observation time — no `recorded[i] <
// recorded[i-1]` after a sequence of pure inserts.
func TestMCPSessionTracker_OnChangeUnderLock(t *testing.T) {
	const goroutines = 32
	const opsPerGoroutine = 50

	var (
		mu       sync.Mutex
		observed []int
	)
	tr := newMCPSessionTracker(time.Hour, func(n int) {
		mu.Lock()
		observed = append(observed, n)
		mu.Unlock()
	})

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			// Each goroutine touches a unique id repeatedly. Only the
			// first touch fires onChange; the rest are refreshes.
			id := "sess-touch-" + string(rune('A'+g))
			for i := 0; i < opsPerGoroutine; i++ {
				tr.touch(id)
			}
		}(g)
	}
	wg.Wait()

	// Exactly `goroutines` onChange invocations (one per first-touch).
	if got := len(observed); got != goroutines {
		t.Fatalf("onChange invocations: got %d, want %d", got, goroutines)
	}

	// With the lock held, observations form a strictly monotonic
	// sequence 1, 2, 3, ... goroutines (one per insert, no reorder).
	// Without the fix, a race would let observations land out of order
	// and this assertion would fail intermittently under -race.
	for i, n := range observed {
		want := i + 1
		if n != want {
			t.Errorf("observed[%d] = %d, want %d (sequence: %v)", i, n, want, observed)
			break
		}
	}

	// Final map size matches the last observation.
	if got := tr.size(); got != goroutines {
		t.Errorf("final size: got %d, want %d", got, goroutines)
	}
	if observed[len(observed)-1] != goroutines {
		t.Errorf("last observation: got %d, want %d", observed[len(observed)-1], goroutines)
	}
}

// TestMCPSessionTracker_RunStopsCleanly verifies the run() loop
// exits promptly after shutdown(). Uses a tight sweep interval so
// the goroutine has cycled at least once before we stop it.
func TestMCPSessionTracker_RunStopsCleanly(t *testing.T) {
	tr := newMCPSessionTracker(time.Minute, nil)

	done := make(chan struct{})
	go func() {
		tr.run(5 * time.Millisecond)
		close(done)
	}()

	// Let the loop tick at least once.
	time.Sleep(20 * time.Millisecond)
	tr.shutdown()

	select {
	case <-done:
		// expected
	case <-time.After(time.Second):
		t.Fatal("run() did not exit within 1s of shutdown()")
	}

	// Idempotent — calling shutdown twice must not panic on a
	// double channel close.
	tr.shutdown()
}

// TestServer_TrackMCPSession_LifecycleHappyPath covers the
// integration seam the audit middleware uses: trackMCPSession
// pulls the session-id from the response header (initialize) or
// request header (subsequent calls), touches on success, evicts on
// DELETE.
func TestServer_TrackMCPSession_LifecycleHappyPath(t *testing.T) {
	s := &Server{metrics: metrics.New()}
	s.startMCPSessionTracker()
	defer s.stopMCPSessionTracker()

	// Helper to build closure-based header readers from a map.
	header := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	// 1. initialize → response carries the session-id, request doesn't.
	s.trackMCPSession(
		header(map[string]string{}),
		header(map[string]string{"Mcp-Session-Id": "sess-123"}),
		"POST",
		200,
	)
	if got := gaugeValueOrZero(t, s.metrics.MCPActiveSessions); got != 1 {
		t.Errorf("after initialize: gauge=%v, want 1", got)
	}

	// 2. Subsequent tool call → request carries the echoed session-id.
	s.trackMCPSession(
		header(map[string]string{"Mcp-Session-Id": "sess-123"}),
		header(map[string]string{}),
		"POST",
		200,
	)
	if got := gaugeValueOrZero(t, s.metrics.MCPActiveSessions); got != 1 {
		t.Errorf("after tool call: gauge=%v, want 1 (unchanged — same session)", got)
	}

	// 3. DELETE → evicts.
	s.trackMCPSession(
		header(map[string]string{"Mcp-Session-Id": "sess-123"}),
		header(map[string]string{}),
		"DELETE",
		200,
	)
	if got := gaugeValueOrZero(t, s.metrics.MCPActiveSessions); got != 0 {
		t.Errorf("after DELETE: gauge=%v, want 0", got)
	}
}

// TestServer_TrackMCPSession_FailedInitializeDoesNotOpen guards
// against the bug the original v1 logic also tried to handle: a 500
// on initialize must NOT register a session.
func TestServer_TrackMCPSession_FailedInitializeDoesNotOpen(t *testing.T) {
	s := &Server{metrics: metrics.New()}
	s.startMCPSessionTracker()
	defer s.stopMCPSessionTracker()

	noHeader := func(string) string { return "" }
	withHeader := func(v string) func(string) string {
		return func(k string) string {
			if k == "Mcp-Session-Id" {
				return v
			}
			return ""
		}
	}

	// 5xx on initialize: tracker must skip even though a session-id
	// would be present in the response (mcp-go may have set one
	// before the handler errored).
	s.trackMCPSession(noHeader, withHeader("sess-fail"), "POST", 500)
	if got := gaugeValueOrZero(t, s.metrics.MCPActiveSessions); got != 0 {
		t.Errorf("after failed initialize: gauge=%v, want 0", got)
	}
}

// TestServer_TrackMCPSession_NoIDIsNoOp verifies a request with no
// Mcp-Session-Id in either header doesn't churn the tracker. Covers
// the corner where the inner handler returned an error before
// mcp-go got to set the response header.
func TestServer_TrackMCPSession_NoIDIsNoOp(t *testing.T) {
	s := &Server{metrics: metrics.New()}
	s.startMCPSessionTracker()
	defer s.stopMCPSessionTracker()

	noHeader := func(string) string { return "" }
	s.trackMCPSession(noHeader, noHeader, "POST", 200)

	if s.mcpSessions.size() != 0 {
		t.Errorf("no-id call must not insert; tracker size=%d", s.mcpSessions.size())
	}
}

// TestServer_TrackMCPSession_NilTrackerIsSafe verifies the helper is
// a clean no-op on Servers that never wired the tracker (selfhost /
// tests).
func TestServer_TrackMCPSession_NilTrackerIsSafe(t *testing.T) {
	s := &Server{metrics: metrics.New()}
	// Intentionally NOT calling startMCPSessionTracker.
	noHeader := func(string) string { return "" }
	s.trackMCPSession(noHeader, noHeader, "POST", 200) // no panic
}

// TestServer_StartMCPSessionTracker_Idempotent guards the
// "called twice from a test that flips MCP transport state" path.
// First call wires the tracker; second is a no-op.
func TestServer_StartMCPSessionTracker_Idempotent(t *testing.T) {
	s := &Server{metrics: metrics.New()}
	s.startMCPSessionTracker()
	tracker1 := s.mcpSessions
	s.startMCPSessionTracker()
	if s.mcpSessions != tracker1 {
		t.Error("second startMCPSessionTracker re-created the tracker (expected no-op)")
	}
	s.stopMCPSessionTracker()
	s.bg.Wait()
}

// TestServer_TrackMCPSession_DeleteEvictsOnAnyStatus pins the spec
// behavior: the client considers the session over once it sends
// DELETE, so the server should evict regardless of the response
// status. Otherwise a transient 5xx on the DELETE path would leave
// the session pinned in the gauge until TTL eviction.
func TestServer_TrackMCPSession_DeleteEvictsOnAnyStatus(t *testing.T) {
	s := &Server{metrics: metrics.New()}
	s.startMCPSessionTracker()
	defer s.stopMCPSessionTracker()

	withHeader := func(v string) func(string) string {
		return func(k string) string {
			if k == "Mcp-Session-Id" {
				return v
			}
			return ""
		}
	}

	// Open a session.
	s.trackMCPSession(func(string) string { return "" }, withHeader("sess-x"), "POST", 200)
	if got := gaugeValueOrZero(t, s.metrics.MCPActiveSessions); got != 1 {
		t.Fatalf("setup: want 1, got %v", got)
	}

	// DELETE that came back 500 — still evicts.
	s.trackMCPSession(withHeader("sess-x"), func(string) string { return "" }, "DELETE", 500)
	if got := gaugeValueOrZero(t, s.metrics.MCPActiveSessions); got != 0 {
		t.Errorf("after failed DELETE: gauge=%v, want 0 (evict on any DELETE status)", got)
	}
}
