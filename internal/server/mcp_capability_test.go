package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
	"github.com/PerpetualSoftware/pad/internal/oauth"
)

// testHTTPSEndpoints is an https self-host's resolved MCP addressing: a
// usable origin with the two URLs the test harness has always used.
func testHTTPSEndpoints() config.MCPEndpoints {
	return config.MCPEndpoints{
		Origin:        "https://pad.test.example",
		OriginVar:     "PAD_URL",
		ResourceURL:   testCanonicalAudience,
		AuthServerURL: testAuthServerURL,
	}
}

// TestE2E_HTTPSSelfHostFlow is PLAN-2310 DR-4's acceptance for an https
// self-host, off cloud, with MCP turned on through the stored setting:
// the 401 points at a metadata document that exists, discovery names the
// configured URLs, and DCR, authorize and token complete for the audience
// <origin>/mcp, whose access token then reaches /mcp as its user.
func TestE2E_HTTPSSelfHostFlow(t *testing.T) {
	t.Parallel()
	const (
		origin   = "https://pad.selfhost.example"
		audience = origin + "/mcp"
	)
	srv := testServer(t)
	srv.SetMCPConfig(config.MCPEndpoints{Origin: origin, OriginVar: "PAD_URL", ResourceURL: audience, AuthServerURL: origin}, nil)
	if err := srv.store.SetPlatformSetting(settingMCPEnabled, "true"); err != nil {
		t.Fatalf("SetPlatformSetting: %v", err)
	}
	transport := &mcpStubTransport{}
	srv.SetMCPTransport(http.HandlerFunc(transport.serve), audience, origin, nil)
	o, err := oauth.NewServer(oauth.Config{Store: srv.store, HMACSecret: bytes32ForTest(), AllowedAudience: audience})
	if err != nil {
		t.Fatalf("oauth.NewServer: %v", err)
	}
	srv.SetOAuthServer(o)
	if srv.IsCloud() {
		t.Fatal("this test is about self-host; cloud mode must be off")
	}
	user, sessionToken := loginTestUser(t, srv)

	rr := doRequest(srv, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	const wantChallenge = `Bearer realm="pad", resource_metadata="` + origin + `/.well-known/oauth-protected-resource/mcp"`
	if rr.Code != http.StatusUnauthorized || rr.Header().Get("WWW-Authenticate") != wantChallenge {
		t.Fatalf("401 challenge: status %d, WWW-Authenticate %q, want %q", rr.Code, rr.Header().Get("WWW-Authenticate"), wantChallenge)
	}

	rr = doRequest(srv, "GET", "/.well-known/oauth-protected-resource/mcp", nil)
	var prDoc protectedResourceMetadata
	parseJSON(t, rr, &prDoc)
	if rr.Code != http.StatusOK || prDoc.Resource != audience || !sliceEqual(prDoc.AuthorizationServers, []string{origin}) {
		t.Fatalf("protected-resource doc: status %d, %+v", rr.Code, prDoc)
	}
	rr = doRequest(srv, "GET", "/.well-known/oauth-authorization-server", nil)
	var asDoc map[string]any
	parseJSON(t, rr, &asDoc)
	if rr.Code != http.StatusOK || asDoc["issuer"] != origin || asDoc["token_endpoint"] != origin+"/oauth/token" {
		t.Fatalf("auth-server doc: status %d, %v", rr.Code, asDoc)
	}

	clientID := registerTestClient(t, srv, "https://app.test/cb")
	csrfTok := readCSRFFromCookieFor(t, srv, sessionToken, audience)
	tokens := runAuthCodeFlowFor(t, srv, sessionToken, csrfTok, clientID,
		"verifier-selfhost-quick-brown-fox-jumps-over-the-lazy-dog", audience)
	access, _ := tokens["access_token"].(string)
	if access == "" {
		t.Fatalf("no access token: %v", tokens)
	}

	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !transport.Called || transport.SeenUserID != user.ID {
		t.Fatalf("/mcp with the access token: status %d, reached=%v, user %q want %q (body %s)",
			rec.Code, transport.Called, transport.SeenUserID, user.ID, rec.Body.String())
	}
}

// TestProtectedResourceMetadataURL pins RFC 9728 §3.1's shape for the
// 401's resource_metadata, including byte identity for a path-less
// resource in any spelling (cloud's audience is compared byte-for-byte
// and its historical header was resource + the well-known suffix).
func TestProtectedResourceMetadataURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://mcp.getpad.dev":        "https://mcp.getpad.dev/.well-known/oauth-protected-resource",
		"https://mcp.getpad.dev/":       "https://mcp.getpad.dev/.well-known/oauth-protected-resource",
		"HTTPS://MCP.GetPad.dev:443":    "HTTPS://MCP.GetPad.dev:443/.well-known/oauth-protected-resource",
		"https://pad.example.com/mcp":   "https://pad.example.com/.well-known/oauth-protected-resource/mcp",
		"http://pad.lan:7777/mcp/":      "http://pad.lan:7777/.well-known/oauth-protected-resource/mcp",
		"https://pad.example.com/a/mcp": "https://pad.example.com/.well-known/oauth-protected-resource/a/mcp",
		"":                              "",
	} {
		if got := protectedResourceMetadataURL(in); got != want {
			t.Errorf("protectedResourceMetadataURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// wireOAuthForTest makes oauthAvailable true the way production does on
// an https self-host (PLAN-2310 DR-1, DR-4): https addressing, the MCP
// setting forced on through the environment override, and an OAuth
// server constructed for the resolved audience. It works on a cloud
// server too, where the addressing is simply unused by the predicate.
func wireOAuthForTest(t *testing.T, srv *Server) *oauth.Server {
	t.Helper()
	on := true
	srv.SetMCPConfig(testHTTPSEndpoints(), &on)
	o, err := newTestOAuthServer(t, srv)
	if err != nil {
		t.Fatalf("oauth.NewServer: %v", err)
	}
	srv.SetOAuthServer(o)
	return o
}
