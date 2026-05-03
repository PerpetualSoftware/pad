package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// doAuthedDeleteWithCSRF issues a DELETE with both the session
// cookie + the CSRF double-submit pair. The session-cookie code path
// requires CSRF on every state-changing method; tests that bypass
// the API token route through SessionAuth need to attach both
// halves of the double-submit pair to clear CSRFProtect.
//
// The CSRF token must match in cookie + header (CSRFProtect's
// double-submit check) and be 64 hex chars (TASK-659 length gate).
func doAuthedDeleteWithCSRF(srv *Server, path string, sessionToken string) *httptest.ResponseRecorder {
	const testCSRF = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req := httptest.NewRequest("DELETE", path, nil)
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: sessionToken})
	req.AddCookie(&http.Cookie{Name: csrfCookieName(srv.secureCookies), Value: testCSRF})
	req.Header.Set("X-CSRF-Token", testCSRF)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// Connected-apps handler tests (PLAN-943 TASK-954).
//
// What's covered:
//
//   - GET /api/v1/connected-apps requires cloud mode (404 outside).
//   - List returns only the requesting user's connections.
//   - DTO surfaces the right field names + populates last_used /
//     calls_30d from the audit-log enrichment.
//   - DELETE 404s when the chain belongs to another user (no 403
//     enumeration leak).
//   - DELETE 204s on success and on a second idempotent call.
//   - DELETE writes an audit_trail entry (action="oauth_connection_revoked").

// connectedAppsTestServer builds a cloud-mode server with one OAuth
// client + helpers to seed grant chains. Returns the server + the
// client_id so tests can pass it to seedAccessChain.
func connectedAppsTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	srv := testServer(t)
	srv.SetCloudMode("test-secret")
	c, err := srv.store.CreateOAuthClient(models.OAuthClientCreate{
		Name:                    "Claude Desktop",
		RedirectURIs:            []string{"https://example.test/cb"},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		Scopes:                  []string{"pad:read", "pad:write"},
		Public:                  true,
	})
	if err != nil {
		t.Fatalf("CreateOAuthClient: %v", err)
	}
	return srv, c.ID
}

// seedGrantChain inserts one access-token row in a chain. Returns
// the request_id so tests can revoke / list / audit by it.
func seedGrantChain(t *testing.T, srv *Server, clientID, userID, requestID, sessionData, scopes string) {
	t.Helper()
	if err := srv.store.CreateAccessToken(models.OAuthRequest{
		Signature:     newSignatureForTest(),
		RequestID:     requestID,
		RequestedAt:   time.Now().UTC(),
		ClientID:      clientID,
		GrantedScopes: scopes,
		SessionData:   sessionData,
		Subject:       userID,
	}); err != nil {
		t.Fatalf("CreateAccessToken: %v", err)
	}
}

func newSignatureForTest() string {
	// Same shape as fosite would mint — just unique within the test
	// (the store treats it as opaque text).
	return "sig-" + time.Now().Format("150405.000000000")
}

func TestHandleListConnectedApps_RequiresCloudMode(t *testing.T) {
	srv := testServer(t) // no SetCloudMode
	_, tok := loginTestUser(t, srv)
	rr := doAuthedRequest(srv, "GET", "/api/v1/connected-apps", nil, tok)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no cloud mode)", rr.Code)
	}
}

