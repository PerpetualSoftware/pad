package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/oauth"
)

// HTTP-handler tests for the OAuth 2.1 authorization server (PLAN-943
// TASK-1025 sub-PR C).
//
// What's covered here:
//
//   - /.well-known/oauth-authorization-server: replaces the 501 stub
//     from TASK-950 with real RFC 8414 metadata; pin the shape so a
//     future field tweak is caught fast.
//   - /oauth/register (DCR): happy path + each validation gate.
//   - /oauth/authorize: redirects to /login when no session;
//     renders consent stub when logged in; rejects on bad audience.
//   - /oauth/authorize/decide: CSRF-token-required (form-bound);
//     denies on decision=deny; runs fosite + redirects on approve.
//   - /oauth/token: full auth-code → token round-trip via PKCE.
//
// The full Claude-Desktop-end-to-end (DCR → authorize → token →
// /mcp call) lands in sub-PR E's e2e test once MCPBearerAuth gains
// the OAuth introspection branch. This file covers the OAuth-server
// layer in isolation.

const (
	testCanonicalAudience = "https://mcp.test.example/mcp"
	testAuthServerURL     = "https://app.test.example"
)

// oauthEnabledTestServer builds a Server with cloud mode + a real
// internal/oauth.Server backed by the test store. Returns the
// Server so each test can drive doRequest helpers, plus the
// underlying *oauth.Server in case a test wants to seed clients or
// inspect storage state directly.
func oauthEnabledTestServer(t *testing.T) (*Server, *oauth.Server) {
	t.Helper()
	srv := testServer(t)
	srv.SetCloudMode("test-secret")
	// Stub MCP transport so SetMCPTransport's URL fields are populated;
	// the OAuth handlers read mcpAuthServerURL via authServerIssuerURL.
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	srv.SetMCPTransport(stub, testCanonicalAudience[:strings.LastIndex(testCanonicalAudience, "/")], testAuthServerURL)

	o, err := oauth.NewServer(oauth.Config{
		Store:           srv.store,
		HMACSecret:      bytes32ForTest(),
		AllowedAudience: testCanonicalAudience,
	})
	if err != nil {
		t.Fatalf("oauth.NewServer: %v", err)
	}
	srv.SetOAuthServer(o)
	return srv, o
}

func bytes32ForTest() []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = byte(i + 1)
	}
	return out
}

// =====================================================================
// Discovery doc — replaces TASK-950's 501 stub
// =====================================================================

func TestOAuth_AuthorizationServerMetadata_PopulatedShape(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)

	rr := doRequest(srv, "GET", "/.well-known/oauth-authorization-server", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (replaced from 501 stub), got %d (body: %s)", rr.Code, rr.Body.String())
	}

	var doc map[string]any
	parseJSON(t, rr, &doc)

	wantStr := map[string]string{
		"issuer":                 testAuthServerURL,
		"authorization_endpoint": testAuthServerURL + "/oauth/authorize",
		"token_endpoint":         testAuthServerURL + "/oauth/token",
		"registration_endpoint":  testAuthServerURL + "/oauth/register",
	}
	for k, want := range wantStr {
		if got, _ := doc[k].(string); got != want {
			t.Errorf("%s: got %q, want %q", k, got, want)
		}
	}

	// Critical compliance bits Claude Desktop reads on connect.
	if !sliceContainsString(doc["code_challenge_methods_supported"], "S256") {
		t.Error("code_challenge_methods_supported missing S256")
	}
	if sliceContainsString(doc["code_challenge_methods_supported"], "plain") {
		t.Error("code_challenge_methods_supported must NOT include plain (we enforce S256-only)")
	}
	if !sliceContainsString(doc["response_types_supported"], "code") {
		t.Error("response_types_supported missing 'code'")
	}
	if !sliceContainsString(doc["grant_types_supported"], "authorization_code") {
		t.Error("grant_types_supported missing authorization_code")
	}
	if !sliceContainsString(doc["grant_types_supported"], "refresh_token") {
		t.Error("grant_types_supported missing refresh_token")
	}
	if !sliceContainsString(doc["token_endpoint_auth_methods_supported"], "none") {
		t.Error("token_endpoint_auth_methods_supported missing 'none' (public clients only)")
	}
	if doc["resource_indicators_supported"] != true {
		t.Error("resource_indicators_supported must be true (RFC 8707)")
	}
}

