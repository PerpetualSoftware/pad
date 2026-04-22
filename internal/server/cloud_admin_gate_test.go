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

// TestCloudAdminGate_QueryParamSecret_BackwardCompat keeps the legacy
// ?cloud_secret= query param working for GETs while TASK-656 pares it back.
func TestCloudAdminGate_QueryParamSecret_BackwardCompat(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode("legacy-secret")

	req := cloudAdminReq(t, "GET",
		"/api/v1/admin/user-by-customer?customer_id=cus_unknown&cloud_secret=legacy-secret",
		nil, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	// Should reach the handler and return 404 (no user mapped to that customer).
	if rr.Code == http.StatusUnauthorized || rr.Code == http.StatusForbidden {
		t.Fatalf("auth/CSRF blocked legacy query-param secret: %d %s", rr.Code, rr.Body.String())
	}
}
