package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"golang.org/x/time/rate"

	"github.com/PerpetualSoftware/pad/internal/metrics"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3408 (U10c): per-endpoint delivery state, the owner no-duplicate
// ledger, the per-install rate cap and the 24 h drop.

// ownerSink is an owner webhook endpoint answering status, counting hits.
func ownerSink(t *testing.T, e *deliveryEnv, status *atomic.Int32) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(srv.Close)
	if _, err := e.srv.store.CreateWebhook(e.wsID, models.WebhookCreate{URL: srv.URL, Events: `["*"]`}); err != nil {
		t.Fatal(err)
	}
	return &hits
}

// An app held back by its rate cap for several passes causes exactly ONE
// delivery to an owner hook that succeeded (lead ruling, day 87).
func TestAppDelivery_RateLimitedAppDoesNotDuplicateOwnerDeliveries(t *testing.T) {
	e := newDeliveryEnv(t)
	e.redeemNow(t)
	var ok atomic.Int32
	ok.Store(http.StatusNoContent)
	owner := ownerSink(t, e, &ok)

	e.srv.appRate.mu.Lock()
	e.srv.appRate.by, e.srv.appRate.limit, e.srv.appRate.burst = map[string]*rate.Limiter{}, 0, 0 // never allows
	e.srv.appRate.mu.Unlock()

	e.item(t, e.companion.ID, "Busy app")
	for i := 0; i < 3; i++ {
		e.tick(t)
	}
	if n := owner.Load(); n != 1 {
		t.Fatalf("owner endpoint received %d deliveries while the app was rate-limited, want 1", n)
	}
	if got := e.hooksAt("/hooks"); len(got) != 0 {
		t.Fatalf("a rate-limited app received %d deliveries", len(got))
	}
	if e.pendingOutbox(t) == 0 {
		t.Fatal("a rate-limited app delivery did not keep the event owed")
	}
	// Deferred, not attempted: three passes charge no attempt.
	var status string
	var attempts int
	if err := e.srv.store.DB().QueryRow(`SELECT d.status, d.attempts FROM webhook_deliveries d JOIN webhooks w ON w.id = d.webhook_id WHERE w.app_install_id = ?`, e.installID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "rate_limited" || attempts != 0 {
		t.Fatalf("app row %s/%d, want rate_limited/0", status, attempts)
	}

	// The cap lifts: the app gets it once, the owner still once, and the
	// event is acked.
	e.srv.appRate.mu.Lock()
	e.srv.appRate.by, e.srv.appRate.limit, e.srv.appRate.burst = map[string]*rate.Limiter{}, rate.Inf, 1
	e.srv.appRate.mu.Unlock()
	e.tick(t)
	e.tick(t)
	if got := e.hooksAt("/hooks"); len(got) != 1 {
		t.Fatalf("%d app deliveries after the cap lifted, want 1", len(got))
	}
	if n := owner.Load(); n != 1 {
		t.Fatalf("owner endpoint received %d deliveries, want 1", n)
	}
	if n := e.pendingOutbox(t); n != 0 {
		t.Fatalf("%d events still owed", n)
	}
}

// An owner hook that FAILED is retried as today, while an app endpoint that
// already received the event is not sent it again.
func TestAppDelivery_FailingOwnerRetriedAppNotResent(t *testing.T) {
	e := newDeliveryEnv(t)
	e.redeemNow(t)
	var down atomic.Int32
	down.Store(http.StatusServiceUnavailable)
	owner := ownerSink(t, e, &down)

	e.item(t, e.companion.ID, "Owner down")
	e.tick(t)
	first := owner.Load()
	e.tick(t)
	if owner.Load() <= first || first == 0 {
		t.Fatalf("a failing owner hook was not retried (%d then %d hits)", first, owner.Load())
	}
	if got := e.hooksAt("/hooks"); len(got) != 1 {
		t.Fatalf("the app received %d deliveries across the owner's retries, want 1", len(got))
	}
}

// An app endpoint that failed transiently is retried, and once delivered is
// done.
func TestAppDelivery_TransientAppRetriedUntilDelivered(t *testing.T) {
	e := newDeliveryEnv(t)
	e.redeemNow(t)
	e.status["/hooks"] = http.StatusServiceUnavailable
	e.item(t, e.companion.ID, "Flaky app")
	e.tick(t)
	if e.pendingOutbox(t) == 0 {
		t.Fatal("a transient app failure acked the event")
	}
	delete(e.status, "/hooks")
	e.tick(t)
	before := len(e.hooksAt("/hooks"))
	e.tick(t)
	if len(e.hooksAt("/hooks")) != before {
		t.Fatal("a delivered app endpoint was sent the event again")
	}
	if n := e.pendingOutbox(t); n != 0 {
		t.Fatalf("%d events owed after delivery", n)
	}
}

// Undelivered after 24 h: dropped, counted on the hook and shown to the
// owner, never sent; the event is no longer owed to the app.
func TestAppDelivery_DroppedAfter24Hours(t *testing.T) {
	e := newDeliveryEnv(t)
	e.redeemNow(t)
	e.item(t, e.companion.ID, "Old")
	db := e.srv.store.DB()
	// Owed (the hook was deliverable then), and older than 24 h.
	if _, err := db.Exec(`UPDATE webhooks SET deliver_from = '1999-01-01T00:00:00Z' WHERE app_install_id = ?`, e.installID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE event_outbox SET occurred_at = '2000-01-01T00:00:00Z' WHERE dispatched_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	e.tick(t)
	if got := e.hooksAt("/hooks"); len(got) != 0 {
		t.Fatalf("a day-old event was delivered: %s", got[0].Body)
	}
	if n := e.pendingOutbox(t); n != 0 {
		t.Fatalf("%d events owed after the drop", n)
	}
	got := e.getInstall(t)
	if got.Webhook == nil || got.Webhook.UndeliveredDropped != 1 {
		t.Fatalf("install view: %+v, want undelivered_dropped 1", got.Webhook)
	}
	// A second pass does not count it again.
	e.tick(t)
	if got := e.getInstall(t); got.Webhook.UndeliveredDropped != 1 {
		t.Fatalf("undelivered_dropped %d after a second pass", got.Webhook.UndeliveredDropped)
	}
}

func TestAppDelivery_DropBoundary(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		age  time.Duration
		want bool
	}{{23 * time.Hour, false}, {24*time.Hour - time.Second, false}, {24*time.Hour + time.Second, true}, {25 * time.Hour, true}} {
		if got := appDeliveryExpired(now.Add(-tc.age).Format(time.RFC3339), now); got != tc.want {
			t.Errorf("age %v: expired %v, want %v", tc.age, got, tc.want)
		}
	}
	if appDeliveryExpired("not a time", now) {
		t.Error("an unparseable occurred_at was treated as expired")
	}
}

// A skipped event (no projection) is recorded, so an owner hook's retry
// does not count it again.
func TestAppDelivery_SkipIsCountedOnce(t *testing.T) {
	e := newDeliveryEnv(t)
	m := metrics.New()
	e.srv.SetMetrics(m)
	e.redeemNow(t)
	var down atomic.Int32
	down.Store(http.StatusServiceUnavailable)
	ownerSink(t, e, &down)
	e.item(t, e.companion.ID, "No block")
	db := e.srv.store.DB()
	if _, err := db.Exec(`UPDATE event_outbox SET payload = '{"id":"x","title":"t"}' WHERE dispatched_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	e.tick(t)
	e.tick(t)
	var got dto.Metric
	if err := m.AppWebhookDeliveriesTotal.WithLabelValues("skipped_no_projection").Write(&got); err != nil {
		t.Fatal(err)
	}
	if n := got.GetCounter().GetValue(); n != 1 {
		t.Fatalf("skipped_no_projection = %v across an owner retry, want 1", n)
	}
}

func (e *deliveryEnv) setAppRate(t *testing.T, limit rate.Limit, burst int) {
	t.Helper()
	e.srv.appRate.mu.Lock()
	e.srv.appRate.by, e.srv.appRate.limit, e.srv.appRate.burst = map[string]*rate.Limiter{}, limit, burst
	e.srv.appRate.mu.Unlock()
}

// codex r1 on U10c: an event in a collection the app is not subscribed to
// spends none of its rate budget.
func TestAppDelivery_UnrelatedCollectionsSpendNoBudget(t *testing.T) {
	e := newDeliveryEnv(t)
	e.redeemNow(t)
	e.setAppRate(t, 0, 1) // one token, never refilled
	other, err := e.srv.store.CreateCollection(e.wsID, models.CollectionCreate{Name: "Internal", Slug: "internal"})
	if err != nil {
		t.Fatal(err)
	}
	e.item(t, other.ID, "Not the app's")
	e.tick(t)
	e.item(t, e.companion.ID, "The app's")
	e.tick(t)
	if got := e.hooksAt("/hooks"); len(got) != 1 {
		t.Fatalf("%d deliveries: an unrelated event spent the app's only token", len(got))
	}
}

// codex r1 on U10c: the cap bounds REQUESTS, retries included.
func TestAppDelivery_RateCapCountsEveryAttempt(t *testing.T) {
	e := newDeliveryEnv(t)
	e.redeemNow(t)
	e.setAppRate(t, 0, 2) // two requests, never refilled
	e.status["/hooks"] = http.StatusServiceUnavailable
	e.item(t, e.companion.ID, "Flaky")
	e.tick(t)
	if got := len(e.hooksAt("/hooks")); got != 2 {
		t.Fatalf("%d requests against a budget of 2", got)
	}
	if e.pendingOutbox(t) == 0 {
		t.Fatal("the event was acked while still owed")
	}
}

// codex r1 on U10c: the limiter map does not grow without bound, and pruning
// drops only idle (full-bucket) entries.
func TestAppRateLimiter_PrunesIdle(t *testing.T) {
	l := appRateLimiter{by: map[string]*rate.Limiter{}, limit: 10, burst: 60}
	busy := rate.NewLimiter(l.limit, l.burst)
	busy.AllowN(time.Now(), 60) // spent: not idle
	l.by["busy"] = busy
	for i := 0; len(l.by) < appRateLimiterPruneAt; i++ {
		l.by[fmt.Sprintf("idle-%d", i)] = rate.NewLimiter(l.limit, l.burst) // full: idle
	}
	l.allow("new")
	if len(l.by) > 2 {
		t.Fatalf("%d limiters retained after pruning, want busy and new", len(l.by))
	}
	if _, ok := l.by["busy"]; !ok {
		t.Fatal("pruning dropped a limiter that had spent tokens")
	}
}

// codex r1 on U10c: a bulk skip is recorded, so an owner hook's retry of
// the same unit does not count it again.
func TestAppDelivery_BulkSkipCountedOnce(t *testing.T) {
	e := newDeliveryEnv(t)
	m := metrics.New()
	e.srv.SetMetrics(m)
	e.redeemNow(t)
	var down atomic.Int32
	down.Store(http.StatusServiceUnavailable)
	ownerSink(t, e, &down)
	it := e.item(t, e.companion.ID, "Member")
	e.tick(t)
	title := "Renamed in bulk"
	if _, err := e.srv.store.UpdateItem(it.ID, models.ItemUpdate{Title: &title}, store.WithEventBatch("batch-once")); err != nil {
		t.Fatal(err)
	}
	e.tick(t)
	e.tick(t)
	var got dto.Metric
	if err := m.AppWebhookDeliveriesTotal.WithLabelValues("skipped_bulk").Write(&got); err != nil {
		t.Fatal(err)
	}
	if n := got.GetCounter().GetValue(); n != 1 {
		t.Fatalf("skipped_bulk = %v across an owner retry, want 1", n)
	}
}

// codex r2 on U10c: an old event the app was never owed (before its
// deliver_from) is refused, not dropped and counted.
func TestAppDelivery_NotOwedIsNotDropped(t *testing.T) {
	e := newDeliveryEnv(t)
	e.redeemNow(t)
	e.item(t, e.companion.ID, "Before the hook")
	if _, err := e.srv.store.DB().Exec(`UPDATE event_outbox SET occurred_at = '2000-01-01T00:00:00Z' WHERE dispatched_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	e.tick(t)
	if got := e.getInstall(t); got.Webhook.UndeliveredDropped != 0 {
		t.Fatalf("undelivered_dropped %d for an event the app was never owed", got.Webhook.UndeliveredDropped)
	}
	if n := e.pendingOutbox(t); n != 0 {
		t.Fatalf("%d events owed", n)
	}
}

// codex r2 on U10c: an attempt admission refuses spends no rate budget.
func TestAppDelivery_RefusalsSpendNoBudget(t *testing.T) {
	e := newDeliveryEnv(t)
	var down atomic.Int32
	down.Store(http.StatusServiceUnavailable)
	ownerSink(t, e, &down)
	e.item(t, e.companion.ID, "While held") // pending: the owner is down
	e.tick(t)
	time.Sleep(1100 * time.Millisecond)
	e.redeemNow(t)
	e.setAppRate(t, 0, 1) // one token
	e.item(t, e.companion.ID, "After redeem")
	e.tick(t)
	if got := e.hooksAt("/hooks"); len(got) != 1 {
		t.Fatalf("%d deliveries: the refused old event spent the only token", len(got))
	}
}

// codex r2 on U10c: the ledger counts requests actually sent.
func TestAppDelivery_LedgerCountsRequests(t *testing.T) {
	e := newDeliveryEnv(t)
	e.redeemNow(t)
	e.status["/hooks"] = http.StatusServiceUnavailable
	e.item(t, e.companion.ID, "Three tries")
	e.tick(t)
	attempts := func() (string, int) {
		var st string
		var n int
		if err := e.srv.store.DB().QueryRow(`SELECT d.status, d.attempts FROM webhook_deliveries d JOIN webhooks w ON w.id = d.webhook_id WHERE w.app_install_id = ?`, e.installID).Scan(&st, &n); err != nil {
			t.Fatal(err)
		}
		return st, n
	}
	if st, n := attempts(); st != "transient" || n != 3 {
		t.Fatalf("ledger %s/%d after three 503s, want transient/3", st, n)
	}
	// One more request, then the cap.
	e.setAppRate(t, 0, 1)
	e.tick(t)
	if st, n := attempts(); st != "rate_limited" || n != 4 {
		t.Fatalf("ledger %s/%d, want rate_limited/4 (the request sent before the cap counts)", st, n)
	}
}