func sliceContainsString(v any, want string) bool {
	if arr, ok := v.([]any); ok {
		for _, x := range arr {
			if s, _ := x.(string); s == want {
				return true
			}
		}
	}
	return false
}

// =====================================================================
// /oauth/register — DCR (RFC 7591)
// =====================================================================

func TestOAuth_Register_HappyPath(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)

	rr := doRequest(srv, "POST", "/oauth/register", map[string]any{
		"client_name":   "Test Client",
		"redirect_uris": []string{"https://app.test/cb", "claude://oauth/callback"},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	parseJSON(t, rr, &resp)
	if resp["client_id"] == "" {
		t.Error("missing client_id")
	}
	if resp["client_id_issued_at"] == nil {
		t.Error("missing client_id_issued_at")
	}
	// Defaults applied per the handler.
	if !sliceContainsString(resp["grant_types"], "authorization_code") ||
		!sliceContainsString(resp["grant_types"], "refresh_token") {
		t.Errorf("grant_types defaults wrong: %v", resp["grant_types"])
	}
	if !sliceContainsString(resp["response_types"], "code") {
		t.Errorf("response_types default wrong: %v", resp["response_types"])
	}
	if resp["token_endpoint_auth_method"] != "none" {
		t.Errorf("token_endpoint_auth_method default wrong: %v", resp["token_endpoint_auth_method"])
	}
}

func TestOAuth_Register_RejectsMissingRedirectURIs(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	rr := doRequest(srv, "POST", "/oauth/register", map[string]any{
		"client_name": "No URIs",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
	var resp map[string]any
	parseJSON(t, rr, &resp)
	if resp["error"] != "invalid_redirect_uri" {
		t.Errorf("error code: got %v want invalid_redirect_uri", resp["error"])
	}
}

func TestOAuth_Register_RejectsBadRedirectURIShapes(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	cases := map[string]string{
		"relative":          "/oauth/cb",
		"http+non-loopback": "http://attacker.example/cb",
		"fragment":          "https://app.test/cb#x",
		"javascript scheme": "javascript:alert(1)",
	}
	for name, badURI := range cases {
		t.Run(name, func(t *testing.T) {
			rr := doRequest(srv, "POST", "/oauth/register", map[string]any{
				"redirect_uris": []string{badURI},
			})
			if rr.Code != http.StatusBadRequest {
				t.Errorf("%s: expected 400, got %d (body: %s)", name, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestOAuth_Register_RejectsNonPublicClientAuth(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	rr := doRequest(srv, "POST", "/oauth/register", map[string]any{
		"redirect_uris":              []string{"https://app.test/cb"},
		"token_endpoint_auth_method": "client_secret_basic",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-public-client auth method, got %d", rr.Code)
	}
}

func TestOAuth_Register_RejectsUnknownGrantType(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	rr := doRequest(srv, "POST", "/oauth/register", map[string]any{
		"redirect_uris": []string{"https://app.test/cb"},
		"grant_types":   []string{"client_credentials"},
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unsupported grant_type, got %d", rr.Code)
	}
}

func TestOAuth_Register_NotMountedOutsideCloudMode(t *testing.T) {
	srv := testServer(t) // no SetCloudMode, no SetOAuthServer
	rr := doRequest(srv, "POST", "/oauth/register", map[string]any{
		"redirect_uris": []string{"https://app.test/cb"},
	})
	// The route group is cloud-mode-gated; falls through to the SPA
	// catch-all (200 with HTML index in self-host mode), or 404 in
	// the test harness without a SPA.
	if rr.Code == http.StatusCreated {
		t.Errorf("DCR endpoint must NOT be reachable outside cloud mode; got 201")
	}
}

// =====================================================================
// /oauth/authorize — login redirect + consent stub
// =====================================================================

func TestOAuth_Authorize_RedirectsToLoginWhenNoSession(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	// Register a client to make the request well-formed.
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	verifier := "code-verifier-abc123-must-be-43-to-128-chars-long"
	challenge := s256Challenge(verifier)

	q := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"scope":                 {"pad:read"},
		"resource":              {testCanonicalAudience},
		"audience":              {testCanonicalAudience}, // fosite reads `audience` form param
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"client-csrf-state"},
	}

	rr := doRequest(srv, "GET", "/oauth/authorize?"+q.Encode(), nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302 to /login, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login?redirect=") {
		t.Errorf("Location should redirect to /login?redirect=...; got %q", loc)
	}
	if !strings.Contains(loc, url.QueryEscape("/oauth/authorize?")) {
		t.Errorf("redirect= must encode /oauth/authorize?...; got %q", loc)
	}
}

func TestOAuth_Authorize_RendersConsentStubWhenLoggedIn(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	user, sessionToken := loginTestUser(t, srv)

	verifier := "code-verifier-abc123-must-be-43-to-128-chars-long"
	challenge := s256Challenge(verifier)

	q := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"scope":                 {"pad:read pad:write"},
		"resource":              {testCanonicalAudience},
		"audience":              {testCanonicalAudience},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"client-state"},
	}

	rr := doAuthedRequest(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with consent HTML, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "<form method=\"POST\" action=\"/oauth/authorize/decide\"") {
		t.Error("consent stub must POST to /oauth/authorize/decide")
	}
	if !strings.Contains(body, `name="csrf_token"`) {
		t.Error("consent stub must include csrf_token hidden field")
	}
	if !strings.Contains(body, "pad:read") || !strings.Contains(body, "pad:write") {
		t.Error("consent stub must list requested scopes")
	}
	if !strings.Contains(body, user.Name) {
		t.Errorf("consent stub must surface the username %q for clarity", user.Name)
	}
}

func TestOAuth_Authorize_RejectsAudienceMismatch(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	_, sessionToken := loginTestUser(t, srv)

	verifier := "code-verifier-abc123-must-be-43-to-128-chars-long"
	challenge := s256Challenge(verifier)

	q := url.Values{
		"client_id":     {clientID},
		"response_type": {"code"},
		"redirect_uri":  {"https://app.test/cb"},
		"scope":         {"pad:read"},
		// Wrong audience — neither matches canonical. fosite's
		// AudienceMatchingStrategy from sub-PR B rejects.
		"audience":              {"https://other.example/mcp"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}

	rr := doAuthedRequest(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sessionToken)
	// fosite's WriteAuthorizeError redirects to the client's
	// redirect_uri with error params (so the client can surface).
	// fosite uses 303 See Other per RFC 7231 for non-idempotent
	// result redirects (authorize_error.go:68).
	if rr.Code != http.StatusSeeOther && rr.Code != http.StatusFound {
		t.Fatalf("expected 303/302 with OAuth error, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://app.test/cb") {
		t.Errorf("error redirect should target client redirect_uri; got %q", loc)
	}
	if !strings.Contains(loc, "error=invalid_request") {
		t.Errorf("expected error=invalid_request in error redirect; got %q", loc)
	}
}

// =====================================================================
// /oauth/authorize/decide — consent processing
// =====================================================================

func TestOAuth_AuthorizeDecide_RejectsMissingCSRFToken(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"scope":                 {"pad:read"},
		"code_challenge":        {s256Challenge("abc-12345-the-quick-brown-fox-1234567890")},
		"code_challenge_method": {"S256"},
		"audience":              {testCanonicalAudience},
		"state":                 {"test-state-12345"}, // fosite requires ≥8 chars
		"decision":              {"approve"},
		// Deliberately no csrf_token.
	}

	req := httptest.NewRequest("POST", "/oauth/authorize/decide", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: sessionToken})
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403 for missing csrf_token, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestOAuth_AuthorizeDecide_DenyRedirectsAccessDenied(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)

	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"scope":                 {"pad:read"},
		"code_challenge":        {s256Challenge("abc-12345-the-quick-brown-fox-1234567890")},
		"code_challenge_method": {"S256"},
		"audience":              {testCanonicalAudience},
		"state":                 {"test-state-12345"},
		"decision":              {"deny"},
		"csrf_token":            {csrfTok},
	}
	rr := postFormWithCookie(srv, "/oauth/authorize/decide", form, sessionToken, csrfTok)
	// fosite uses 303 See Other for OAuth redirects.
	if rr.Code != http.StatusSeeOther && rr.Code != http.StatusFound {
		t.Fatalf("expected 303/302 with access_denied, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "error=access_denied") {
		t.Errorf("Location should carry error=access_denied; got %q", loc)
	}
}

// =====================================================================
// Full /authorize → /token round-trip with PKCE
// =====================================================================

func TestOAuth_FullAuthCodeFlow_PKCE(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)

	// Step 1: POST /oauth/authorize/decide with decision=approve.
	verifier := "verifier-1234567890-abcdef-the-quick-brown-fox-jumps"
	challenge := s256Challenge(verifier)

	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"pad:read"},
		"audience":              {testCanonicalAudience},
		"state":                 {"client-csrf-state"},
		"decision":              {"approve"},
		"csrf_token":            {csrfTok},
	}
	rr := postFormWithCookie(srv, "/oauth/authorize/decide", form, sessionToken, csrfTok)
	// fosite uses 303 See Other for OAuth redirects.
	if rr.Code != http.StatusSeeOther && rr.Code != http.StatusFound {
		t.Fatalf("decide expected 303/302, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	cbURL, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse callback Location: %v", err)
	}
	code := cbURL.Query().Get("code")
	if code == "" {
		t.Fatalf("callback URL missing code param: %s", loc)
	}
	if cbURL.Query().Get("state") != "client-csrf-state" {
		t.Errorf("state not echoed: got %q", cbURL.Query().Get("state"))
	}

	// Step 2: POST /oauth/token with PKCE verifier + audience.
	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {"https://app.test/cb"},
		"code_verifier": {verifier},
		"audience":      {testCanonicalAudience},
	}
	tr := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(tokenForm.Encode()))
	tr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tr.RemoteAddr = "192.0.2.1:1234"
	trr := httptest.NewRecorder()
	srv.ServeHTTP(trr, tr)
	if trr.Code != http.StatusOK {
		t.Fatalf("token expected 200, got %d (body: %s)", trr.Code, trr.Body.String())
	}

	var resp map[string]any
	parseJSON(t, trr, &resp)
	if resp["access_token"] == "" || resp["access_token"] == nil {
		t.Errorf("missing access_token: %v", resp)
	}
	if resp["refresh_token"] == "" || resp["refresh_token"] == nil {
		t.Errorf("missing refresh_token (RefreshTokenScopes=[] should issue on every grant): %v", resp["refresh_token"])
	}
	if resp["token_type"] != "bearer" {
		t.Errorf("token_type should be bearer, got %v", resp["token_type"])
	}
}

// TestOAuth_Authorize_AcceptsResourceOnly pins Codex review #372
// round 1 fix: real RFC 8707 clients (Claude Desktop / Cursor /
// ChatGPT) send `resource=` not `audience=`. fosite v0.49 only
// reads the `audience` form key, so without translation the
// audienceMatchingStrategy sees an empty needle and rejects
// every authorize request from a real-world client.
//
// This test sends ONLY `resource=` (no `audience=`) and asserts
// the request reaches the consent stub successfully — i.e.
// translateResourceToAudience populated fosite's expected key.
func TestOAuth_Authorize_AcceptsResourceOnly(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	_, sessionToken := loginTestUser(t, srv)

	verifier := "verifier-the-quick-brown-fox-1234567890-abcdef"
	challenge := s256Challenge(verifier)

	// Note: ONLY resource= here — no audience=. RFC 8707 form.
	q := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"scope":                 {"pad:read"},
		"resource":              {testCanonicalAudience},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"resource-only-state"},
	}
	rr := doAuthedRequest(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (consent stub) for resource-only request; got %d (Location: %s)",
			rr.Code, rr.Header().Get("Location"))
	}
}

