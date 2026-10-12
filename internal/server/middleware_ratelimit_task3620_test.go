package server

import (
	"fmt"
	"net/http"
	"testing"
)

// TASK-3620: the health probe has its own per-address bucket. Refill is
// frozen, so each count below is the bucket's burst. The search burst (30) is
// pinned by TestRateLimit_SearchEndpointLimited.

// countUntil429 sends GET path from remote up to max times and returns how
// many were answered before the first 429 (max if none was).
func countUntil429(srv *Server, path, remote string, max int) int {
	for i := 0; i < max; i++ {
		if rl3310Request(srv, path, remote, "") == http.StatusTooManyRequests {
			return i
		}
	}
	return max
}

func TestRateLimit_HealthHasItsOwnBoundedBucket(t *testing.T) {
	srv := testServer(t)
	freezeRefill(t, srv)
	const addr = "192.0.2.70:1"

	// The general API bucket, exhausted, does not refuse the probe...
	rl3310Exhaust(t, srv, "/api/v1/workspaces", addr, "")
	if code := rl3310Request(srv, "/api/v1/health", addr, ""); code == http.StatusTooManyRequests {
		t.Fatal("/api/v1/health got 429 after the API bucket was exhausted: it still shares that bucket")
	}

	// ...and the probe's own bucket is generous but bounded: one probe was
	// spent above, so 299 more pass and the next is refused.
	if got := countUntil429(srv, "/api/v1/health", addr, 400); got != 299 {
		t.Fatalf("health probes answered before the first 429 = %d, want 299 (burst 300, one spent)", got)
	}
	// Each sibling route draws on the same bucket: from a fresh address whose
	// API bucket is unspent, the burst is the health one (300, not the API's
	// 60), and the API bucket is still untouched afterwards.
	for i, sibling := range []string{"/api/v1/health/live", "/api/v1/health/ready"} {
		fresh := fmt.Sprintf("192.0.2.%d:1", 80+i)
		if got := countUntil429(srv, sibling, fresh, 400); got != 300 {
			t.Errorf("%s answered %d before the first 429, want 300: it is not in the health bucket", sibling, got)
		}
		if code := rl3310Request(srv, "/api/v1/workspaces", fresh, ""); code == http.StatusTooManyRequests {
			t.Errorf("an API request got 429 after only %s probes: they spent the API bucket", sibling)
		}
	}
	// Per address: another address still probes.
	if code := rl3310Request(srv, "/api/v1/health", "192.0.2.71:1", ""); code == http.StatusTooManyRequests {
		t.Error("a probe from another address got 429")
	}
}

func TestRateLimit_HealthProbesDoNotSpendTheAPIBucket(t *testing.T) {
	srv := testServer(t)
	freezeRefill(t, srv)
	const addr = "192.0.2.72:1"

	countUntil429(srv, "/api/v1/health", addr, 300)
	if code := rl3310Request(srv, "/api/v1/workspaces", addr, ""); code == http.StatusTooManyRequests {
		t.Fatal("an API request got 429 after only health probes: the probes spent the API bucket")
	}
}
