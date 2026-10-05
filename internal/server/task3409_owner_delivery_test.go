package server

import (
	"net/http"
	"sync/atomic"
	"testing"
)

// TASK-3409: owner hooks keep per-endpoint delivery state. A retry goes only
// to endpoints still owed: not to one that accepted the event, and not to
// one that permanently refused it. A transient endpoint is retried and its
// attempts and last error are recorded.
func TestTask3409_ARetryGoesOnlyToOwnerEndpointsStillOwed(t *testing.T) {
	e := newDeliveryEnv(t)
	var refuses, flaky, healthy atomic.Int32
	refuses.Store(http.StatusNotFound)
	flaky.Store(http.StatusServiceUnavailable)
	healthy.Store(http.StatusNoContent)
	hitsRefuses := ownerSink(t, e, &refuses)
	hitsFlaky := ownerSink(t, e, &flaky)
	hitsHealthy := ownerSink(t, e, &healthy)

	e.item(t, e.companion.ID, "Three endpoints")
	e.tick(t)
	if hitsRefuses.Load() != 1 || hitsHealthy.Load() != 1 || hitsFlaky.Load() == 0 {
		t.Fatalf("first pass: refuses %d, healthy %d, flaky %d", hitsRefuses.Load(), hitsHealthy.Load(), hitsFlaky.Load())
	}
	if e.pendingOutbox(t) == 0 {
		t.Fatal("a transient owner endpoint acked the event")
	}
	var status string
	var attempts int
	var lastErr *string
	if err := e.srv.store.DB().QueryRow(`SELECT d.status, d.attempts, d.last_error FROM webhook_deliveries d JOIN webhooks w ON w.id = d.webhook_id
		WHERE w.app_install_id IS NULL AND d.status = 'transient'`).Scan(&status, &attempts, &lastErr); err != nil {
		t.Fatalf("the transient endpoint has no row: %v", err)
	}
	if attempts != int(hitsFlaky.Load()) || lastErr == nil || *lastErr == "" {
		t.Errorf("transient row attempts %d (hits %d), last_error %v", attempts, hitsFlaky.Load(), lastErr)
	}

	// Second pass, still failing: only the flaky endpoint is sent it again.
	before := hitsFlaky.Load()
	e.tick(t)
	if hitsRefuses.Load() != 1 {
		t.Errorf("a permanently refusing endpoint was sent the event again (%d hits)", hitsRefuses.Load())
	}
	if hitsHealthy.Load() != 1 {
		t.Errorf("an endpoint that accepted was sent the event again (%d hits)", hitsHealthy.Load())
	}
	if hitsFlaky.Load() <= before {
		t.Error("the transient endpoint was not retried")
	}

	// It recovers: the event is acked, and nothing more goes anywhere.
	flaky.Store(http.StatusNoContent)
	e.tick(t)
	if n := e.pendingOutbox(t); n != 0 {
		t.Fatalf("%d events owed after every endpoint was decided", n)
	}
	settled := hitsFlaky.Load()
	e.tick(t)
	if hitsFlaky.Load() != settled || hitsRefuses.Load() != 1 || hitsHealthy.Load() != 1 {
		t.Error("a decided endpoint was sent the event after the ack")
	}
}

// Parity: a single owner endpoint behaves as before, apart from the record.
// A success acks at once; a 429 is retried as transient (TASK-3409).
func TestTask3409_ASingleOwnerEndpointBehavesAsBefore(t *testing.T) {
	e := newDeliveryEnv(t)
	var st atomic.Int32
	st.Store(http.StatusTooManyRequests)
	hits := ownerSink(t, e, &st)
	e.item(t, e.companion.ID, "One endpoint")
	e.tick(t)
	if e.pendingOutbox(t) == 0 {
		t.Fatal("a 429 acked the event")
	}
	st.Store(http.StatusNoContent)
	e.tick(t)
	if n := e.pendingOutbox(t); n != 0 {
		t.Fatalf("%d events owed after success", n)
	}
	if hits.Load() < 2 {
		t.Errorf("%d hits; the 429 was not retried", hits.Load())
	}
}