// TestOAuth_AuthorizationServerMetadata_OmitsUnimplementedEndpoints
// pins Codex review #372 round 1 fix #2 + round 2 fix #2: the
// discovery doc must NOT advertise revocation_endpoint /
// introspection_endpoint (sub-PR D wires those) or
// authorization_response_iss_parameter_supported (RFC 9207 — fosite
// v0.49 doesn't add iss to the redirect, so claiming support would
// mislead RFC 9207-aware clients).
//
// Advertised-but-broken metadata is worse than absent metadata —
// RFC 8414 §2 marks all four fields OPTIONAL.
func TestOAuth_AuthorizationServerMetadata_OmitsUnimplementedEndpoints(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)

	rr := doRequest(srv, "GET", "/.well-known/oauth-authorization-server", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var doc map[string]any
	parseJSON(t, rr, &doc)

	for _, k := range []string{
		"revocation_endpoint",
		"introspection_endpoint",
		"revocation_endpoint_auth_methods_supported",
		"introspection_endpoint_auth_methods_supported",
		"authorization_response_iss_parameter_supported",
	} {
		if _, ok := doc[k]; ok {
			t.Errorf("doc must NOT advertise %q (handler/feature not implemented); got %v", k, doc[k])
		}
	}
}

// TestOAuth_AuthorizationServerMetadata_503WhenOAuthDisabled pins
// Codex review #372 round 3: the discovery doc lives in the MCP
// route group while the /oauth/* handlers live in their own group.
// A cloud deployment with PAD_MCP_PUBLIC_URL unset gets the MCP
// routes mounted (so the discovery doc is reachable) but NOT the
// OAuth handlers (cmd/pad/main.go skips oauth.NewServer wiring
// because there's no canonical audience). Without the gate, the
// doc would 200 with /oauth/{register,authorize,token} URLs that
// 404 — worse for clients than no document at all.
//
// Fail-loud 503 lets ops detect the misconfiguration immediately.
func TestOAuth_AuthorizationServerMetadata_503WhenOAuthDisabled(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode("test-secret")
	// Mount MCP transport (so the discovery route is registered)
	// but DO NOT call SetOAuthServer — simulating the
	// PAD_MCP_PUBLIC_URL-unset path in cmd/pad/main.go.
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	srv.SetMCPTransport(stub, "https://mcp.test.example", "https://app.test.example")

	rr := doRequest(srv, "GET", "/.well-known/oauth-authorization-server", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 fail-loud when oauthServer is nil, got %d (body: %s)",
			rr.Code, rr.Body.String())
	}
}