func TestHandleListConnectedApps_ReturnsOnlyOwnConnections(t *testing.T) {
	srv, clientID := connectedAppsTestServer(t)
	alice, aliceTok := loginTestUser(t, srv)
	bob, err := srv.store.CreateUser(models.UserCreate{
		Email: "bob-conn-handler@example.com", Name: "Bob", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	seedGrantChain(t, srv, clientID, alice.ID, "alice-chain",
		`{"extra":{"allowed_workspaces":["docapp"]}}`, "pad:read pad:write")
	seedGrantChain(t, srv, clientID, bob.ID, "bob-chain", "", "pad:read")

	rr := doAuthedRequest(srv, "GET", "/api/v1/connected-apps", nil, aliceTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("len items = %d, want 1 (Alice's only)", len(resp.Items))
	}
	if got, _ := resp.Items[0]["id"].(string); got != "alice-chain" {
		t.Errorf("id = %v, want alice-chain", resp.Items[0]["id"])
	}
}

func TestHandleListConnectedApps_DTOShapeAndAuditEnrichment(t *testing.T) {
	srv, clientID := connectedAppsTestServer(t)
	user, tok := loginTestUser(t, srv)

	seedGrantChain(t, srv, clientID, user.ID, "shape-chain",
		`{"extra":{"allowed_workspaces":["alpha","beta"]}}`, "pad:read pad:write")

	// Seed a couple of audit rows so calls_30d > 0 + last_used populates.
	insertAudit := func(ts time.Time) {
		err := srv.store.InsertMCPAuditEntry(models.MCPAuditEntryInput{
			Timestamp:    ts,
			UserID:       user.ID,
			TokenKind:    models.TokenKindOAuth,
			TokenRef:     "shape-chain",
			ToolName:     "pad_item",
			ResultStatus: models.MCPAuditResultOK,
			RequestID:    "req",
		})
		if err != nil {
			t.Fatalf("InsertMCPAuditEntry: %v", err)
		}
	}
	now := time.Now().UTC()
	insertAudit(now.Add(-1 * time.Hour))
	insertAudit(now.Add(-30 * time.Minute))

	rr := doAuthedRequest(srv, "GET", "/api/v1/connected-apps", nil, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("len items = %d, want 1", len(resp.Items))
	}
	row := resp.Items[0]

	wantKeys := []string{"id", "client_id", "client_name", "allowed_workspaces",
		"scope_string", "capability_tier", "connected_at", "calls_30d"}
	for _, k := range wantKeys {
		if _, ok := row[k]; !ok {
			t.Errorf("missing field %q; got %+v", k, row)
		}
	}
	if got, _ := row["client_name"].(string); got != "Claude Desktop" {
		t.Errorf("client_name = %v, want Claude Desktop", row["client_name"])
	}
	if got, _ := row["capability_tier"].(string); got != "read_write" {
		t.Errorf("capability_tier = %v, want read_write", row["capability_tier"])
	}
	if got, _ := row["calls_30d"].(float64); got != 2 {
		t.Errorf("calls_30d = %v, want 2", row["calls_30d"])
	}
	if got, _ := row["last_used_at"].(string); got == "" {
		t.Errorf("last_used_at is empty; want a timestamp")
	}
	allowed, _ := row["allowed_workspaces"].([]any)
	if len(allowed) != 2 {
		t.Errorf("allowed_workspaces len = %d, want 2", len(allowed))
	}
}

func TestHandleRevokeConnectedApp_OwnerOnly(t *testing.T) {
	srv, clientID := connectedAppsTestServer(t)
	alice, aliceTok := loginTestUser(t, srv)
	bob, err := srv.store.CreateUser(models.UserCreate{
		Email: "bob-revoke@example.com", Name: "Bob", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}
	seedGrantChain(t, srv, clientID, bob.ID, "bobs-chain", "", "pad:read")

	// Alice tries to revoke Bob's chain → 404 (not 403 — anti-enumeration).
	rr := doAuthedDeleteWithCSRF(srv, "/api/v1/connected-apps/bobs-chain", aliceTok)
	if rr.Code != http.StatusNotFound {
		t.Errorf("Alice revoking Bob's chain: status = %d, want 404", rr.Code)
	}

	// Bob's chain is still active.
	conns, err := srv.store.ListUserOAuthConnections(bob.ID)
	if err != nil {
		t.Fatalf("ListUserOAuthConnections bob: %v", err)
	}
	if len(conns) != 1 {
		t.Errorf("after Alice's failed revoke, Bob's chains = %d, want 1", len(conns))
	}
	_ = alice
}

func TestHandleRevokeConnectedApp_HappyPathAndIdempotent(t *testing.T) {
	srv, clientID := connectedAppsTestServer(t)
	user, tok := loginTestUser(t, srv)
	seedGrantChain(t, srv, clientID, user.ID, "revoke-me", "", "pad:read")

	// First revoke → 204.
	rr := doAuthedDeleteWithCSRF(srv, "/api/v1/connected-apps/revoke-me", tok)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("first revoke status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
	}

	// Confirm gone from list.
	rr2 := doAuthedRequest(srv, "GET", "/api/v1/connected-apps", nil, tok)
	if rr2.Code != http.StatusOK {
		t.Fatalf("list after revoke: status = %d, want 200", rr2.Code)
	}
	var resp struct {
		Items []any `json:"items"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Errorf("after revoke, list len = %d, want 0", len(resp.Items))
	}

	// Second revoke → still 204 (idempotent).
	rr3 := doAuthedDeleteWithCSRF(srv, "/api/v1/connected-apps/revoke-me", tok)
	if rr3.Code != http.StatusNoContent {
		t.Errorf("idempotent second revoke status = %d, want 204", rr3.Code)
	}
}

func TestHandleRevokeConnectedApp_UnknownChain(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	_, tok := loginTestUser(t, srv)
	rr := doAuthedDeleteWithCSRF(srv, "/api/v1/connected-apps/no-such-chain", tok)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleRevokeConnectedApp_WritesAuditTrail(t *testing.T) {
	srv, clientID := connectedAppsTestServer(t)
	user, tok := loginTestUser(t, srv)
	seedGrantChain(t, srv, clientID, user.ID, "audit-trail-chain", "", "pad:read")

	rr := doAuthedDeleteWithCSRF(srv, "/api/v1/connected-apps/audit-trail-chain", tok)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204", rr.Code)
	}

	// Verify the audit_trail entry landed via the existing ListAuditLog
	// store method. action = oauth_connection_revoked.
	entries, err := srv.store.ListAuditLog(models.AuditLogParams{
		Action: "oauth_connection_revoked",
		Days:   1,
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("ListAuditLog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(entries))
	}
	if entries[0].UserID != user.ID {
		t.Errorf("audit user_id = %q, want %q", entries[0].UserID, user.ID)
	}
}
