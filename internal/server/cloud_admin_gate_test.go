package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xarmian/pad/internal/email"
	"github.com/xarmian/pad/internal/models"
)

// cloudAdminReq is a small helper that builds a request with optional JSON body
// and the caller-chosen headers.
func cloudAdminReq(t *testing.T, method, path string, body interface{}, headers map[string]string) *http.Request {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = "192.0.2.1:1"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

// TestCloudAdminGate_SelfHost_Returns404 verifies the cloud-admin endpoints
// disappear entirely when the server isn't in cloud mode — no "cloud mode
// not configured" disclosure, no auth prompt, just 404.
func TestCloudAdminGate_SelfHost_Returns404(t *testing.T) {
	srv := testServer(t)
	// Not in cloud mode — SetCloudMode never called.

	tests := []struct {
		name   string
		method string
		path   string
		body   interface{}
	}{
		{"POST /admin/plan with cloud secret header", "POST", "/api/v1/admin/plan", map[string]string{"cloud_secret": "x"}},
		{"POST /admin/stripe-customer-id with header", "POST", "/api/v1/admin/stripe-customer-id", map[string]string{"cloud_secret": "x"}},
		{"GET /admin/user-by-customer with header", "GET", "/api/v1/admin/user-by-customer?customer_id=cus_x", nil},
		{"POST /admin/stripe-event-processed with header", "POST", "/api/v1/admin/stripe-event-processed", map[string]string{"cloud_secret": "x", "event_id": "evt_x"}},
		{"POST /admin/stripe-event-unmark with header", "POST", "/api/v1/admin/stripe-event-unmark", map[string]string{"cloud_secret": "x", "event_id": "evt_x", "processed_at": "2025-01-01T00:00:00Z"}},
		{"POST /admin/payment-failed with header", "POST", "/api/v1/admin/payment-failed", map[string]string{"cloud_secret": "x", "stripe_customer_id": "cus_x"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Attach X-Cloud-Secret so the auth gate lets us through and we
			// hit requireCloudMode specifically.
			req := cloudAdminReq(t, tt.method, tt.path, tt.body, map[string]string{"X-Cloud-Secret": "any-value"})
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != http.StatusNotFound {
				t.Fatalf("%s: expected 404 in self-host mode, got %d: %s", tt.name, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestCloudAdminGate_NoCloudSecret_RequiresAuth verifies that without
// X-Cloud-Secret/?cloud_secret the endpoint falls through to the normal
// auth gate — an anonymous probe gets 401, not the handler-level
// "Cloud mode not configured" response that used to leak.
func TestCloudAdminGate_NoCloudSecret_RequiresAuth(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("secret-for-cloud") // enable cloud mode so requireCloudMode doesn't 404

	req := cloudAdminReq(t, "GET", "/api/v1/admin/user-by-customer?customer_id=cus_x", nil, nil)
	// No X-Cloud-Secret, no cookie, no token.
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (auth required), got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestCloudAdminGate_ValidCloudSecret_PassesAuthAndCSRF verifies a sidecar
// POST with the right X-Cloud-Secret reaches the handler — CSRF is off
// because the header signals non-cookie auth.
func TestCloudAdminGate_ValidCloudSecret_PassesAuthAndCSRF(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("shh-its-a-secret")

	req := cloudAdminReq(t, "POST", "/api/v1/admin/stripe-customer-id", map[string]string{
		"user_id":      "does-not-exist",
		"customer_id":  "cus_1234",
		"cloud_secret": "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	// User doesn't exist so we expect 404 from the handler — but critically,
	// NOT 401/403 from auth/CSRF middleware. Reaching the handler at all is
	// the proof that the gate let the sidecar through.
	if rr.Code == http.StatusUnauthorized || rr.Code == http.StatusForbidden {
		t.Fatalf("auth/CSRF incorrectly blocked sidecar call: %d %s", rr.Code, rr.Body.String())
	}
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected handler to reject unknown user_id with 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestCloudAdminGate_BypassScopedToCloudPaths verifies the regression
// Codex P0'd on PR #182: setting X-Cloud-Secret on a NON-cloud-admin
// path must NOT bypass auth. Applies to every non-whitelisted route.
func TestCloudAdminGate_BypassScopedToCloudPaths(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	// Setting X-Cloud-Secret on GET /api/v1/workspaces must still require
	// normal user auth — a pre-fix attacker could list workspaces anonymously.
	req := cloudAdminReq(t, "GET", "/api/v1/workspaces", nil,
		map[string]string{"X-Cloud-Secret": "does-not-matter"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("X-Cloud-Secret on non-cloud path bypassed auth: got %d, want 401", rr.Code)
	}

	// Same test with the legacy query-param.
	req = cloudAdminReq(t, "GET", "/api/v1/workspaces?cloud_secret=x", nil, nil)
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("?cloud_secret on non-cloud path bypassed auth: got %d, want 401", rr.Code)
	}

	// POST /api/v1/workspaces — creates a workspace. If X-Cloud-Secret
	// bypassed auth here, an anon attacker could create workspaces.
	// CSRF middleware may reject first (403) before auth (401); either
	// status is a valid rejection, but must NOT be a 2xx.
	req = cloudAdminReq(t, "POST", "/api/v1/workspaces",
		map[string]string{"name": "hijacked"},
		map[string]string{"X-Cloud-Secret": "x"})
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code < 400 {
		t.Fatalf("X-Cloud-Secret on POST /workspaces bypassed auth: got %d (expected 401 or 403)", rr.Code)
	}
}

// TestCloudAdminGate_BodySecret_BackwardCompat verifies a POST with
// cloud_secret ONLY in the JSON body still works — the existing pad-cloud
// sidecar sent the secret there, not in a header, so removing body support
// outright would break deployed sidecars. Scoped to cloud admin paths
// only (the body peek never fires elsewhere).
func TestCloudAdminGate_BodySecret_BackwardCompat(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("body-secret")

	req := cloudAdminReq(t, "POST", "/api/v1/admin/stripe-customer-id", map[string]string{
		"user_id":      "unknown-user-id",
		"customer_id":  "cus_body",
		"cloud_secret": "body-secret",
	}, nil) // NO X-Cloud-Secret header — body only
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	// Must reach the handler — not be rejected at auth/CSRF.
	if rr.Code == http.StatusUnauthorized || rr.Code == http.StatusForbidden {
		t.Fatalf("body-only cloud_secret rejected by middleware: %d %s", rr.Code, rr.Body.String())
	}
}

// TestCloudAdminGate_QueryParamSecret_Rejected verifies TASK-656 dropped
// the legacy ?cloud_secret= query-param — query values land in access
// logs, so accepting them there leaked the cloud trust boundary. Must
// be rejected by the auth gate.
func TestCloudAdminGate_QueryParamSecret_Rejected(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("legacy-secret")

	req := cloudAdminReq(t, "GET",
		"/api/v1/admin/user-by-customer?customer_id=cus_unknown&cloud_secret=legacy-secret",
		nil, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("?cloud_secret= should no longer authenticate, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestStripeEventProcessed_RecordsAndDetectsDuplicates verifies TASK-696:
// first call for a given event_id returns already_processed=false; a
// second call for the same event_id returns already_processed=true. This
// is what gives the sidecar durable idempotency across restarts.
//
// Also covers TASK-736: the response MUST include processed_at, and the
// duplicate call MUST return the existing row's processed_at (not a fresh
// one) so the sidecar can store a token that matches the row in the DB.
func TestStripeEventProcessed_RecordsAndDetectsDuplicates(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("shh-its-a-secret")

	// First call — should be new.
	req := cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-processed", map[string]string{
		"event_id":     "evt_test_12345",
		"cloud_secret": "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("first call expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var first struct {
		EventID          string `json:"event_id"`
		AlreadyProcessed bool   `json:"already_processed"`
		ProcessedAt      string `json:"processed_at"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if first.AlreadyProcessed {
		t.Fatalf("first call should return already_processed=false")
	}
	if first.EventID != "evt_test_12345" {
		t.Fatalf("first call returned wrong event_id: %q", first.EventID)
	}
	if first.ProcessedAt == "" {
		t.Fatalf("first call must return a non-empty processed_at for the unmark round-trip")
	}

	// Second call with same event_id — must be flagged as duplicate AND
	// return the existing row's processed_at (not a fresh timestamp).
	req = cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-processed", map[string]string{
		"event_id":     "evt_test_12345",
		"cloud_secret": "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("second call expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var second struct {
		EventID          string `json:"event_id"`
		AlreadyProcessed bool   `json:"already_processed"`
		ProcessedAt      string `json:"processed_at"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if !second.AlreadyProcessed {
		t.Fatalf("second call should return already_processed=true")
	}
	if second.ProcessedAt != first.ProcessedAt {
		t.Errorf("duplicate call must return the EXISTING row's processed_at (%q), got %q",
			first.ProcessedAt, second.ProcessedAt)
	}
}

// markEventForUnmarkTest is a helper that POSTs stripe-event-processed and
// returns the processed_at token the response hands back. Keeps the setup
// of the unmark tests focused on the unmark semantics.
func markEventForUnmarkTest(t *testing.T, srv *Server, eventID, secret string) string {
	t.Helper()
	req := cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-processed", map[string]string{
		"event_id":     eventID,
		"cloud_secret": secret,
	}, map[string]string{"X-Cloud-Secret": secret})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("mark %s: %d %s", eventID, rr.Code, rr.Body.String())
	}
	var resp struct {
		ProcessedAt string `json:"processed_at"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode mark response: %v", err)
	}
	if resp.ProcessedAt == "" {
		t.Fatalf("mark response missing processed_at")
	}
	return resp.ProcessedAt
}

// TestStripeEventUnmark_RoundTripWithMarkProcessed is the happy-path
// regression for TASK-736: a row previously written by /stripe-event-processed
// can be deleted by the unmark endpoint (given the matching processed_at
// token), and a subsequent mark call returns already_processed=false
// (proving the row really went away and Stripe retries can re-run).
func TestStripeEventUnmark_RoundTripWithMarkProcessed(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("shh-its-a-secret")

	// 1. Mark an event as processed and grab the token.
	token := markEventForUnmarkTest(t, srv, "evt_unmark_roundtrip", "shh-its-a-secret")

	// 2. Unmark it — should report unmarked=true.
	req := cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-unmark", map[string]string{
		"event_id":     "evt_unmark_roundtrip",
		"processed_at": token,
		"cloud_secret": "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unmark: %d %s", rr.Code, rr.Body.String())
	}
	var unmarkResp struct {
		EventID  string `json:"event_id"`
		Unmarked bool   `json:"unmarked"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &unmarkResp); err != nil {
		t.Fatalf("decode unmark: %v", err)
	}
	if !unmarkResp.Unmarked {
		t.Errorf("unmark should return unmarked=true when row existed")
	}
	if unmarkResp.EventID != "evt_unmark_roundtrip" {
		t.Errorf("unmark returned wrong event_id: %q", unmarkResp.EventID)
	}

	// 3. Re-mark — now that the row is gone, must report already_processed=false.
	req = cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-processed", map[string]string{
		"event_id":     "evt_unmark_roundtrip",
		"cloud_secret": "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("re-mark: %d %s", rr.Code, rr.Body.String())
	}
	var remark struct {
		AlreadyProcessed bool `json:"already_processed"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &remark); err != nil {
		t.Fatalf("decode re-mark: %v", err)
	}
	if remark.AlreadyProcessed {
		t.Errorf("after unmark, re-mark must return already_processed=false (retry path broken)")
	}
}

// TestStripeEventUnmark_StaleTokenIsNoOp addresses the TASK-736 race-
// protection HIGH: a delayed unmark from an EARLIER failed attempt must
// NOT delete the fresh marker left by a SUCCESSFUL retry. The composite
// (event_id, processed_at) delete key enforces this — a stale token
// simply doesn't match the fresh row.
func TestStripeEventUnmark_StaleTokenIsNoOp(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("shh-its-a-secret")

	// Capture the stale token from the first (doomed) attempt.
	staleTok := markEventForUnmarkTest(t, srv, "evt_race", "shh-its-a-secret")

	// Sidecar rolls back (successful unmark with the stale token).
	req := cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-unmark", map[string]string{
		"event_id":     "evt_race",
		"processed_at": staleTok,
		"cloud_secret": "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("rollback: %d %s", rr.Code, rr.Body.String())
	}

	// now() has second resolution; wait so the next mark writes a
	// distinct timestamp. Without this the retry could re-use the same
	// token and the test assumption collapses.
	time.Sleep(1100 * time.Millisecond)

	// A successful retry re-marks with a fresh token.
	freshTok := markEventForUnmarkTest(t, srv, "evt_race", "shh-its-a-secret")
	if freshTok == staleTok {
		t.Fatalf("fresh mark reused the stale token (%q) — test setup assumption broken", freshTok)
	}

	// A delayed stale unmark fires. MUST be a no-op so the fresh row
	// stays put.
	req = cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-unmark", map[string]string{
		"event_id":     "evt_race",
		"processed_at": staleTok,
		"cloud_secret": "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("stale unmark: %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Unmarked bool `json:"unmarked"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode stale unmark: %v", err)
	}
	if resp.Unmarked {
		t.Error("stale unmark must NOT delete the fresh marker (race would reopen retry window)")
	}

	// Sanity check: marking again still sees the fresh row.
	req = cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-processed", map[string]string{
		"event_id":     "evt_race",
		"cloud_secret": "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	var sanity struct {
		AlreadyProcessed bool   `json:"already_processed"`
		ProcessedAt      string `json:"processed_at"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &sanity)
	if !sanity.AlreadyProcessed {
		t.Error("fresh marker must still be present after stale unmark")
	}
	if sanity.ProcessedAt != freshTok {
		t.Errorf("token changed unexpectedly; got %q, want %q", sanity.ProcessedAt, freshTok)
	}
}

// TestStripeEventUnmark_IdempotentWhenRowMissing verifies the unmark call
// succeeds with unmarked=false when the event ID was never marked. Either
// outcome is a 200.
func TestStripeEventUnmark_IdempotentWhenRowMissing(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("shh-its-a-secret")

	req := cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-unmark", map[string]string{
		"event_id":     "evt_never_marked",
		"processed_at": "2025-01-01T00:00:00Z",
		"cloud_secret": "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on missing-row unmark, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Unmarked bool `json:"unmarked"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Unmarked {
		t.Errorf("unmark of missing row must return unmarked=false")
	}
}

// TestStripeEventUnmark_RejectsWrongSecret verifies the cloud-secret gate
// is applied symmetrically with /stripe-event-processed.
func TestStripeEventUnmark_RejectsWrongSecret(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("the-real-secret")

	req := cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-unmark", map[string]string{
		"event_id":     "evt_x",
		"processed_at": "2025-01-01T00:00:00Z",
		"cloud_secret": "wrong-secret",
	}, map[string]string{"X-Cloud-Secret": "wrong-secret"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on wrong secret, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestStripeEventUnmark_RequiresProcessedAt verifies that a missing
// processed_at field produces a 400 rather than falling back to any
// fallback-by-event-id behaviour. The processed_at is the race-protection
// contract; dropping it would be unsafe.
func TestStripeEventUnmark_RequiresProcessedAt(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("shh-its-a-secret")

	req := cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-unmark", map[string]string{
		"event_id": "evt_x",
		// processed_at intentionally omitted
		"cloud_secret": "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without processed_at, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestStripeEventUnmark_ValidatesEventIDPrefix verifies the handler
// rejects event IDs that don't start with 'evt_'.
func TestStripeEventUnmark_ValidatesEventIDPrefix(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("shh-its-a-secret")

	tests := []struct {
		name    string
		eventID string
	}{
		{"empty event_id", ""},
		{"missing evt_ prefix", "sub_12345"},
		{"wrong prefix cus_", "cus_12345"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-unmark", map[string]string{
				"event_id":     tt.eventID,
				"processed_at": "2025-01-01T00:00:00Z",
				"cloud_secret": "shh-its-a-secret",
			}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("%s: expected 400, got %d: %s", tt.name, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestStripeEventUnmark_WritesAuditLog is the TASK-736 durable-audit
// finding fix: a successful unmark MUST emit an ActionStripeEventUnmarked
// audit entry so admins can see who reopened retry windows and when. slog
// alone is not enough (no actor, not queryable via /audit-log).
func TestStripeEventUnmark_WritesAuditLog(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("shh-its-a-secret")

	token := markEventForUnmarkTest(t, srv, "evt_audit", "shh-its-a-secret")

	req := cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-unmark", map[string]string{
		"event_id":     "evt_audit",
		"processed_at": token,
		"cloud_secret": "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unmark: %d %s", rr.Code, rr.Body.String())
	}

	// Verify an audit row exists via the audit-log query interface.
	activities, err := srv.store.ListAuditLog(models.AuditLogParams{Action: models.ActionStripeEventUnmarked, Limit: 10})
	if err != nil {
		t.Fatalf("list audit log: %v", err)
	}
	if len(activities) == 0 {
		t.Fatal("no ActionStripeEventUnmarked activity was logged")
	}
	// Find the one we just created.
	var found bool
	for _, a := range activities {
		if strings.Contains(a.Metadata, "evt_audit") {
			found = true
			// Metadata should record unmarked=true.
			if !strings.Contains(a.Metadata, `"unmarked":"true"`) {
				t.Errorf("expected unmarked=true in metadata, got %q", a.Metadata)
			}
			break
		}
	}
	if !found {
		t.Errorf("no audit entry contained event_id=evt_audit; got %d entries", len(activities))
	}
}

// TestStripeEventProcessed_ValidatesEventIDPrefix verifies the handler
// rejects event IDs that don't start with 'evt_', matching the existing
// 'cus_' prefix validation on stripe-customer-id.
func TestStripeEventProcessed_ValidatesEventIDPrefix(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("shh-its-a-secret")

	tests := []struct {
		name    string
		eventID string
	}{
		{"empty event_id", ""},
		{"missing evt_ prefix", "sub_12345"},
		{"wrong prefix cus_", "cus_12345"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-processed", map[string]string{
				"event_id":     tt.eventID,
				"cloud_secret": "shh-its-a-secret",
			}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("%s: expected 400, got %d: %s", tt.name, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestPaymentFailed_ValidatesCustomerIDPrefix rejects stripe_customer_id
// values that don't start with 'cus_'. Same shape as the evt_ prefix
// check on stripe-event-processed.
func TestPaymentFailed_ValidatesCustomerIDPrefix(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("shh-its-a-secret")

	tests := []struct {
		name       string
		customerID string
	}{
		{"empty customer_id", ""},
		{"missing cus_ prefix", "sub_12345"},
		{"wrong prefix evt_", "evt_12345"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := cloudAdminReq(t, "POST", "/api/v1/admin/payment-failed", map[string]string{
				"stripe_customer_id": tt.customerID,
				"cloud_secret":       "shh-its-a-secret",
			}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("%s: expected 400, got %d: %s", tt.name, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestPaymentFailed_UnknownCustomer_Returns200_NoEmail verifies that when
// the sidecar forwards an invoice.payment_failed for a customer pad does
// not recognise, pad returns 200 with email_sent=false and reason=
// "no_customer" — the sidecar must NOT treat this as a 5xx that would
// trigger a webhook retry.
func TestPaymentFailed_UnknownCustomer_Returns200_NoEmail(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("shh-its-a-secret")

	req := cloudAdminReq(t, "POST", "/api/v1/admin/payment-failed", map[string]string{
		"stripe_customer_id": "cus_nonexistent_abc",
		"cloud_secret":       "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for unknown customer, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if sent, _ := resp["email_sent"].(bool); sent {
		t.Errorf("email_sent=true for unknown customer; want false")
	}
	if got, _ := resp["reason"].(string); got != "no_customer" {
		t.Errorf("reason=%q for unknown customer; want no_customer", got)
	}
}

// TestPaymentFailed_EmailNotConfigured_Returns200_NoEmail verifies the
// handler degrades gracefully when Maileroo is not wired up — audit log
// captures the skip, response is 200 so the sidecar doesn't retry.
func TestPaymentFailed_EmailNotConfigured_Returns200_NoEmail(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("shh-its-a-secret")
	// Look up the bootstrapped admin and assign a Stripe customer ID so
	// the handler's lookup step resolves, then falls through to the
	// "email not configured" branch.
	adminUser, err := srv.store.GetUserByEmail("admin@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if err := srv.store.SetUserStripeCustomerID(adminUser.ID, "cus_test_nomail"); err != nil {
		t.Fatalf("SetUserStripeCustomerID: %v", err)
	}
	// Note: testServer does NOT call email.Configure, so s.email has no
	// credentials and s.baseURL is empty — both fail the configured check.

	req := cloudAdminReq(t, "POST", "/api/v1/admin/payment-failed", map[string]string{
		"stripe_customer_id": "cus_test_nomail",
		"amount_display":     "$10.00",
		"next_retry_display": "April 30, 2026",
		"cloud_secret":       "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 when email not configured, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if sent, _ := resp["email_sent"].(bool); sent {
		t.Errorf("email_sent=true with no Maileroo config; want false")
	}
	if got, _ := resp["reason"].(string); got != "email_not_configured" {
		t.Errorf("reason=%q with no Maileroo config; want email_not_configured", got)
	}
}

// paymentFailedAuditForUser walks the audit log for a given user and
// returns every payment_failed_email_sent row's parsed metadata. Used
// by the send-path tests to assert reason + sent fields land correctly
// for each branch.
func paymentFailedAuditForUser(t *testing.T, srv *Server, userID string) []map[string]string {
	t.Helper()
	events, err := srv.store.ListAuditLog(models.AuditLogParams{
		Action: models.ActionPaymentFailedEmailSent,
		Limit:  100,
	})
	if err != nil {
		t.Fatalf("ListAuditLog: %v", err)
	}
	var out []map[string]string
	for _, ev := range events {
		if userID != "" && ev.UserID != userID {
			continue
		}
		var meta map[string]string
		_ = json.Unmarshal([]byte(ev.Metadata), &meta)
		out = append(out, meta)
	}
	return out
}

// mockMailerooEndpoint stands up an httptest server that speaks enough
// of Maileroo's v2 JSON API to satisfy email.Sender. Returning 500
// exercises the send_failed branch; returning 200 + success:true
// exercises the sent branch.
func mockMailerooEndpoint(t *testing.T, status int, success bool) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		body := `{"success":true,"message":"sent"}`
		if !success {
			body = `{"success":false,"message":"mock failure"}`
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// configureEmailForTest attaches an email.Sender wired to the given
// mock endpoint plus a base URL so s.baseURL != "" (the configured
// check looks at both).
func configureEmailForTest(srv *Server, endpoint, baseURL string) {
	sender := email.NewSender("test-key", "noreply@test.getpad.dev", "Pad Test", baseURL)
	sender.SetEndpoint(endpoint)
	srv.SetEmailSender(sender, "test-key")
	srv.SetBaseURL(baseURL)
}

// TestPaymentFailed_HappyPath_SendsAndAudits covers the end-to-end
// success path: Maileroo returns 200/success, pad records an audit row
// with reason=sent and sent=true attached to the target user ID.
func TestPaymentFailed_HappyPath_SendsAndAudits(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "paying@example.com", "Paying User")
	srv.SetCloudMode("shh-its-a-secret")

	payingUser, err := srv.store.GetUserByEmail("paying@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if err := srv.store.SetUserStripeCustomerID(payingUser.ID, "cus_happy_path"); err != nil {
		t.Fatalf("SetUserStripeCustomerID: %v", err)
	}

	mock := mockMailerooEndpoint(t, http.StatusOK, true)
	configureEmailForTest(srv, mock.URL, "https://app.test.getpad.dev")

	req := cloudAdminReq(t, "POST", "/api/v1/admin/payment-failed", map[string]string{
		"stripe_customer_id": "cus_happy_path",
		"amount_display":     "10.00 USD",
		"next_retry_display": "April 30, 2026",
		"cloud_secret":       "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("happy path: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if sent, _ := resp["email_sent"].(bool); !sent {
		t.Errorf("email_sent=false on happy path; want true: %v", resp)
	}
	if got, _ := resp["reason"].(string); got != "sent" {
		t.Errorf("reason=%q on happy path; want sent", got)
	}

	audit := paymentFailedAuditForUser(t, srv, payingUser.ID)
	if len(audit) != 1 {
		t.Fatalf("expected 1 audit row for happy path, got %d", len(audit))
	}
	if audit[0]["reason"] != "sent" || audit[0]["sent"] != "true" {
		t.Errorf("audit metadata wrong: %v", audit[0])
	}
	if audit[0]["stripe_customer_id"] != "cus_happy_path" {
		t.Errorf("audit missing stripe_customer_id: %v", audit[0])
	}
}

// TestPaymentFailed_MailerooError_Returns200_SendFailed_AndAudits covers
// the send_failed branch: Maileroo 500 → response stays 200 + reason=
// send_failed so the sidecar does not retry the Stripe webhook, and an
// audit row with reason=send_failed + sent=false is written against
// the target user ID.
func TestPaymentFailed_MailerooError_Returns200_SendFailed_AndAudits(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "unlucky@example.com", "Unlucky User")
	srv.SetCloudMode("shh-its-a-secret")

	unluckyUser, err := srv.store.GetUserByEmail("unlucky@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if err := srv.store.SetUserStripeCustomerID(unluckyUser.ID, "cus_send_fail"); err != nil {
		t.Fatalf("SetUserStripeCustomerID: %v", err)
	}

	mock := mockMailerooEndpoint(t, http.StatusInternalServerError, false)
	configureEmailForTest(srv, mock.URL, "https://app.test.getpad.dev")

	req := cloudAdminReq(t, "POST", "/api/v1/admin/payment-failed", map[string]string{
		"stripe_customer_id": "cus_send_fail",
		"cloud_secret":       "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("send_failed: expected 200 (so sidecar does not retry webhook), got %d: %s",
			rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if sent, _ := resp["email_sent"].(bool); sent {
		t.Errorf("email_sent=true on send failure; want false")
	}
	if got, _ := resp["reason"].(string); got != "send_failed" {
		t.Errorf("reason=%q on send failure; want send_failed", got)
	}

	audit := paymentFailedAuditForUser(t, srv, unluckyUser.ID)
	if len(audit) != 1 {
		t.Fatalf("expected 1 audit row for send_failed, got %d", len(audit))
	}
	if audit[0]["reason"] != "send_failed" || audit[0]["sent"] != "false" {
		t.Errorf("audit metadata wrong: %v", audit[0])
	}
}

// TestPaymentFailed_UnknownCustomer_AuditsWithoutUserID confirms that
// the no_customer skip path also writes an audit row — just with no
// user filter, since we don't have a user to attribute it to. This is
// important for dunning reconciliation: an operator can still find the
// event via action + stripe_customer_id metadata.
func TestPaymentFailed_UnknownCustomer_AuditsWithoutUserID(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("shh-its-a-secret")

	req := cloudAdminReq(t, "POST", "/api/v1/admin/payment-failed", map[string]string{
		"stripe_customer_id": "cus_totally_unknown",
		"cloud_secret":       "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	audit := paymentFailedAuditForUser(t, srv, "")
	// Filter to rows whose metadata customer ID matches, since the test
	// DB might contain other payment_failed rows from earlier tests that
	// share the same DB instance if testServer is ever pooled.
	var found map[string]string
	for _, row := range audit {
		if row["stripe_customer_id"] == "cus_totally_unknown" {
			found = row
			break
		}
	}
	if found == nil {
		t.Fatalf("expected audit row for cus_totally_unknown, got %d rows total", len(audit))
	}
	if found["reason"] != "no_customer" || found["sent"] != "false" {
		t.Errorf("audit metadata wrong: %v", found)
	}
}

// TestCloudAdminGate_HeaderSecret_StillAuthenticates confirms the header
// form (the only supported sidecar auth after TASK-656) still works on
// the GET endpoint that previously used query-param.
func TestCloudAdminGate_HeaderSecret_StillAuthenticates(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("header-only")

	req := cloudAdminReq(t, "GET",
		"/api/v1/admin/user-by-customer?customer_id=cus_unknown",
		nil, map[string]string{"X-Cloud-Secret": "header-only"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	// 404 from the handler (no user maps to the customer ID) — not 401/403.
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected handler-level 404, got %d: %s", rr.Code, rr.Body.String())
	}
}