// TestOAuth_Register_RateLimited pins Codex review #372 round 2:
// /oauth/register is open by RFC 7591 design but must be rate-
// limited so an attacker can't flood the oauth_clients table.
// Reuses the Register limiter (5/hour/IP, burst 5), so the 6th
// request from the same IP within an hour returns 429.
//
// All requests share the test harness's "192.0.2.1" RemoteAddr,
// so the limiter buckets per-IP work as expected. testServer's
// fresh Server has fresh limiters, so other tests don't leak
// budget across.
func TestOAuth_Register_RateLimited(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)

	body := map[string]any{
		"client_name":   "Spammer",
		"redirect_uris": []string{"https://app.test/cb"},
	}

	// First 5 within the burst window must succeed.
	for i := 0; i < 5; i++ {
		rr := doRequest(srv, "POST", "/oauth/register", body)
		if rr.Code != http.StatusCreated {
			t.Fatalf("request %d: expected 201, got %d (body: %s)", i+1, rr.Code, rr.Body.String())
		}
	}
	// 6th must trip the limiter.
	rr := doRequest(srv, "POST", "/oauth/register", body)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("6th request: expected 429 (rate-limited), got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestOAuth_Token_RejectsMissingPKCEVerifier(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)

	verifier := "verifier-1234567890-abcdef-the-quick-brown-fox-jumps"
	challenge := s256Challenge(verifier)

	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"scope":                 {"pad:read"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"audience":              {testCanonicalAudience},
		"state":                 {"test-state-12345"},
		"decision":              {"approve"},
		"csrf_token":            {csrfTok},
	}
	rr := postFormWithCookie(srv, "/oauth/authorize/decide", form, sessionToken, csrfTok)
	loc := rr.Header().Get("Location")
	cbURL, _ := url.Parse(loc)
	code := cbURL.Query().Get("code")

	// Token exchange WITHOUT code_verifier — must fail.
	tokenForm := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"client_id":    {clientID},
		"redirect_uri": {"https://app.test/cb"},
		"audience":     {testCanonicalAudience},
		// Deliberately no code_verifier.
	}
	tr := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(tokenForm.Encode()))
	tr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tr.RemoteAddr = "192.0.2.1:1234"
	trr := httptest.NewRecorder()
	srv.ServeHTTP(trr, tr)
	if trr.Code == http.StatusOK {
		t.Errorf("expected non-200 for missing PKCE verifier; got %d (body: %s)", trr.Code, trr.Body.String())
	}
}

