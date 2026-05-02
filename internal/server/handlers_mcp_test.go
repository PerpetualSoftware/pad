package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/oauth"
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

// TestMCP_AuthServerMetadata_MountedAndGated confirms the RFC 8414
// authorization-server discovery doc is mounted by the cloud-mode
// route group AND fail-loud-503s when the OAuth server isn't
// wired (Codex review #372 round 3 — the MCP routes mount the
// discovery doc, but the /oauth/* handlers only mount when
// SetOAuthServer is called; without OAuth the doc must NOT
// advertise live URLs that 404).
//
// mcpEnabledTestServer mounts MCP transport but NOT OAuth, so 503
// is the correct fail-loud response. The full happy-path 200
// shape assertions live in TestOAuth_AuthorizationServerMetadata_PopulatedShape
// (which uses oauthEnabledTestServer).
func TestMCP_AuthServerMetadata_MountedAndGated(t *testing.T) {
	srv := mcpEnabledTestServer(t)

	rr := doRequest(srv, "GET", "/.well-known/oauth-authorization-server", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 (MCP mounted but OAuth disabled), got %d (body: %s)", rr.Code, rr.Body.String())
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

// =====================================================================
// /mcp ↔ OAuth integration (sub-PR E, TASK-1027)
// =====================================================================

// mcpAndOAuthEnabledTestServer builds a server with BOTH the MCP
// transport stub AND the fosite-backed OAuth server wired. The two
// surfaces share the same canonical audience (testCanonicalAudience
// from handlers_oauth_test.go), so a token issued by the OAuth flow
// is valid against the MCP gate.
//
// Stub captures the user attached to the request context so tests
// can assert "the OAuth bearer routed the right user through" in
// addition to "the auth gate accepted the bearer."
func mcpAndOAuthEnabledTestServer(t *testing.T) (srv *Server, transport *mcpStubTransport) {
	t.Helper()
	srv = testServer(t)
	srv.SetCloudMode("test-secret")

	transport = &mcpStubTransport{}
	srv.SetMCPTransport(http.HandlerFunc(transport.serve),
		strings.TrimSuffix(testCanonicalAudience, "/mcp"),
		testAuthServerURL)

	o, err := newTestOAuthServer(t, srv)
	if err != nil {
		t.Fatalf("oauth.NewServer: %v", err)
	}
	srv.SetOAuthServer(o)
	return srv, transport
}

// mcpStubTransport is a recording stub for the /mcp Streamable-HTTP
// surface. Captures the user attached to context so tests can
// distinguish "auth gate let the request through" from "auth gate
// attached the right identity." A misrouted request shows up as
// SeenUserID == "" (stub never reached / no user in context).
type mcpStubTransport struct {
	Called      bool
	SeenUserID  string
	SeenScopes  string
	SeenIsToken bool
}

func (t *mcpStubTransport) serve(w http.ResponseWriter, r *http.Request) {
	t.Called = true
	if u, ok := CurrentUserFromContext(r.Context()); ok && u != nil {
		t.SeenUserID = u.ID
	}
	t.SeenScopes = TokenScopesFromContext(r.Context())
	t.SeenIsToken = IsAPITokenFromContext(r.Context())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
}

// TestMCP_OAuthAccessToken_Authenticates is the primary happy-path
// test for sub-PR E: a token minted by the full /authorize → /token
// flow authenticates against /mcp, attaches the right user to
// context, and stashes scopes via WithTokenScopes so the in-process
// dispatcher can enforce per-tool checks.
func TestMCP_OAuthAccessToken_Authenticates(t *testing.T) {
	srv, transport := mcpAndOAuthEnabledTestServer(t)
	user, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	tokens := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID,
		"verifier-mcp-quick-brown-fox-jumps-over-lazy-dog-1234")
	access, _ := tokens["access_token"].(string)
	if access == "" {
		t.Fatalf("missing access token: %v", tokens)
	}

	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1}`))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 from OAuth-authed /mcp, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if !transport.Called {
		t.Fatal("transport stub never called — auth or routing failed silently")
	}
	if transport.SeenUserID != user.ID {
		t.Errorf("user not attached: got %q, want %q", transport.SeenUserID, user.ID)
	}
	if !transport.SeenIsToken {
		t.Error("ctxIsAPIToken should be true for OAuth-authed requests (mirrors PAT path)")
	}
	// Scopes must be the JSON-array form tokenScopeAllows expects.
	// runAuthCodeFlow grants pad:read, so that's what we should see.
	if !strings.Contains(transport.SeenScopes, "pad:read") {
		t.Errorf("scopes not stashed correctly; got %q want JSON array containing pad:read", transport.SeenScopes)
	}
}

// TestMCP_OAuthAccessToken_AudienceMismatch_Rejected pins the
// RFC 8707 anti-replay defense: even if a token is otherwise valid,
// /mcp rejects it when the granted audience doesn't include the
// resource server's canonical URL.
//
// Strategy: mint a token normally (audience matches the OAuth
// server's canonical = testCanonicalAudience), then swap the
// Server's oauthServer for one with a DIFFERENT canonical so the
// MCP gate's AllowedAudience() returns a value that doesn't match
// the token's stored aud claim. fosite.IntrospectToken still
// validates the token (storage isn't audience-bound), but our
// post-introspection check in handleMCPOAuthAuth rejects.
//
// This is belt-and-suspenders: in normal operation,
// audienceMatchingStrategy on the grant side refuses to mint
// tokens for non-canonical audiences. The resource-server check
// here defends against scenarios where the auth server is
// compromised, misconfigured, or shared across multiple resources.
func TestMCP_OAuthAccessToken_AudienceMismatch_Rejected(t *testing.T) {
	// Step 1: build a normal MCP+OAuth server matched to
	// testCanonicalAudience and mint a token through the standard
	// helper.
	srv, _ := mcpAndOAuthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	tokens := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID,
		"verifier-audmismatch-quick-brown-fox-jumps-over-lazy-")
	access, _ := tokens["access_token"].(string)
	if access == "" {
		t.Fatalf("missing access token: %v", tokens)
	}

	// Step 2: swap in an OAuth server with a different canonical
	// audience. The token we just minted has
	// granted_audience=testCanonicalAudience, but the new gate
	// expects something else.
	mismatched, err := oauth.NewServer(oauth.Config{
		Store:           srv.store,
		HMACSecret:      bytes32ForTest(),
		AllowedAudience: "https://mcp.unrelated.example/mcp",
	})
	if err != nil {
		t.Fatalf("oauth.NewServer (mismatched): %v", err)
	}
	srv.SetOAuthServer(mismatched)

	// Step 3: try to use the token at /mcp — must reject.
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+access)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("audience mismatch must produce 401; got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// WWW-Authenticate must still point at discovery so the client
	// knows how to recover.
	if rr.Header().Get("WWW-Authenticate") == "" {
		t.Error("audience-mismatch 401 must still emit WWW-Authenticate")
	}
}

// TestMCP_OAuthRefreshToken_RejectedAtMCP pins the rule that refresh
// tokens are NOT valid bearers for resource calls. RFC 6749 §1.5
// distinguishes refresh tokens (used to obtain access tokens) from
// access tokens (used to access protected resources). A client that
// accidentally puts the refresh token in the Authorization header
// must NOT authenticate.
func TestMCP_OAuthRefreshToken_RejectedAtMCP(t *testing.T) {
	srv, _ := mcpAndOAuthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	tokens := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID,
		"verifier-refresh-token-quick-brown-fox-jumps-over-laz")
	refresh, _ := tokens["refresh_token"].(string)
	if refresh == "" {
		t.Fatalf("missing refresh token: %v", tokens)
	}

	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+refresh)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("refresh token must NOT authenticate /mcp; got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestMCP_RevokedOAuthToken_Rejected pins the rule that revocation
// takes effect immediately at the resource server. Mints a token,
// revokes it via /oauth/revoke, then confirms /mcp rejects.
//
// This is the primary security guarantee of the family-revocation
// flow that sub-PRs A + D wired: a revoke call must invalidate
// every bearer in the chain.
func TestMCP_RevokedOAuthToken_Rejected(t *testing.T) {
	srv, _ := mcpAndOAuthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	tokens := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID,
		"verifier-revoke-mcp-quick-brown-fox-jumps-over-lazy-d")
	access, _ := tokens["access_token"].(string)
	if access == "" {
		t.Fatalf("missing access token: %v", tokens)
	}

	// Revoke via /oauth/revoke (RFC 7009). Public client → only
	// client_id needed.
	rrRevoke := postOAuthForm(srv, "/oauth/revoke", url.Values{
		"token":     {access},
		"client_id": {clientID},
	})
	if rrRevoke.Code != http.StatusOK {
		t.Fatalf("revoke: expected 200, got %d (body: %s)", rrRevoke.Code, rrRevoke.Body.String())
	}

	// Use the revoked token at /mcp — must 401.
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+access)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("revoked token must NOT authenticate /mcp; got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestMCP_PATPath_StillWorks regresses the original TASK-950 PAT
// auth path. Sub-PR E added an OAuth branch; the PAT branch must
// keep working unchanged so existing CLI / Claude-Code-on-self-host
// users don't see a regression.
func TestMCP_PATPath_StillWorks(t *testing.T) {
	srv, transport := mcpAndOAuthEnabledTestServer(t)

	user, err := srv.store.CreateUser(models.UserCreate{
		Email:    "pat-regression@example.com",
		Name:     "PAT Regression",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "PAT Test WS"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	tok, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{
		Name: "regression-token", WorkspaceID: ws.ID,
	}, 30, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("PAT path broken by OAuth integration: got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if transport.SeenUserID != user.ID {
		t.Errorf("PAT path should attach user; got %q want %q", transport.SeenUserID, user.ID)
	}
}

// TestMCP_OAuthScopeReadOnly_StashesPadReadScope confirms that an
// OAuth token granted only `pad:read` reaches the in-process
// dispatcher with that scope stashed in the canonical JSON-array
// form. Pairs with the tokenScopeAllows pad:* extension — together
// they ensure that an OAuth read-only token can NOT drive write
// MCP tools.
//
// This test exercises only the MCPBearerAuth-side stash; the
// dispatcher-side enforcement is covered by
// TestTokenScopeAllows_PublicWrapper / token_scopes_test.go.
func TestMCP_OAuthScopeReadOnly_StashesPadReadScope(t *testing.T) {
	srv, transport := mcpAndOAuthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	tokens := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID,
		"verifier-readonly-quick-brown-fox-jumps-over-lazy-dog")
	access, _ := tokens["access_token"].(string)

	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+access)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (auth passes — scope check is dispatcher-side), got %d", rr.Code)
	}
	// runAuthCodeFlow grants `pad:read` (single scope). Stash should
	// be the JSON-array equivalent.
	want := `["pad:read"]`
	if transport.SeenScopes != want {
		t.Errorf("scopes stash: got %q, want %q", transport.SeenScopes, want)
	}
}

// TestOAuthScopesToJSON_FailClosedOnEmpty pins Codex review #375
// round 1: OAuth's `scope` parameter is optional per RFC 6749 §3.3,
// so fosite can hand back a token with empty granted scopes. Without
// the fail-closed mapping in oauthScopesToJSON, that empty list
// would serialize to `[]` — which tokenScopeAllows interprets as
// the legacy "unrestricted" PAT shape, granting write access.
//
// The fix maps empty → JSON `null`, which tokenScopeAllows denies
// via its "scopes == nil" branch. This test asserts both halves of
// the contract: oauthScopesToJSON produces null, and tokenScopeAllows
// then denies for every method.
func TestOAuthScopesToJSON_FailClosedOnEmpty(t *testing.T) {
	got := oauthScopesToJSON(nil)
	if got != "null" {
		t.Errorf("nil scopes: got %q, want %q", got, "null")
	}
	gotEmpty := oauthScopesToJSON([]string{})
	if gotEmpty != "null" {
		t.Errorf("empty scopes: got %q, want %q", gotEmpty, "null")
	}

	// Routing through tokenScopeAllows must produce false for all
	// methods — pin the end-to-end contract, not just the helper.
	for _, method := range []string{
		http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete,
	} {
		if tokenScopeAllows(got, method, "/api/v1/items") {
			t.Errorf("empty OAuth scopes must deny %s; tokenScopeAllows returned true", method)
		}
	}

	// Sanity: non-empty OAuth scopes round-trip the canonical JSON
	// form (regression — the fail-closed path must NOT extend to
	// the populated case).
	if got := oauthScopesToJSON([]string{"pad:read"}); got != `["pad:read"]` {
		t.Errorf("populated scopes: got %q, want %q", got, `["pad:read"]`)
	}
}

// =====================================================================
// /api/v1/oauth/clients/{id}/public-info (sub-PR E, TASK-1027)
// =====================================================================

// TestOAuthClientPublicInfo_HappyPath confirms the consent-screen
// support endpoint returns the four whitelisted fields and matches
// what was registered.
func TestOAuthClientPublicInfo_HappyPath(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)

	// Register via the public DCR endpoint so the test exercises the
	// full create → read round-trip a real client would do.
	rrReg := doRequest(srv, "POST", "/oauth/register", map[string]any{
		"client_name":   "Test Connector",
		"redirect_uris": []string{"https://app.test/cb", "claude://oauth/callback"},
		"logo_uri":      "https://test.example/logo.png",
	})
	if rrReg.Code != http.StatusCreated {
		t.Fatalf("register: %d (body: %s)", rrReg.Code, rrReg.Body.String())
	}
	var regResp map[string]any
	parseJSON(t, rrReg, &regResp)
	clientID, _ := regResp["client_id"].(string)
	if clientID == "" {
		t.Fatal("DCR returned empty client_id")
	}

	rr := doAuthedRequest(srv, "GET",
		"/api/v1/oauth/clients/"+clientID+"/public-info", nil, sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("public-info: %d (body: %s)", rr.Code, rr.Body.String())
	}
	var info map[string]any
	parseJSON(t, rr, &info)

	if got, _ := info["client_id"].(string); got != clientID {
		t.Errorf("client_id: got %q want %q", got, clientID)
	}
	if got, _ := info["client_name"].(string); got != "Test Connector" {
		t.Errorf("client_name: got %q want Test Connector", got)
	}
	if got, _ := info["logo_uri"].(string); got != "https://test.example/logo.png" {
		t.Errorf("logo_uri: got %q", got)
	}
	uris, _ := info["redirect_uris"].([]any)
	if len(uris) != 2 {
		t.Errorf("redirect_uris: got %v, want 2 entries", uris)
	}

	// Leak surface: nothing else should be present.
	for _, leakField := range []string{
		"grant_types", "response_types", "token_endpoint_auth_method",
		"scope", "client_id_issued_at",
	} {
		if _, ok := info[leakField]; ok {
			t.Errorf("public-info leaked %q (whitelist surface only); got %v", leakField, info[leakField])
		}
	}
}

// TestOAuthClientPublicInfo_UnknownClient_404 confirms an unknown
// client ID gets 404, not 401 (caller IS authenticated; the resource
// just doesn't exist).
func TestOAuthClientPublicInfo_UnknownClient_404(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)

	rr := doAuthedRequest(srv, "GET",
		"/api/v1/oauth/clients/no-such-client/public-info", nil, sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown client, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestOAuthClientPublicInfo_Unauthenticated_401 confirms the auth
// gate works — reading client metadata requires a session, so an
// attacker can't enumerate registered clients pre-login.
//
// Setup quirk: pad's auth bootstrap path lets requests through when
// no users exist (setup_required mode for first-admin creation). We
// have to seed a user so the system is past setup, even though we
// don't use that user's session in the actual request.
func TestOAuthClientPublicInfo_Unauthenticated_401(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	// Seed a user so the system isn't in setup_required mode (which
	// auto-allows everything). Discard the session — we want this
	// request to land WITHOUT auth to verify the gate.
	_, _ = loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	rr := doRequest(srv, "GET",
		"/api/v1/oauth/clients/"+clientID+"/public-info", nil)
	if rr.Code == http.StatusOK {
		t.Errorf("public-info must require auth; got 200 (body: %s)", rr.Body.String())
	}
}

// TestOAuthClientPublicInfo_NotMountedOutsideCloudMode confirms the
// cloud-mode gate covers the endpoint — self-hosted deployments
// without the OAuth surface don't expose it.
func TestOAuthClientPublicInfo_NotMountedOutsideCloudMode(t *testing.T) {
	srv := testServer(t)
	_, sessionToken := loginTestUser(t, srv)

	rr := doAuthedRequest(srv, "GET",
		"/api/v1/oauth/clients/anything/public-info", nil, sessionToken)
	// requireCloudMode returns 404 outside cloud mode.
	if rr.Code == http.StatusOK {
		t.Errorf("public-info must NOT be reachable outside cloud mode; got 200")
	}
}

// =====================================================================
// E2E simulation: discovery → DCR → authorize → token → /mcp call
// =====================================================================

// TestE2E_ClaudeDesktopFlow simulates the sequence Claude Desktop
// (and any RFC 9728 / RFC 7591 / OAuth 2.1 client) walks on first
// connect:
//
//  1. POST /mcp without a token → 401 + WWW-Authenticate pointing
//     at the protected-resource discovery doc.
//  2. GET .well-known/oauth-protected-resource → resource +
//     authorization_servers.
//  3. GET .well-known/oauth-authorization-server → endpoint URLs.
//  4. POST /oauth/register (DCR) → client_id.
//  5. POST /oauth/authorize/decide (consent stub) → authorization
//     code.
//  6. POST /oauth/token → access + refresh tokens.
//  7. POST /mcp with the access token → 200 + transport reached
//     with the right user context.
//
// Without sub-PR E, the final /mcp call would 401 because
// MCPBearerAuth only validated PATs. The full chain pinned here is
// what makes the OAuth surface end-to-end usable; if any single
// hop breaks, the whole connector experience breaks for every
// MCP-aware client. Worth one slow integration test.
func TestE2E_ClaudeDesktopFlow(t *testing.T) {
	srv, transport := mcpAndOAuthEnabledTestServer(t)
	user, sessionToken := loginTestUser(t, srv)

	// 1. Unauthenticated /mcp → 401 + WWW-Authenticate.
	rr1 := doRequest(srv, "POST", "/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
	})
	if rr1.Code != http.StatusUnauthorized {
		t.Fatalf("step 1: expected 401, got %d", rr1.Code)
	}
	if !strings.Contains(rr1.Header().Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatalf("step 1: missing resource_metadata pointer; got %q", rr1.Header().Get("WWW-Authenticate"))
	}

	// 2. Protected-resource discovery doc.
	rr2 := doRequest(srv, "GET", "/.well-known/oauth-protected-resource", nil)
	if rr2.Code != http.StatusOK {
		t.Fatalf("step 2: discovery doc %d", rr2.Code)
	}

	// 3. Authorization-server discovery doc.
	rr3 := doRequest(srv, "GET", "/.well-known/oauth-authorization-server", nil)
	if rr3.Code != http.StatusOK {
		t.Fatalf("step 3: auth-server doc %d", rr3.Code)
	}
	var asDoc map[string]any
	parseJSON(t, rr3, &asDoc)
	regEndpoint, _ := asDoc["registration_endpoint"].(string)
	if !strings.Contains(regEndpoint, "/oauth/register") {
		t.Fatalf("step 3: bad registration_endpoint %q", regEndpoint)
	}

	// 4. DCR.
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	// 5 + 6: full authorize → token via the helper.
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	tokens := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID,
		"verifier-e2e-quick-brown-fox-jumps-over-the-lazy-dog")
	access, _ := tokens["access_token"].(string)
	refresh, _ := tokens["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("step 6: missing tokens %v", tokens)
	}

	// 7. /mcp with the access token.
	req := httptest.NewRequest("POST", "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("step 7: /mcp expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if !transport.Called {
		t.Fatal("step 7: transport not reached")
	}
	if transport.SeenUserID != user.ID {
		t.Errorf("step 7: user mismatch — got %q want %q", transport.SeenUserID, user.ID)
	}
}

// =====================================================================
// Workspace allow-list gate (TASK-953)
// =====================================================================

// TestWorkspaceAllowList_AllowsListedSlug verifies the happy path:
// an OAuth token whose consent allow-list includes "alpha" passes
// the gate when reaching /api/v1/workspaces/alpha/* — same as the
// pre-TASK-953 behaviour for tokens whose allow-list isn't set.
//
// Drives the full chain: OAuth token via /mcp's MCPBearerAuth
// stashes the allow-list in context; the synthesized API request
// inherits it; RequireWorkspaceAccess reads it; the workspace
// resolves; the gate matches; the standard membership check runs.
func TestWorkspaceAllowList_AllowsListedSlug(t *testing.T) {
	srv, _ := mcpAndOAuthEnabledTestServer(t)
	user, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	// Seed two workspaces; user is owner of both.
	mustSeedWorkspaceWithRole(t, srv, user.ID, "Alpha", "alpha", "owner")
	mustSeedWorkspaceWithRole(t, srv, user.ID, "Beta", "beta", "owner")

	// Mint a token with allow-list = [alpha] (NOT beta).
	verifier := "verifier-allow-alpha-quick-brown-fox-jumps-over-laz"
	challenge := s256Challenge(verifier)
	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"pad:read"},
		"audience":              {testCanonicalAudience},
		"state":                 {"allow-alpha-state"},
		"decision":              {"approve"},
		"csrf_token":            {csrfTok},
		"capability_tier":       {"read"},
		"allowed_workspaces":    {"alpha"},
	}
	access := mintOAuthTokenForTest(t, srv, sessionToken, csrfTok, clientID, verifier, form)

	// Hit /api/v1/workspaces/alpha — inside allow-list, should
	// pass the gate. Use the dispatcher path: drive through /mcp's
	// MCPBearerAuth so the context plumbing matches production.
	rrAlpha := callWorkspaceViaOAuth(t, srv, access, "alpha")
	if rrAlpha.Code == http.StatusForbidden {
		t.Errorf("alpha (in allow-list) must NOT 403; got %d (body: %s)",
			rrAlpha.Code, rrAlpha.Body.String())
	}
}

// TestWorkspaceAllowList_DeniesUnlistedSlug pins the security
// invariant: a workspace not in the consent allow-list must be
// rejected with 403 even when the user is a member of it. The
// user's choice at consent time is binding.
func TestWorkspaceAllowList_DeniesUnlistedSlug(t *testing.T) {
	srv, _ := mcpAndOAuthEnabledTestServer(t)
	user, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	mustSeedWorkspaceWithRole(t, srv, user.ID, "Alpha", "alpha", "owner")
	mustSeedWorkspaceWithRole(t, srv, user.ID, "Beta", "beta", "owner")

	// Token's allow-list contains ONLY "alpha".
	verifier := "verifier-deny-beta-quick-brown-fox-jumps-over-lazy-"
	challenge := s256Challenge(verifier)
	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"pad:read"},
		"audience":              {testCanonicalAudience},
		"state":                 {"deny-beta-state"},
		"decision":              {"approve"},
		"csrf_token":            {csrfTok},
		"capability_tier":       {"read"},
		"allowed_workspaces":    {"alpha"},
	}
	access := mintOAuthTokenForTest(t, srv, sessionToken, csrfTok, clientID, verifier, form)

	// Beta is NOT in the allow-list — must 403 even though the
	// user is owner of beta.
	rrBeta := callWorkspaceViaOAuth(t, srv, access, "beta")
	if rrBeta.Code != http.StatusForbidden {
		t.Errorf("beta (not in allow-list) must 403; got %d (body: %s)",
			rrBeta.Code, rrBeta.Body.String())
	}
}

// TestWorkspaceAllowList_WildcardAllowsAnyMembership pins the
// wildcard semantics: ["*"] grants access to every workspace the
// user is a member of, no per-slug gate. Standard membership is
// still the boundary.
func TestWorkspaceAllowList_WildcardAllowsAnyMembership(t *testing.T) {
	srv, _ := mcpAndOAuthEnabledTestServer(t)
	user, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	mustSeedWorkspaceWithRole(t, srv, user.ID, "Alpha", "alpha", "owner")
	mustSeedWorkspaceWithRole(t, srv, user.ID, "Beta", "beta", "editor")

	verifier := "verifier-wildcard-quick-brown-fox-jumps-over-lazy-d"
	challenge := s256Challenge(verifier)
	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"pad:read"},
		"audience":              {testCanonicalAudience},
		"state":                 {"wildcard-state"},
		"decision":              {"approve"},
		"csrf_token":            {csrfTok},
		"capability_tier":       {"read"},
		"allowed_workspaces":    {"*"},
	}
	access := mintOAuthTokenForTest(t, srv, sessionToken, csrfTok, clientID, verifier, form)

	for _, slug := range []string{"alpha", "beta"} {
		rr := callWorkspaceViaOAuth(t, srv, access, slug)
		if rr.Code == http.StatusForbidden {
			t.Errorf("wildcard token must NOT 403 on %s; got %d (body: %s)",
				slug, rr.Code, rr.Body.String())
		}
	}
}

// TestWorkspaceAllowList_LiveMembershipRevocation pins the live-
// membership check: a token whose allow-list includes "alpha"
// stops working when the user's membership in "alpha" is revoked
// — even though the token's stored allow-list still includes the
// slug. The membership table is the binding gate (RequireWorkspaceAccess
// runs after the allow-list check).
//
// This is the "if your membership changes, this connection's
// permissions change immediately" property from the PLAN-943 design
// (TASK-952's consent-screen footer references this).
func TestWorkspaceAllowList_LiveMembershipRevocation(t *testing.T) {
	srv, _ := mcpAndOAuthEnabledTestServer(t)
	user, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	ws := mustSeedWorkspaceWithRole(t, srv, user.ID, "Alpha", "alpha", "owner")

	verifier := "verifier-revoke-quick-brown-fox-jumps-over-the-lazy"
	challenge := s256Challenge(verifier)
	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"pad:read"},
		"audience":              {testCanonicalAudience},
		"state":                 {"revoke-state"},
		"decision":              {"approve"},
		"csrf_token":            {csrfTok},
		"capability_tier":       {"read"},
		"allowed_workspaces":    {"alpha"},
	}
	access := mintOAuthTokenForTest(t, srv, sessionToken, csrfTok, clientID, verifier, form)

	// Sanity check — token works against alpha while membership
	// is intact.
	rrBefore := callWorkspaceViaOAuth(t, srv, access, "alpha")
	if rrBefore.Code == http.StatusForbidden {
		t.Fatalf("baseline: token should pass while user is owner of alpha; got %d", rrBefore.Code)
	}

	// Revoke membership. Reuse the store directly so the test
	// doesn't depend on an admin-only API path.
	if err := srv.store.RemoveWorkspaceMember(ws.ID, user.ID); err != nil {
		t.Fatalf("RemoveWorkspaceMember: %v", err)
	}

	// Same token now — must NOT succeed (live membership check).
	// resolveWorkspace short-circuits for non-admin users by scoping
	// slug lookup to the user's memberships, so post-revocation the
	// response is actually 404 (workspace doesn't resolve) rather
	// than 403 (workspace resolves but member is missing). Both
	// are valid "no access" outcomes — 404 leaks less than 403,
	// since the attacker can't tell whether the workspace exists.
	// The TASK-953 design lists 403 as the canonical response, but
	// either denial mode satisfies "membership revocation cuts the
	// connection immediately."
	rrAfter := callWorkspaceViaOAuth(t, srv, access, "alpha")
	if rrAfter.Code != http.StatusForbidden && rrAfter.Code != http.StatusNotFound {
		t.Errorf("post-revocation: must 403 or 404 (live membership check); got %d (body: %s)",
			rrAfter.Code, rrAfter.Body.String())
	}
}

// TestWorkspaceAllowList_PATPathUnaffected regresses the PAT auth
// path: a Personal Access Token doesn't carry a workspace allow-
// list (the consent UI is OAuth-only), so its requests must NOT
// hit the gate. PATs that hit /api/v1/workspaces/<slug>/* should
// continue to work the same as they did pre-TASK-953.
func TestWorkspaceAllowList_PATPathUnaffected(t *testing.T) {
	srv, _ := mcpAndOAuthEnabledTestServer(t)
	user := mustCreateUserForTest(t, srv, "pat-no-gate@example.com")
	mustSeedWorkspaceWithRole(t, srv, user.ID, "Alpha", "alpha", "owner")

	// Workspace-scoped PAT (the Y2025 model — workspace required for the
	// FK but the user is the auth principal).
	wsForToken, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Token WS"})
	if err != nil {
		t.Fatalf("CreateWorkspace (token holder): %v", err)
	}
	tok, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{
		Name: "pat-no-gate", WorkspaceID: wsForToken.ID,
	}, 30, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// PAT against /api/v1/workspaces/alpha must not be gate-rejected.
	req := httptest.NewRequest("GET", "/api/v1/workspaces/alpha/", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code == http.StatusForbidden {
		t.Errorf("PAT path must NOT be subject to the OAuth allow-list gate; got 403 (body: %s)", rr.Body.String())
	}
}

// TestWorkspaceAllowList_TierTimesRole_WriteByViewer pins the
// central tier×role security claim from PLAN-943: a token whose
// capability tier is `pad:write` does NOT bypass the user's
// workspace role. If the user is only a Viewer in workspace foo,
// a `pad:write` token must NOT be able to create items there even
// though the token's tier is sufficient — the role gate kicks in
// at the per-handler level (requireEditPermission via
// workspaceRole(r)).
//
// This is the test the task spec calls out specifically:
// "pad:write action + Viewer → 403 (capability allows but role
// doesn't)."
func TestWorkspaceAllowList_TierTimesRole_WriteByViewer(t *testing.T) {
	srv, _ := mcpAndOAuthEnabledTestServer(t)
	user, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	// User is a VIEWER in workspace alpha — restrictive role.
	ws := mustSeedWorkspaceWithRole(t, srv, user.ID, "Alpha", "alpha", "viewer")

	// Seed a collection so the POST has a valid target.
	if _, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Slug:   "tasks",
		Schema: `{"fields":[]}`,
	}); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	// Mint a pad:write tier token, allow-list = [alpha]. Capability
	// tier alone passes tokenScopeAllows for POST.
	verifier := "verifier-tier-role-quick-brown-fox-jumps-over-lazy-"
	challenge := s256Challenge(verifier)
	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"pad:read pad:write"},
		"audience":              {testCanonicalAudience},
		"state":                 {"tier-role-state"},
		"decision":              {"approve"},
		"csrf_token":            {csrfTok},
		"capability_tier":       {"write"},
		"allowed_workspaces":    {"alpha"},
	}
	access := mintOAuthTokenForTest(t, srv, sessionToken, csrfTok, clientID, verifier, form)

	// POST a new item under alpha/tasks. Token's tier is write
	// (passes tokenScopeAllows), token's allow-list includes alpha
	// (passes the new gate), user is a viewer in alpha (must
	// trigger requireEditPermission's role check → 403).
	urlPath := "/api/v1/workspaces/alpha/collections/tasks/items"
	body := strings.NewReader(`{"title":"should be denied"}`)
	req := httptest.NewRequest("POST", urlPath, body)
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api := req.Clone(r.Context())
		api.Header.Del("Authorization")
		srv.ServeHTTP(w, api)
	})
	rr := httptest.NewRecorder()
	srv.MCPBearerAuth(inner).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("pad:write tier × viewer role MUST 403 on item create; got %d (body: %s)",
			rr.Code, rr.Body.String())
	}
}

// mintOAuthTokenForTest drives the consent → token exchange and
// returns the access token. Lightly customizable via the form arg
// so callers can vary the consent payload (allow-list, tier, etc.).
//
// Centralizes the boilerplate that would otherwise repeat across
// every workspace-allow-list test.
func mintOAuthTokenForTest(t *testing.T, srv *Server, sessionToken, csrfTok, clientID, verifier string, form url.Values) string {
	t.Helper()

	rrDecide := postFormWithCookie(srv, "/oauth/authorize/decide", form, sessionToken, csrfTok)
	if rrDecide.Code != http.StatusSeeOther && rrDecide.Code != http.StatusFound {
		t.Fatalf("decide: expected 303/302, got %d (body: %s)", rrDecide.Code, rrDecide.Body.String())
	}
	cbURL, _ := url.Parse(rrDecide.Header().Get("Location"))
	code := cbURL.Query().Get("code")
	if code == "" {
		t.Fatalf("missing code in callback: %s", rrDecide.Header().Get("Location"))
	}

	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {"https://app.test/cb"},
		"code_verifier": {verifier},
		"audience":      {testCanonicalAudience},
	}
	trr := postOAuthForm(srv, "/oauth/token", tokenForm)
	if trr.Code != http.StatusOK {
		t.Fatalf("token: expected 200, got %d (body: %s)", trr.Code, trr.Body.String())
	}
	var resp map[string]any
	parseJSON(t, trr, &resp)
	access, _ := resp["access_token"].(string)
	if access == "" {
		t.Fatalf("missing access_token: %v", resp)
	}
	return access
}

// callWorkspaceViaOAuth simulates an MCP-driven workspace API call
// by going through MCPBearerAuth's OAuth path. The MCP transport
// stub the test harness wires (mcpAndOAuthEnabledTestServer) DOES
// NOT actually synthesize a workspace request — it just records
// the auth context. So we make the workspace request directly with
// the OAuth bearer, going through the same auth chain a synthesized
// dispatcher request would go through.
//
// This works because /api/v1/workspaces/<slug>/ is mounted on the
// /api/v1 group with TokenAuth + SessionAuth + RequireAuth. We need
// MCPBearerAuth to set up the context, but a direct API request
// with a Bearer token won't hit MCPBearerAuth — it'll hit TokenAuth
// instead, which doesn't currently introspect OAuth tokens. So we
// use httptest.NewRecorder + manually inject the context the
// dispatcher would, then invoke the API via a fresh handler call.
//
// Concretely: this helper invokes MCPBearerAuth as a chain wrapper
// around a simple workspace handler so the OAuth-token validation
// + context plumbing happens once, exactly as production.
func callWorkspaceViaOAuth(t *testing.T, srv *Server, access, workspaceSlug string) *httptest.ResponseRecorder {
	t.Helper()
	// Build a request to /api/v1/workspaces/<slug>/. The path goes
	// through chi's routing for slug extraction in the workspace
	// middleware.
	req := httptest.NewRequest("GET", "/api/v1/workspaces/"+workspaceSlug+"/", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	req.RemoteAddr = "192.0.2.1:1234"

	// MCPBearerAuth normally only runs on /mcp. To exercise the
	// OAuth-token allow-list gate end-to-end, manually wrap the
	// handler chain so MCPBearerAuth runs first and stashes context,
	// then the regular API chain sees the synthesized token state.
	// This mirrors what the dispatcher does in production: the
	// inbound /mcp request sets context, then in-process
	// synthesized requests inherit it.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Build a fresh request for /api with the bearer-derived
		// context preserved — mirror buildHTTPRequest's pattern.
		api := req.Clone(r.Context())
		api.Header.Del("Authorization") // drop bearer; context carries the auth state
		srv.ServeHTTP(w, api)
	})
	rr := httptest.NewRecorder()
	srv.MCPBearerAuth(inner).ServeHTTP(rr, req)
	return rr
}

// mustCreateUserForTest creates a user for tests that need a user
// row but don't need a session. Counterpart to loginTestUser, which
// creates BOTH a user and a session.
func mustCreateUserForTest(t *testing.T, srv *Server, email string) *models.User {
	t.Helper()
	user, err := srv.store.CreateUser(models.UserCreate{
		Email:    email,
		Name:     "Test User",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return user
}

// newTestOAuthServer builds an internal/oauth.Server matched to the
// test harness's canonical audience (testCanonicalAudience). Used by
// mcpAndOAuthEnabledTestServer to wire OAuth alongside the MCP stub.
//
// Extracted so the audience-mismatch test can build the same shape
// with a different audience.
func newTestOAuthServer(t *testing.T, srv *Server) (*oauth.Server, error) {
	t.Helper()
	return oauth.NewServer(oauth.Config{
		Store:           srv.store,
		HMACSecret:      bytes32ForTest(),
		AllowedAudience: testCanonicalAudience,
	})
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
