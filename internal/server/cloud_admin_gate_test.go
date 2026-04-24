package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
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
		{"POST /admin/stripe-event-unmark with header", "POST", "/api/v1/admin/stripe-event-unmark", map[string]string{"cloud_secret": "x", "event_id": "evt_x"}},
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

	// Second call with same event_id — must be flagged as duplicate.
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
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if !second.AlreadyProcessed {
		t.Fatalf("second call should return already_processed=true")
	}
}

// TestStripeEventUnmark_RoundTripWithMarkProcessed is the happy-path
// regression for TASK-736: a row previously written by MarkStripeEventProcessed
// can be deleted by the unmark endpoint, and a subsequent
// MarkStripeEventProcessed call returns already_processed=false (proving
// the row really went away and Stripe retries can re-run the handler).
func TestStripeEventUnmark_RoundTripWithMarkProcessed(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("shh-its-a-secret")

	// 1. Mark an event as processed.
	req := cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-processed", map[string]string{
		"event_id":     "evt_unmark_roundtrip",
		"cloud_secret": "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("mark: %d %s", rr.Code, rr.Body.String())
	}

	// 2. Unmark it — should report unmarked=true.
	req = cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-unmark", map[string]string{
		"event_id":     "evt_unmark_roundtrip",
		"cloud_secret": "shh-its-a-secret",
	}, map[string]string{"X-Cloud-Secret": "shh-its-a-secret"})
	rr = httptest.NewRecorder()
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
	//    This is the behavior Stripe retries rely on after an unmark.
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

// TestStripeEventUnmark_IdempotentWhenRowMissing verifies the unmark call
// succeeds with unmarked=false when the event ID was never marked. This is
// the "unmark retry" case: the sidecar calls Unmark best-effort after a
// handler failure, but the handler might have failed on a retried event
// that was already rolled back. Either outcome is a 200.
func TestStripeEventUnmark_IdempotentWhenRowMissing(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("shh-its-a-secret")

	req := cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-unmark", map[string]string{
		"event_id":     "evt_never_marked",
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
// is applied symmetrically with /stripe-event-processed. A sidecar that
// presents the wrong secret must get a 403, not a 200, so a compromised
// or misconfigured caller cannot silently reopen Stripe retry windows for
// arbitrary event IDs.
func TestStripeEventUnmark_RejectsWrongSecret(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("the-real-secret")

	req := cloudAdminReq(t, "POST", "/api/v1/admin/stripe-event-unmark", map[string]string{
		"event_id":     "evt_x",
		"cloud_secret": "wrong-secret",
	}, map[string]string{"X-Cloud-Secret": "wrong-secret"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on wrong secret, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestStripeEventUnmark_ValidatesEventIDPrefix verifies the handler
// rejects event IDs that don't start with 'evt_', matching the
// /stripe-event-processed contract.
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