// =====================================================================
// Helpers
// =====================================================================

// registerTestClient calls /oauth/register and returns the client_id.
func registerTestClient(t *testing.T, srv *Server, redirectURI string) string {
	t.Helper()
	rr := doRequest(srv, "POST", "/oauth/register", map[string]any{
		"client_name":   "Test",
		"redirect_uris": []string{redirectURI},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("registerTestClient: %d (body: %s)", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	parseJSON(t, rr, &resp)
	id, _ := resp["client_id"].(string)
	if id == "" {
		t.Fatalf("registerTestClient: empty client_id")
	}
	return id
}

// testSessionUA is the User-Agent the test session is bound to.
// SessionAuth (middleware_auth.go:186) hashes the request's UA
// against the session's stored hash; mismatch silently drops the
// session (returns w/o currentUser populated). Tests using
// doAuthedRequest must send this exact value as User-Agent.
const testSessionUA = "oauth-test-ua/1.0"

// loginTestUser creates a user + session, returns the user + the
// raw session token suitable for AddCookie.
func loginTestUser(t *testing.T, srv *Server) (*models.User, string) {
	t.Helper()
	user, err := srv.store.CreateUser(models.UserCreate{
		Email:    "oauth-test@example.com",
		Name:     "OAuth Tester",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	tok, err := srv.store.CreateSession(user.ID, "test", "192.0.2.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return user, tok
}

// doAuthedRequest is like doRequest but with a session cookie + the
// matching User-Agent attached (SessionAuth UA-binding, see
// middleware_auth.go:186).
func doAuthedRequest(srv *Server, method, path string, body any, sessionToken string) *httptest.ResponseRecorder {
	var bodyReader *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = strings.NewReader(string(b))
	}
	var req *http.Request
	if bodyReader != nil {
		req = httptest.NewRequest(method, path, bodyReader)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: sessionToken})
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// postFormWithCookie POSTs an x-www-form-urlencoded body with both
// a session cookie and the matching CSRF cookie set, plus the
// session-bound User-Agent.
func postFormWithCookie(srv *Server, path string, form url.Values, sessionToken, csrfTok string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: sessionToken})
	req.AddCookie(&http.Cookie{Name: csrfCookieName(srv.secureCookies), Value: csrfTok})
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// readCSRFFromCookie hits any consent-rendering endpoint that sets
// the CSRF cookie on first visit, returns the token. We use
// /oauth/authorize because it's the natural source — the consent
// stub renders + sets the cookie.
//
// Returns the raw cookie value (hex string).
func readCSRFFromCookie(t *testing.T, srv *Server, sessionToken string) string {
	t.Helper()
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	verifier := "verifier-the-quick-brown-fox-1234567890-abcdef-1234"
	challenge := s256Challenge(verifier)
	q := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"scope":                 {"pad:read"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"audience":              {testCanonicalAudience},
		// fosite requires state to be ≥8 chars (entropy guard);
		// authorize_request_handler.go validates this on every flow.
		"state": {"helper-state-12345"},
	}
	rr := doAuthedRequest(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("readCSRFFromCookie: authorize render = %d (Location: %s, body: %s)", rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == csrfCookieName(srv.secureCookies) {
			return c.Value
		}
	}
	t.Fatal("readCSRFFromCookie: no csrf cookie in response")
	return ""
}

// s256Challenge computes the base64url(no-padding, SHA256(verifier))
// per RFC 7636 §4.2. PKCE clients use this to derive code_challenge
// from code_verifier.
func s256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
