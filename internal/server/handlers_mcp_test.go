package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestMCP_CloudModeOff_RoutesAbsent verifies the negative case:
// when cloud mode is NOT enabled, every MCP-related route returns
// 404. This is the self-hosted contract — the binary stays free of
// MCP-server overhead unless an operator explicitly opts in.
func TestMCP_CloudModeOff_RoutesAbsent(t *testing.T) {
	srv := testServer(t)
	// Cloud mode deliberately not set; SetMCPTransport never called.

	cases := []struct {
		method, path string
	}{
		{"POST", "/mcp"},
		{"GET", "/.well-known/oauth-protected-resource"},
		{"GET", "/.well-known/oauth-authorization-server"},
	}
	for _, tc := range cases {
		rr := doRequest(srv, tc.method, tc.path, nil)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s %s: expected 404 with cloud mode off, got %d (body: %s)",
				tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

// TestMCP_CloudModeOnButTransportNotWired_RoutesAbsent guards against
// the regression where someone enables cloud mode but forgets to call
// SetMCPTransport. The route group is gated on BOTH conditions; this
// pins the AND.
func TestMCP_CloudModeOnButTransportNotWired_RoutesAbsent(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode("test-secret")
	// Note: SetMCPTransport NOT called.

	rr := doRequest(srv, "POST", "/mcp", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 when transport not wired, got %d", rr.Code)
	}
}

// TestMCP_DiscoveryDoc_PopulatedFromConfig verifies the protected-
// resource metadata document echoes the URLs we hand SetMCPTransport
// (no host-derived fallback). Pinning this prevents a regression
// where a config change makes the doc emit fallback values that don't
// match the cert in production.
func TestMCP_DiscoveryDoc_PopulatedFromConfig(t *testing.T) {
	srv := mcpEnabledTestServer(t)

	rr := doRequest(srv, "GET", "/.well-known/oauth-protected-resource", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc == "" {
		t.Errorf("expected Cache-Control header on discovery doc, got empty")
	}

	var doc protectedResourceMetadata
	parseJSON(t, rr, &doc)

	if doc.Resource != "https://mcp.test.example/mcp" {
		t.Errorf("Resource: got %q, want %q", doc.Resource, "https://mcp.test.example/mcp")
	}
	if len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != "https://app.test.example" {
		t.Errorf("AuthorizationServers: got %v, want [https://app.test.example]", doc.AuthorizationServers)
	}
	wantScopes := []string{"pad:read", "pad:write", "pad:admin"}
	if !sliceEqual(doc.ScopesSupported, wantScopes) {
		t.Errorf("ScopesSupported: got %v, want %v", doc.ScopesSupported, wantScopes)
	}
	if !sliceEqual(doc.BearerMethodsSupported, []string{"header"}) {
		t.Errorf("BearerMethodsSupported: got %v, want [header]", doc.BearerMethodsSupported)
	}
}

// TestMCP_AuthServerMetadata_Mounted confirms the RFC 8414
// authorization-server discovery doc is mounted by the cloud-mode
// route group.
//
// The stub-501 contract from TASK-950 was replaced by sub-PR C
// (TASK-1025) when /oauth/{authorize,token,register} actually
// exist. The full document-shape assertions live in
// TestOAuth_AuthorizationServerMetadata_PopulatedShape; here we
// just confirm the endpoint mounts + serves a 200 (or a 503 when
// the auth-server URL is not configured, the fail-loud branch).
func TestMCP_AuthServerMetadata_Mounted(t *testing.T) {
	srv := mcpEnabledTestServer(t)

	rr := doRequest(srv, "GET", "/.well-known/oauth-authorization-server", nil)
	// mcpEnabledTestServer passes a non-empty mcpAuthServerURL
	// ("https://app.test.example"), so the doc serves 200 with
	// the populated metadata. 501 is now extinct.
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 with populated RFC 8414 metadata, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestMCP_NoToken_Returns401WithWWWAuthenticate covers the most
// important MCP discovery-flow case: a fresh client with no token hits
// /mcp and gets the spec-shaped 401 + WWW-Authenticate that points
// them at the discovery doc. Without this, Claude Desktop refuses to
// proceed past the first request.
func TestMCP_NoToken_Returns401WithWWWAuthenticate(t *testing.T) {
	srv := mcpEnabledTestServer(t)

	rr := doRequest(srv, "POST", "/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no token, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	wwwAuth := rr.Header().Get("WWW-Authenticate")
	if wwwAuth == "" {
		t.Fatal("expected WWW-Authenticate header on 401, got empty")
	}
	if !strings.Contains(wwwAuth, `Bearer realm="pad"`) {
		t.Errorf("WWW-Authenticate missing Bearer realm: %q", wwwAuth)
	}
	if !strings.Contains(wwwAuth, `resource_metadata="https://mcp.test.example/.well-known/oauth-protected-resource"`) {
		t.Errorf("WWW-Authenticate missing resource_metadata pointing at discovery doc: %q", wwwAuth)
	}
}

// TestMCP_BadTokenFormat_Returns401WithWWWAuthenticate covers the
// same envelope shape for a bearer that isn't even shaped like a pad
// PAT. Format-rejection happens before the DB lookup.
func TestMCP_BadTokenFormat_Returns401WithWWWAuthenticate(t *testing.T) {
	srv := mcpEnabledTestServer(t)

	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer not-a-real-pad-token")
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
	if rr.Header().Get("WWW-Authenticate") == "" {
		t.Error("expected WWW-Authenticate header on 401 for bad-format token")
	}
}

// TestMCP_ValidPAT_ReachesTransport confirms the happy path: a real
// PAT for a real user authenticates, and the request reaches the
// downstream MCP transport handler with the user attached to context.
//
// We replace the streamable-HTTP server with a tiny stub so the test
// doesn't need to drive a full MCP handshake — the contract this
// test pins is "auth + routing put the request in front of the
// transport with the right user," not "the streamable transport
// works" (mcp-go's own tests cover that).
func TestMCP_ValidPAT_ReachesTransport(t *testing.T) {
	srv := testServer(t)

	user, err := srv.store.CreateUser(models.UserCreate{
		Email:    "mcp-test@example.com",
		Name:     "MCP Tester",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: err=%v", err)
	}
	// PATs are stored with workspace_id NOT NULL (migration 011),
	// so even user-owned tokens need a workspace handle. The auth
	// middleware doesn't enforce token-workspace match for user-
	// owned tokens — workspace membership is the gate (see
	// middleware_auth.go:393) — but we still need a row that
	// satisfies the FK.
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "MCP Test WS"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	tok, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{
		Name:        "mcp-test-token",
		WorkspaceID: ws.ID,
	}, 30, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: err=%v", err)
	}

	// Stub transport: records that it was called + which user was
	// attached. Assertions live in the stub so a misrouted request
	// (e.g. anonymous reach) shows up as "stub never called" rather
	// than a misleading 200.
	var called bool
	var seenUserID string
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if u, ok := CurrentUserFromContext(r.Context()); ok && u != nil {
			seenUserID = u.ID
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	})

	srv.SetCloudMode("test-secret")
	srv.SetMCPTransport(stub, "https://mcp.test.example", "https://app.test.example")

	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if !called {
		t.Fatal("transport stub never called — auth or routing failed silently")
	}
	if seenUserID != user.ID {
		t.Errorf("user not attached to transport context: got %q, want %q", seenUserID, user.ID)
	}
}

// Note on legacy workspace-scoped tokens: MCPBearerAuth rejects PATs
// that have no user_id (the pre-migration-012 path). We don't have a
// full-stack test for that branch because the current api_tokens
// schema (workspace_id NOT NULL + user_id FK to users) makes it
// impossible to construct such a token via the public store API —
// CreateAPIToken with userID="" fails the FK constraint, and there's
// no SQL backdoor in the test helpers. The branch exists as defense
// in depth for old rows that survived migration 012, and is
// documented in MCPBearerAuth's comment (middleware_mcp_auth.go).
// A unit test against extractBearer + a fake store interface is the
// right shape if someone adds tests for this branch later.

// TestMCP_NoToken_FallsBackToHostWhenPublicURLUnset pins the
// regression Codex review #369 round 1 caught: when PAD_MCP_PUBLIC_URL
// is unset, writeMCPUnauthorized used to drop the WWW-Authenticate
// header entirely, which breaks fresh-client discovery on cloud-mode
// deploys that hadn't configured the public URL yet. The fallback
// derives "https://" + r.Host so the discovery handshake completes.
func TestMCP_NoToken_FallsBackToHostWhenPublicURLUnset(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode("test-secret")
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Both URLs intentionally empty — simulates a cloud deploy that
	// hasn't set PAD_MCP_PUBLIC_URL / PAD_AUTH_SERVER_URL yet.
	srv.SetMCPTransport(stub, "", "")

	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
	req.Host = "mcp.test.local"
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
	wwwAuth := rr.Header().Get("WWW-Authenticate")
	if wwwAuth == "" {
		t.Fatal("WWW-Authenticate must be set even when PAD_MCP_PUBLIC_URL is unset; got empty header")
	}
	wantSubstring := `resource_metadata="https://mcp.test.local/.well-known/oauth-protected-resource"`
	if !strings.Contains(wwwAuth, wantSubstring) {
		t.Errorf("expected fallback resource_metadata derived from r.Host, got %q", wwwAuth)
	}
}

// TestMCP_ReadScopedPAT_RejectedOnWriteTool pins the security fix
// for Codex review #369 round 1 finding 1: a PAT with scopes
// `["read"]` must NOT be able to drive write tools through /mcp.
// MCPBearerAuth used to skip tokenScopeAllows entirely; the
// dispatcher's synthesized in-process request bypassed TokenAuth's
// chain-level check too, so a read-scoped token could perform writes
// silently. The fix stashes scopes via server.WithTokenScopes in
// MCPBearerAuth and re-checks them per synthesized request in
// HTTPHandlerDispatcher.executeRequest. This test exercises the
// MCPBearerAuth-side stash; the dispatcher enforcement is unit-
// tested in internal/mcp/dispatch_http_test.go.
func TestMCP_ReadScopedPAT_StashesScopesInContext(t *testing.T) {
	srv := testServer(t)

	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "scope-test@example.com", Name: "Scope Tester", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Scope Test WS"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	tok, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{
		Name:        "read-only-pat",
		WorkspaceID: ws.ID,
		Scopes:      `["read"]`,
	}, 30, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// Stub transport reads scopes from context to confirm they
	// arrived. Without the WithTokenScopes call in MCPBearerAuth,
	// the assertion fails — pinning the fix.
	var seenScopes string
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenScopes = TokenScopesFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	srv.SetCloudMode("test-secret")
	srv.SetMCPTransport(stub, "https://mcp.test.example", "https://app.test.example")

	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (auth passes — scope check is dispatcher-side), got %d", rr.Code)
	}
	if seenScopes != `["read"]` {
		t.Errorf("expected scopes to round-trip via WithTokenScopes; got %q want `[\"read\"]`", seenScopes)
	}
}

// TestTokenScopeAllows_PublicWrapper sanity-checks that the exported
// helper preserves the policy (so the dispatcher can rely on it).
// Specifically pins the read-vs-write decision the security finding
// cared about.
func TestTokenScopeAllows_PublicWrapper(t *testing.T) {
	cases := []struct {
		name           string
		scopes, method string
		want           bool
	}{
		{"read scope on GET", `["read"]`, "GET", true},
		{"read scope on POST", `["read"]`, "POST", false},
		{"read scope on PATCH", `["read"]`, "PATCH", false},
		{"read scope on DELETE", `["read"]`, "DELETE", false},
		{"write scope on POST", `["write"]`, "POST", true},
		{"wildcard on POST", `["*"]`, "POST", true},
		{"empty scopes (legacy) on POST", "", "POST", true},
		{"unparseable denies", `not-json`, "GET", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TokenScopeAllows(tc.scopes, tc.method, "/api/v1/anything")
			if got != tc.want {
				t.Errorf("TokenScopeAllows(%q, %s) = %v, want %v", tc.scopes, tc.method, got, tc.want)
			}
		})
	}
}

// mcpEnabledTestServer builds a Server with cloud mode + a stub
// streamable-HTTP transport so tests targeting auth / discovery /
// routing don't need to drive a full MCP handshake. The stub returns
// 200 with an empty JSON-RPC success — sufficient to confirm "the
// auth chain let the request through."
func mcpEnabledTestServer(t *testing.T) *Server {
	t.Helper()
	srv := testServer(t)
	srv.SetCloudMode("test-secret")
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	})
	srv.SetMCPTransport(stub, "https://mcp.test.example", "https://app.test.example")
	return srv
}

// sliceEqual reports whether a and b have the same elements in the
// same order. Local tiny helper avoids pulling reflect.DeepEqual into
// each assertion at call sites; the order assertion is intentional —
// the MCP spec doesn't mandate scope order, but pinning it gives us
// stable tests + matches the order our handler produces.
func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
