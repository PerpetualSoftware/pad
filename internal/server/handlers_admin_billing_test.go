package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/billing"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// fakeBillingSidecar is a controllable CloudSidecar used by the
// handleAdminBillingStats tests. Lets each case decide whether
// GetBillingMetrics returns a payload, a transport error, or a
// *billing.SidecarError, without standing up a real HTTP server.
type fakeBillingSidecar struct {
	calls    atomic.Int32
	response *billing.BillingMetricsResponse
	err      error
}

func (f *fakeBillingSidecar) CancelCustomer(string) error {
	return nil // unused by these tests
}

func (f *fakeBillingSidecar) GetBillingMetrics() (*billing.BillingMetricsResponse, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.response, nil
}

// adminBillingReq fires GET /api/v1/admin/billing-stats with the cookie
// from a logged-in user. Returns the recorder so callers can inspect status
// + body. Pinned to 127.0.0.1 to match the bootstrap session's IP and
// avoid the session-binding-changed audit-row write that would otherwise
// fire on a different RemoteAddr.
func adminBillingReq(srv *Server, token string) *httptest.ResponseRecorder {
	return doRequestWithCookieFrom(srv, http.MethodGet, "/api/v1/admin/billing-stats",
		nil, token, "127.0.0.1:5555")
}

// seedUser inserts a user directly with the given plan + created_at so
// tests can deterministically populate the customers_by_plan and
// new_signups_30d aggregates without round-tripping the registration flow.
// Uses the test sqlite backend's raw `?` placeholders — these tests don't
// run against postgres.
//
// Times are pre-formatted as RFC3339 strings to match the store's
// formatting helper; passing a time.Time directly would let the driver
// pick its own format (nano-precision RFC3339Nano), which the store's
// parseTime() can't read back, leaving CreatedAt as a zero value and
// silently breaking new_signups_30d filtering.
func seedUser(t *testing.T, srv *Server, email, plan string, createdAt time.Time) {
	t.Helper()
	id := newSeedID(email)
	username := "u_" + id // full ID for guaranteed uniqueness
	ts := createdAt.UTC().Format(time.RFC3339)
	_, err := srv.store.DB().Exec(
		`INSERT INTO users
			(id, email, username, name, password_hash, role, plan, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, email, username, "Seed "+id, "x", "member", plan, ts, ts)
	if err != nil {
		t.Fatalf("seed user %s plan=%s: %v", email, plan, err)
	}
}

// newSeedID is a stable test-only ID derived from the email. Used as both
// the user.id AND the unique-per-row component of user.username, so
// collisions are impossible across distinct emails.
func newSeedID(email string) string {
	h := uint64(0)
	for _, c := range email {
		h = h*31 + uint64(c)
	}
	return "seed_" + strconvUint(h)
}

// strconvUint is a tiny strconv.FormatUint replacement that avoids the
// import dance for a one-line helper.
func strconvUint(u uint64) string {
	if u == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	return string(buf[i:])
}

func decodeBillingStats(t *testing.T, rr *httptest.ResponseRecorder) AdminBillingStatsResponse {
	t.Helper()
	var resp AdminBillingStatsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	return resp
}

// --- Tests ---

func TestAdminBillingStats_SelfHostReturns404(t *testing.T) {
	// Outside cloud mode the route group is gated by requireCloudMode and
	// must disappear entirely from an authenticated admin's view — no
	// "feature unavailable" disclosure, just 404 like any unrelated path.
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	// Cloud mode deliberately NOT set.

	rr := adminBillingReq(srv, adminToken)
	if rr.Code != http.StatusNotFound {
		t.Errorf("self-host: want 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminBillingStats_NonAdminReturns403(t *testing.T) {
	srv := testServer(t)
	// Bootstrap MUST happen before SetCloudMode — bootstrap is disabled
	// in cloud mode (registration there flows through OAuth/invitations).
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	srv.SetCloudMode("cloud-secret")

	memberToken := registerNonAdmin(t, srv, "member@test.com", "Member")

	rr := adminBillingReq(srv, memberToken)
	if rr.Code != http.StatusForbidden {
		t.Errorf("non-admin: want 403, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminBillingStats_AdminWithSidecarReachable_MergesLocalAndRemote(t *testing.T) {
	srv := testServer(t)
	now := time.Now().UTC()
	recent := now.Add(-5 * 24 * time.Hour)
	old := now.Add(-90 * 24 * time.Hour)

	// Bootstrap before SetCloudMode (bootstrap is disabled in cloud mode).
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	srv.SetCloudMode("cloud-secret")

	// Seed local users for the customers_by_plan and new_signups_30d
	// aggregates. The bootstrapped admin is plan="" → counted as "free" by
	// the handler, so we account for it explicitly below.
	seedUser(t, srv, "free1@test.com", "free", recent)
	seedUser(t, srv, "free2@test.com", "free", old)
	seedUser(t, srv, "pro1@test.com", "pro", recent) // counts as new_signup_30d
	seedUser(t, srv, "pro2@test.com", "pro", recent) // counts as new_signup_30d
	seedUser(t, srv, "pro_old@test.com", "pro", old) // pro but outside 30d window
	seedUser(t, srv, "self@test.com", "self-hosted", old)

	srv.SetCloudSidecar(&fakeBillingSidecar{
		response: &billing.BillingMetricsResponse{
			StripeConfigured:    true,
			ActiveSubscriptions: 7,
			MRRCents:            49000,
			ARRCents:            588000,
			Currency:            "usd",
			ChurnRate30d:        0.05,
			Cancelled30d:        2,
			ComputedAt:          now,
			CacheAgeSeconds:     12,
		},
	})

	rr := adminBillingReq(srv, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeBillingStats(t, rr)

	if resp.CloudUnreachable {
		t.Errorf("cloud_unreachable: want false on healthy sidecar")
	}
	if !resp.StripeConfigured {
		t.Errorf("stripe_configured: want true")
	}

	// Local: 2 free seeded + admin (free) = 3; 3 pro; 1 self-hosted.
	if got, want := resp.CustomersByPlan["free"], 3; got != want {
		t.Errorf("customers_by_plan[free]: want %d, got %d", want, got)
	}
	if got, want := resp.CustomersByPlan["pro"], 3; got != want {
		t.Errorf("customers_by_plan[pro]: want %d, got %d", want, got)
	}
	if got, want := resp.CustomersByPlan["self-hosted"], 1; got != want {
		t.Errorf("customers_by_plan[self-hosted]: want %d, got %d", want, got)
	}
	// Only pro1 + pro2 are inside the 30-day window (pro_old is too old).
	if got, want := resp.NewSignups30d, 2; got != want {
		t.Errorf("new_signups_30d: want %d (pro1+pro2), got %d", want, got)
	}

	// Remote fields verbatim from the fake sidecar.
	if resp.ActiveSubscriptions != 7 {
		t.Errorf("active_subscriptions: want 7, got %d", resp.ActiveSubscriptions)
	}
	if resp.MRRCents != 49000 {
		t.Errorf("mrr_cents: want 49000, got %d", resp.MRRCents)
	}
	if resp.ARRCents != 588000 {
		t.Errorf("arr_cents: want 588000, got %d", resp.ARRCents)
	}
	if resp.Currency != "usd" {
		t.Errorf("currency: want usd, got %s", resp.Currency)
	}
	if resp.Cancelled30d != 2 {
		t.Errorf("cancelled_30d: want 2, got %d", resp.Cancelled30d)
	}
	if resp.CacheAgeSeconds != 12 {
		t.Errorf("cache_age_seconds: want 12, got %d", resp.CacheAgeSeconds)
	}
}

func TestAdminBillingStats_NoSidecarConfigured_DegradesToLocalOnly(t *testing.T) {
	// Cloud mode is on but PAD_CLOUD_SIDECAR_URL was never set, so
	// cloudSidecar is nil. The endpoint must still serve local data with
	// cloud_unreachable=true.
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	srv.SetCloudMode("cloud-secret")
	seedUser(t, srv, "pro@test.com", "pro", time.Now().UTC())
	// cloudSidecar deliberately not set.

	rr := adminBillingReq(srv, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 even without sidecar, got %d body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeBillingStats(t, rr)
	if !resp.CloudUnreachable {
		t.Errorf("cloud_unreachable: want true when sidecar unwired")
	}
	if resp.StripeConfigured {
		t.Errorf("stripe_configured: want false when no sidecar")
	}
	if resp.ActiveSubscriptions != 0 {
		t.Errorf("active_subscriptions: want 0 when sidecar unwired, got %d", resp.ActiveSubscriptions)
	}
	// Local data still flows through.
	if resp.CustomersByPlan["pro"] != 1 {
		t.Errorf("local customers_by_plan[pro]: want 1, got %d", resp.CustomersByPlan["pro"])
	}
	if resp.NewSignups30d != 1 {
		t.Errorf("new_signups_30d should reflect local data even without sidecar: want 1, got %d", resp.NewSignups30d)
	}
}

func TestAdminBillingStats_SidecarTransportError_DegradesToLocalOnly(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	srv.SetCloudMode("cloud-secret")
	srv.SetCloudSidecar(&fakeBillingSidecar{err: errors.New("dial tcp: connection refused")})

	rr := adminBillingReq(srv, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("transport error should still 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeBillingStats(t, rr)
	if !resp.CloudUnreachable {
		t.Errorf("cloud_unreachable: want true on transport error")
	}
	if resp.ActiveSubscriptions != 0 {
		t.Errorf("active_subscriptions: want 0 on transport error, got %d", resp.ActiveSubscriptions)
	}
}

func TestAdminBillingStats_SidecarSidecarError_DegradesToLocalOnly(t *testing.T) {
	// A non-200 from pad-cloud (e.g. wrong cloud secret, upstream Stripe
	// 5xx) is reported as a *billing.SidecarError. The handler must treat
	// it the same way as a transport error: cloud_unreachable=true, local
	// data still flows.
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	srv.SetCloudMode("cloud-secret")
	srv.SetCloudSidecar(&fakeBillingSidecar{
		err: &billing.SidecarError{Status: 500, Body: `{"error":"upstream stripe"}`},
	})

	rr := adminBillingReq(srv, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("sidecar 5xx should still 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeBillingStats(t, rr)
	if !resp.CloudUnreachable {
		t.Errorf("cloud_unreachable: want true on sidecar 5xx")
	}
}

func TestAdminBillingStats_StripeNotConfigured_PropagatesFlag(t *testing.T) {
	// pad-cloud returns stripe_configured=false when STRIPE_SECRET_KEY is
	// unset; the proxy must surface that flag verbatim AND keep
	// cloud_unreachable=false (the sidecar IS reachable, it just has no
	// Stripe wired up).
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	srv.SetCloudMode("cloud-secret")
	srv.SetCloudSidecar(&fakeBillingSidecar{
		response: &billing.BillingMetricsResponse{
			StripeConfigured: false,
			Currency:         "usd",
			ComputedAt:       time.Now().UTC(),
		},
	})

	rr := adminBillingReq(srv, adminToken)
	resp := decodeBillingStats(t, rr)
	if resp.CloudUnreachable {
		t.Errorf("cloud_unreachable: want false (sidecar IS reachable)")
	}
	if resp.StripeConfigured {
		t.Errorf("stripe_configured: want false")
	}
	if resp.ActiveSubscriptions != 0 || resp.MRRCents != 0 {
		t.Errorf("zero metrics expected when Stripe not configured, got active=%d mrr=%d",
			resp.ActiveSubscriptions, resp.MRRCents)
	}
}

// --- helpers ---

// registerNonAdmin inserts a member-role user directly into the store and
// mints a session token for them. Bypasses the admin-registration flow
// because that requires an admin session + CSRF, which would just be more
// plumbing for what's a single-purpose test.
func registerNonAdmin(t *testing.T, srv *Server, email, name string) string {
	t.Helper()
	id := newSeedID(email + "-nonadmin")
	pwHash := "$2a$10$K9Rz.placeholderwhich.rejects.all.passwords.via.bcrypt"
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(`INSERT INTO users
			(id, email, username, name, password_hash, role, plan, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, email, "u_"+id, name, pwHash, "member", "free", ts, ts); err != nil {
		t.Fatalf("insert non-admin user: %v", err)
	}

	// Empty userAgent matches subsequent test requests (httptest.NewRequest
	// does not set a User-Agent header), so the session-binding check sees
	// "" → "" rather than firing a binding-mismatch 401.
	token, err := srv.store.CreateSession(id, "test", "127.0.0.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("create session for non-admin: %v", err)
	}
	return token
}

// Compile-time check that the response shape carries every field the UI
// is going to read. Reaches into models so an unrelated User-shape rename
// also signals a recompile here — cheap canary, drop if it gets noisy.
var _ = func() *models.User {
	_ = AdminBillingStatsResponse{ComputedAt: time.Time{}}
	return nil
}()
