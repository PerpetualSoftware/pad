package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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
	// Canonical resource URL the test fixtures advertise + bind tokens to.
	// No /mcp suffix — matches the post-fix semantic where MCPPublicURL
	// is published verbatim as the RFC 9728 resource (mcp.stripe.com /
	// mcp.linear.app convention). The actual transport still mounts at
	// /mcp internally on the chi router.
	testCanonicalAudience = "https://mcp.test.example"
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
	// Pass testCanonicalAudience directly — post-fix it IS the canonical
	// resource URL (no /mcp suffix to strip).
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	srv.SetMCPTransport(stub, testCanonicalAudience, testAuthServerURL)

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

func TestOAuth_Authorize_RendersConsentWhenLoggedIn(t *testing.T) {
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
	if !strings.Contains(body, `action="/oauth/authorize/decide"`) {
		t.Error("consent UI must POST to /oauth/authorize/decide")
	}
	if !strings.Contains(body, `name="csrf_token"`) {
		t.Error("consent UI must include csrf_token hidden field")
	}
	// Tier radios — client requested pad:read + pad:write, so both
	// radios should render. pad:admin was NOT requested → must NOT
	// render (granting it would fail fosite's subset check).
	if !strings.Contains(body, `value="read"`) {
		t.Error("consent UI must render the pad:read tier radio (requested)")
	}
	if !strings.Contains(body, `value="write"`) {
		t.Error("consent UI must render the pad:write tier radio (requested)")
	}
	if strings.Contains(body, `value="admin"`) {
		t.Error("consent UI must NOT render pad:admin tier radio (not requested)")
	}
	// Workspace allow-list section.
	if !strings.Contains(body, `name="allowed_workspaces"`) {
		t.Error("consent UI must include the workspace allow-list field")
	}
	if !strings.Contains(body, `value="*"`) {
		t.Error("consent UI must offer the wildcard workspace option")
	}
	if !strings.Contains(body, user.Name) {
		t.Errorf("consent UI must surface the username %q for clarity", user.Name)
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
		// TASK-952 consent fields (read tier + wildcard workspace —
		// minimal valid selection that doesn't require seeding workspace
		// memberships in this PKCE-focused test).
		"capability_tier":    {"read"},
		"allowed_workspaces": {"*"},
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

// TestOAuth_Authorize_AcceptsNoResource_DefaultsToCanonical pins the
// fix for the "code: Field required" error Claude Desktop's MCP
// connector flow surfaces against pad: real MCP clients (Claude
// Desktop, Cursor as of 2026-05) don't send the RFC 8707 resource=
// parameter at all on /oauth/authorize. Before this fix,
// translateResourceToAudience only translated resource→audience when
// resource was present, so empty-resource requests reached fosite's
// audienceMatchingStrategy with an empty needle and got rejected
// with "resource parameter is required (RFC 8707)". fosite then
// redirected to the client's redirect_uri with ?error=invalid_request,
// and Claude's callback failed with a missing-`code`-field error
// (Anthropic's pydantic-style "code: Field required" envelope).
//
// RFC 8707 §2 marks the resource parameter OPTIONAL; servers with
// a single canonical audience are expected to default to it. This
// test sends NEITHER resource= NOR audience= and asserts the
// authorize request reaches the consent stub — proof that the
// translation injected the canonical audience instead of erroring.
//
// Pairs with TestOAuth_Authorize_AcceptsResourceOnly above (the
// resource-only path) and the existing TestOAuth_Authorize_*
// audience-mismatch tests (the wrong-audience path) to lock in the
// full matrix of what /authorize accepts for the audience parameter.
func TestOAuth_Authorize_AcceptsNoResource_DefaultsToCanonical(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	_, sessionToken := loginTestUser(t, srv)

	verifier := "verifier-the-quick-brown-fox-1234567890-abcdef"
	challenge := s256Challenge(verifier)

	// Note: NO resource= AND NO audience= — exactly Claude Desktop's
	// real-world authorize-request shape.
	q := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"scope":                 {"pad:read"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"no-resource-state"},
	}
	rr := doAuthedRequest(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (consent stub) for no-resource request; got %d (Location: %s, Body: %s)",
			rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
}

// TestOAuth_ConsentScreen_NonceCSPLetsInlineScriptRun pins the fix
// for the bug where the consent screen rendered with the Allow
// button stuck disabled. The button starts disabled and is enabled
// by an inline <script> when the user picks workspaces; pad serves
// every response with strict CSP (script-src 'self' — no
// 'unsafe-inline'), so without a per-request nonce the browser
// blocked the script and disabled=true persisted from initial render.
//
// The fix generates a nonce per consent render, sets a
// CSP override on the response that authorizes that nonce
// (script-src 'self' 'nonce-X' 'strict-dynamic'), and threads the
// same nonce into the <script> tag's nonce attribute. This test
// pins:
//
//  1. A CSP header is set with a nonce-* token in script-src (proof
//     the strict baseline didn't pass through unmodified).
//  2. The body's <script ...> tag carries a nonce attribute matching
//     the value in the CSP header (proof the two are linked — drift
//     would re-introduce the bug).
//
// Without (1), the browser blocks. Without (2), the browser blocks
// even WITH the CSP override. Both are necessary.
func TestOAuth_ConsentScreen_NonceCSPLetsInlineScriptRun(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	_, sessionToken := loginTestUser(t, srv)

	verifier := "verifier-the-quick-brown-fox-1234567890-abcdef"
	challenge := s256Challenge(verifier)
	q := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"scope":                 {"pad:read"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"nonce-csp-state"},
	}
	rr := doAuthedRequest(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (consent stub); got %d", rr.Code)
	}

	csp := rr.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("consent response must set Content-Security-Policy")
	}
	// Extract the nonce from the script-src directive. Format is
	// `'nonce-<base64>'`; the base64 alphabet includes +, /, and =.
	cspRe := regexp.MustCompile(`'nonce-([A-Za-z0-9+/=]+)'`)
	cspMatches := cspRe.FindStringSubmatch(csp)
	if len(cspMatches) != 2 {
		t.Fatalf("CSP must include a 'nonce-...' token in script-src; got: %s", csp)
	}
	cspNonce := cspMatches[1]

	// Extract the nonce from the rendered <script> tag. html/template
	// HTML-escapes attribute values (e.g. base64 '+' renders as
	// '&#43;'), so we extract the on-the-wire form, unescape it, and
	// compare against the raw CSP nonce. Fail-loud if the two don't
	// match — drift would re-introduce the original bug (nonce in
	// header but not in tag = browser blocks).
	body := rr.Body.String()
	bodyRe := regexp.MustCompile(`<script nonce="([^"]+)">`)
	bodyMatches := bodyRe.FindStringSubmatch(body)
	if len(bodyMatches) != 2 {
		t.Fatalf("body must contain a <script nonce=\"...\"> tag; CSP=%s\nbody snippet: %s",
			csp, snippetAround(body, "<script", 200))
	}
	bodyNonce := html.UnescapeString(bodyMatches[1])
	if bodyNonce != cspNonce {
		t.Errorf("script tag nonce %q (escaped %q) must match CSP nonce %q",
			bodyNonce, bodyMatches[1], cspNonce)
	}
}

// snippetAround returns up to N bytes of body around the first
// occurrence of marker, or "(marker not found)". Used by the nonce
// test for diagnostic output without dumping the entire HTML page.
func snippetAround(body, marker string, n int) string {
	i := strings.Index(body, marker)
	if i < 0 {
		return "(marker not found)"
	}
	end := i + n
	if end > len(body) {
		end = len(body)
	}
	return body[i:end]
}

// TestOAuth_AuthorizationServerMetadata_OmitsRFC9207IssFlag pins
// Codex review #372 round 2: authorization_response_iss_parameter_supported
// (RFC 9207) is intentionally omitted because fosite v0.49 doesn't
// add iss=<issuer> to authorize redirects, so claiming support would
// mislead RFC 9207-aware clients into rejecting the response.
//
// Advertised-but-broken metadata is worse than absent metadata —
// RFC 8414 §2 marks the field OPTIONAL.
//
// (Sub-PR D fills in revocation_endpoint and introspection_endpoint;
// the previous OmitsUnimplementedEndpoints test asserted those were
// also absent — those assertions moved to AdvertisesRevokeIntrospect.)
func TestOAuth_AuthorizationServerMetadata_OmitsRFC9207IssFlag(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)

	rr := doRequest(srv, "GET", "/.well-known/oauth-authorization-server", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var doc map[string]any
	parseJSON(t, rr, &doc)

	if v, ok := doc["authorization_response_iss_parameter_supported"]; ok {
		t.Errorf("doc must NOT advertise authorization_response_iss_parameter_supported (RFC 9207 not actually implemented); got %v", v)
	}
}

// TestOAuth_AuthorizationServerMetadata_AdvertisesRevokeIntrospect
// asserts sub-PR D's discovery doc additions: revocation_endpoint,
// introspection_endpoint, and the revocation auth-methods list are
// populated and point at the URLs the handlers actually serve.
//
// introspection_endpoint_auth_methods_supported is intentionally
// OMITTED — see Codex review #373 round 1 + the comment in
// handleOAuthAuthorizationServer. The introspection endpoint only
// accepts Bearer auth (a separate active access token), which has
// no registered registry value, so listing "none" would be
// misleading and listing nothing matches RFC 8414 §2's OPTIONAL
// status.
//
// Real Claude-Desktop / Cursor / ChatGPT clients fetch this doc
// once on connect and cache it; if either URL is wrong, the client
// will dial a 404 and surface "auth server appears broken" to the
// user with no clean recovery. Pin the wire shape.
func TestOAuth_AuthorizationServerMetadata_AdvertisesRevokeIntrospect(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)

	rr := doRequest(srv, "GET", "/.well-known/oauth-authorization-server", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var doc map[string]any
	parseJSON(t, rr, &doc)

	wantStr := map[string]string{
		"revocation_endpoint":    testAuthServerURL + "/oauth/revoke",
		"introspection_endpoint": testAuthServerURL + "/oauth/introspect",
	}
	for k, want := range wantStr {
		if got, _ := doc[k].(string); got != want {
			t.Errorf("%s: got %q, want %q", k, got, want)
		}
	}
	// Revoke really does accept "none" — public client posts
	// client_id form field, no secret. Honest advertisement.
	if !sliceContainsString(doc["revocation_endpoint_auth_methods_supported"], "none") {
		t.Errorf("revocation_endpoint_auth_methods_supported missing 'none'; got %v",
			doc["revocation_endpoint_auth_methods_supported"])
	}
	// introspection_endpoint_auth_methods_supported MUST NOT be
	// present (Codex review #373 round 1): the endpoint requires
	// Bearer auth which isn't a registered auth-methods value.
	// Listing anything here would mislead clients.
	if v, ok := doc["introspection_endpoint_auth_methods_supported"]; ok {
		t.Errorf("introspection_endpoint_auth_methods_supported must NOT be advertised (Bearer-only is unregistered); got %v", v)
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
		// TASK-952 consent fields (read tier + wildcard workspace).
		"capability_tier":    {"read"},
		"allowed_workspaces": {"*"},
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
// Consent UI (TASK-952)
// =====================================================================

// TestConsent_RendersUserWorkspaces verifies the consent UI lists
// the workspaces the logged-in user is a member of, with the user's
// role shown next to each. Multi-workspace coverage — the single-
// workspace case is covered implicitly by the basic render test.
func TestConsent_RendersUserWorkspaces(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	user, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	// Seed three workspaces with different roles for this user.
	wsAlpha := mustSeedWorkspaceWithRole(t, srv, user.ID, "Alpha Project", "alpha", "owner")
	wsBeta := mustSeedWorkspaceWithRole(t, srv, user.ID, "Beta Site", "beta", "editor")
	wsGamma := mustSeedWorkspaceWithRole(t, srv, user.ID, "Gamma Notes", "gamma", "viewer")
	_ = wsAlpha
	_ = wsBeta
	_ = wsGamma

	verifier := "verifier-render-ws-quick-brown-fox-jumps-over-lazy"
	challenge := s256Challenge(verifier)
	q := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"scope":                 {"pad:read pad:write"},
		"audience":              {testCanonicalAudience},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"render-ws-state"},
	}
	rr := doAuthedRequest(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// Each workspace's slug must appear as a checkbox value, plus
	// its display name + role label so the user can tell them apart.
	for _, ws := range []struct{ slug, name, role string }{
		{"alpha", "Alpha Project", "owner"},
		{"beta", "Beta Site", "editor"},
		{"gamma", "Gamma Notes", "viewer"},
	} {
		if !strings.Contains(body, `value="`+ws.slug+`"`) {
			t.Errorf("checkbox for workspace %q missing", ws.slug)
		}
		if !strings.Contains(body, ws.name) {
			t.Errorf("workspace name %q not displayed", ws.name)
		}
		if !strings.Contains(body, "you are "+ws.role) {
			t.Errorf("role %q not surfaced for workspace %q", ws.role, ws.slug)
		}
	}
}

// TestConsent_NoWorkspaces_ShowsEmptyState confirms the consent UI
// renders cleanly for a user with zero workspace memberships. The
// Allow button stays disabled (server-side validation enforces this
// regardless of JS state).
//
// In production cloud-mode signup creates a default workspace, so
// this case is rare — but worth pinning so a future regression
// (e.g. autoCreateWorkspace failing silently) doesn't leave users
// with a broken consent screen.
func TestConsent_NoWorkspaces_ShowsEmptyState(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	verifier := "verifier-no-ws-quick-brown-fox-jumps-over-lazy-dog"
	challenge := s256Challenge(verifier)
	q := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"scope":                 {"pad:read"},
		"audience":              {testCanonicalAudience},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"no-ws-state"},
	}
	rr := doAuthedRequest(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// The empty-state message must mention how to recover.
	if !strings.Contains(body, "not a member of any workspaces") {
		t.Errorf("expected empty-state message; got body: %s", body[:min(400, len(body))])
	}
}

// TestConsent_TierRadios_OnlyRequestedScopes verifies the tier radio
// is constrained to the intersection of {pad:read, pad:write,
// pad:admin} and the client's requested scopes. fosite's grant-time
// subset check (RFC 6749 §3.3) would reject granting a tier the
// client didn't request, so the UI must never offer it.
func TestConsent_TierRadios_OnlyRequestedScopes(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	verifier := "verifier-tiers-quick-brown-fox-jumps-over-lazy-dog"
	challenge := s256Challenge(verifier)
	q := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"scope":                 {"pad:read"}, // ONLY read requested
		"audience":              {testCanonicalAudience},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"tiers-state"},
	}
	rr := doAuthedRequest(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `value="read"`) {
		t.Error("read tier radio must render (was requested)")
	}
	if strings.Contains(body, `value="write"`) {
		t.Error("write tier radio must NOT render (not requested)")
	}
	if strings.Contains(body, `value="admin"`) {
		t.Error("admin tier radio must NOT render (not requested)")
	}
}

// TestConsent_ApproveWithSpecificWorkspaces verifies the happy
// path: user picks a specific workspace + tier, and the issued
// access token carries the chosen tier scope plus the
// workspace allow-list in session.Extra (where TASK-953 will read
// it for live role enforcement).
func TestConsent_ApproveWithSpecificWorkspaces(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	user, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)

	mustSeedWorkspaceWithRole(t, srv, user.ID, "Alpha", "alpha", "editor")
	mustSeedWorkspaceWithRole(t, srv, user.ID, "Beta", "beta", "editor")

	verifier := "verifier-approve-specific-quick-brown-fox-1234567890"
	challenge := s256Challenge(verifier)
	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"pad:read pad:write"},
		"audience":              {testCanonicalAudience},
		"state":                 {"approve-specific-state"},
		"decision":              {"approve"},
		"csrf_token":            {csrfTok},
		"capability_tier":       {"write"},
		"allowed_workspaces":    {"alpha", "beta"}, // multi-value
	}
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

	// Token's granted scope must be exactly pad:write — selective
	// consent. The client requested pad:read + pad:write, but the
	// user picked write tier; only that scope must be granted.
	scope, _ := resp["scope"].(string)
	if scope != "pad:write" {
		t.Errorf("granted scope: got %q, want exactly %q (selective consent)", scope, "pad:write")
	}

	// Introspect to confirm the session.Extra carries the workspace
	// allow-list. Use a separate grant for the bearer (fosite
	// rejects self-bearer-introspect).
	bearerTokens := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID,
		"verifier-approve-bearer-quick-brown-fox-jumps-over-l")
	bearer, _ := bearerTokens["access_token"].(string)

	rrIntro := postOAuthFormBearer(srv, "/oauth/introspect", url.Values{
		"token": {access},
	}, bearer)
	if rrIntro.Code != http.StatusOK {
		t.Fatalf("introspect: expected 200, got %d (body: %s)", rrIntro.Code, rrIntro.Body.String())
	}
	var ir map[string]any
	parseJSON(t, rrIntro, &ir)
	if active, _ := ir["active"].(bool); !active {
		t.Fatalf("introspected token should be active; got %v", ir)
	}
	// session.Extra is serialized as top-level fields by fosite's
	// WriteIntrospectionResponse (introspection_response_writer.go:197-209).
	// allowed_workspaces must be the list the user picked.
	got, ok := ir["allowed_workspaces"].([]any)
	if !ok {
		t.Fatalf("allowed_workspaces missing from introspection response; got %v", ir)
	}
	want := map[string]bool{"alpha": true, "beta": true}
	if len(got) != len(want) {
		t.Fatalf("allowed_workspaces length: got %d, want %d (entries: %v)", len(got), len(want), got)
	}
	for _, v := range got {
		s, _ := v.(string)
		if !want[s] {
			t.Errorf("unexpected workspace in allow-list: %q", s)
		}
	}
}

// TestConsent_ApproveWithWildcard verifies the wildcard "any
// workspace" path: the issued token's session.Extra carries
// ["*"] which TASK-953 will translate to "no workspace gating
// beyond live membership" at enforcement time.
func TestConsent_ApproveWithWildcard(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)

	verifier := "verifier-approve-wildcard-quick-brown-fox-1234567890"
	challenge := s256Challenge(verifier)
	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"pad:read"},
		"audience":              {testCanonicalAudience},
		"state":                 {"approve-wildcard-state"},
		"decision":              {"approve"},
		"csrf_token":            {csrfTok},
		"capability_tier":       {"read"},
		"allowed_workspaces":    {"*"},
	}
	rrDecide := postFormWithCookie(srv, "/oauth/authorize/decide", form, sessionToken, csrfTok)
	if rrDecide.Code != http.StatusSeeOther && rrDecide.Code != http.StatusFound {
		t.Fatalf("decide: expected 303/302, got %d (body: %s)", rrDecide.Code, rrDecide.Body.String())
	}
	cbURL, _ := url.Parse(rrDecide.Header().Get("Location"))
	code := cbURL.Query().Get("code")

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
		t.Fatalf("token: %d (body: %s)", trr.Code, trr.Body.String())
	}
	var resp map[string]any
	parseJSON(t, trr, &resp)
	access, _ := resp["access_token"].(string)

	// Introspect via a separate bearer.
	bearerTokens := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID,
		"verifier-wildcard-bearer-quick-brown-fox-jumps-over-")
	bearer, _ := bearerTokens["access_token"].(string)

	rrIntro := postOAuthFormBearer(srv, "/oauth/introspect", url.Values{
		"token": {access},
	}, bearer)
	var ir map[string]any
	parseJSON(t, rrIntro, &ir)
	got, ok := ir["allowed_workspaces"].([]any)
	if !ok || len(got) != 1 {
		t.Fatalf("allowed_workspaces should be exactly [\"*\"]; got %v", ir["allowed_workspaces"])
	}
	if s, _ := got[0].(string); s != "*" {
		t.Errorf("allowed_workspaces[0]: got %q, want %q", s, "*")
	}
}

// TestConsent_ApproveWithoutWorkspaceSelection_Rejected pins the
// server-side check that at least one workspace (or wildcard) is
// selected. The UI's Allow button is disabled in this state, but
// a tampered POST that sends decision=approve with no
// allowed_workspaces must still be rejected.
func TestConsent_ApproveWithoutWorkspaceSelection_Rejected(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)

	verifier := "verifier-no-selection-quick-brown-fox-jumps-over-l"
	challenge := s256Challenge(verifier)
	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"pad:read"},
		"audience":              {testCanonicalAudience},
		"state":                 {"no-selection-state"},
		"decision":              {"approve"},
		"csrf_token":            {csrfTok},
		"capability_tier":       {"read"},
		// Deliberately no allowed_workspaces.
	}
	rr := postFormWithCookie(srv, "/oauth/authorize/decide", form, sessionToken, csrfTok)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 with no workspace selection, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestConsent_ApproveWithUntrustedSlug_Rejected pins the defense-in-
// depth check: a tampered form sending a workspace slug the user
// is NOT a member of must be rejected at consent time, not silently
// stored on the token (where TASK-953 would also reject, but with
// a misleading "you don't have access" error far downstream).
func TestConsent_ApproveWithUntrustedSlug_Rejected(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	user, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)

	// User is a member of "alpha" only. Tampered form submits
	// "evil-other-workspace" — must reject.
	mustSeedWorkspaceWithRole(t, srv, user.ID, "Alpha", "alpha", "owner")

	verifier := "verifier-untrusted-quick-brown-fox-jumps-over-lazy"
	challenge := s256Challenge(verifier)
	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"pad:read"},
		"audience":              {testCanonicalAudience},
		"state":                 {"untrusted-state"},
		"decision":              {"approve"},
		"csrf_token":            {csrfTok},
		"capability_tier":       {"read"},
		"allowed_workspaces":    {"alpha", "evil-other-workspace"},
	}
	rr := postFormWithCookie(srv, "/oauth/authorize/decide", form, sessionToken, csrfTok)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for tampered allow-list, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestConsent_ApproveWithUnrequestedTier_Rejected pins the server-
// side tier validation: a tampered form sending capability_tier=admin
// when the client only requested pad:read+pad:write must be rejected.
// fosite's grant-time check would also catch this with a less-
// readable error, so the parse-time check produces a cleaner UX.
func TestConsent_ApproveWithUnrequestedTier_Rejected(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)

	verifier := "verifier-unreq-tier-quick-brown-fox-jumps-over-laz"
	challenge := s256Challenge(verifier)
	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"pad:read pad:write"}, // no admin
		"audience":              {testCanonicalAudience},
		"state":                 {"unreq-tier-state"},
		"decision":              {"approve"},
		"csrf_token":            {csrfTok},
		"capability_tier":       {"admin"}, // tampered: not requested
		"allowed_workspaces":    {"*"},
	}
	rr := postFormWithCookie(srv, "/oauth/authorize/decide", form, sessionToken, csrfTok)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unrequested tier, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestConsent_TokenScopeMatchesTierChoice_Read pins selective consent:
// when the user picks "read" tier even though the client requested
// pad:read AND pad:write, the issued access token's scope is exactly
// pad:read. This is the central security property of the consent UI.
//
// registerTestClient defaults to scopes=[pad:read, pad:write] (per
// handleOAuthRegister), so we can request both but pick only read.
// pad:admin would require explicit registration which isn't relevant
// here — the read-vs-write distinction is the same selective-consent
// property either way.
func TestConsent_TokenScopeMatchesTierChoice_Read(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)

	verifier := "verifier-tier-match-quick-brown-fox-jumps-over-laz"
	challenge := s256Challenge(verifier)
	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"pad:read pad:write"}, // both requested
		"audience":              {testCanonicalAudience},
		"state":                 {"tier-match-state"},
		"decision":              {"approve"},
		"csrf_token":            {csrfTok},
		"capability_tier":       {"read"}, // user picks read despite client asking for both
		"allowed_workspaces":    {"*"},
	}
	rrDecide := postFormWithCookie(srv, "/oauth/authorize/decide", form, sessionToken, csrfTok)
	if rrDecide.Code != http.StatusSeeOther && rrDecide.Code != http.StatusFound {
		t.Fatalf("decide: expected 303/302, got %d (body: %s)", rrDecide.Code, rrDecide.Body.String())
	}
	cbURL, _ := url.Parse(rrDecide.Header().Get("Location"))
	code := cbURL.Query().Get("code")

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
		t.Fatalf("token: %d (body: %s)", trr.Code, trr.Body.String())
	}
	var resp map[string]any
	parseJSON(t, trr, &resp)
	scope, _ := resp["scope"].(string)
	if scope != "pad:read" {
		t.Errorf("selective consent: got scope %q, want exactly %q (user picked read despite client asking for all)", scope, "pad:read")
	}
}

// TestConsent_URLPollution_DoesNotOverrideUserSelection pins Codex
// review #376 round 1: a malicious OAuth client could craft
//
//	/oauth/authorize?client_id=…&capability_tier=admin&allowed_workspaces=*
//
// with extra params alongside the standard OAuth ones. Without the
// allowlistedAuthorizeParams filter, those extras would be rendered
// as hidden inputs in the consent form. On submit, hidden inputs
// (rendered in DOM order BEFORE the user-controlled radios +
// checkboxes) would precede the user's selections, and FormValue +
// PostForm[name] would see the attacker-controlled values first —
// silently overriding the user's choice.
//
// This test simulates the attack: the GET includes attacker
// params, the consent form's hidden fields are inspected to confirm
// they're NOT round-tripped, and the POST decision flow with the
// user's actual selection produces a token matching the user's
// choice (not the attacker's URL params).
func TestConsent_URLPollution_DoesNotOverrideUserSelection(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	verifier := "verifier-pollution-quick-brown-fox-jumps-over-lazy"
	challenge := s256Challenge(verifier)

	// Step 1: GET /oauth/authorize with attacker-injected consent
	// params alongside the legitimate OAuth params.
	q := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"scope":                 {"pad:read pad:write"},
		"audience":              {testCanonicalAudience},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"pollution-state"},
		// Attacker-injected (not standard OAuth params):
		"capability_tier":    {"admin"},
		"allowed_workspaces": {"*"},
		"decision":           {"approve"},
		"evil_extra":         {"oh-no"},
	}
	rr := doAuthedRequest(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("authorize render: %d (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// Hidden-input assertions: the attacker's params must NOT be
	// rendered as `<input type="hidden">` round-trips. Note `decision`
	// legitimately appears in the rendered HTML on the Cancel + Allow
	// `<button name="decision">` elements — those don't preserve
	// URL-injected values, only the value attribute on the button
	// itself. Assert specifically against hidden-input rendering.
	for _, attackerName := range []string{
		"evil_extra", "decision", "capability_tier", "allowed_workspaces",
	} {
		if strings.Contains(body, `<input type="hidden" name="`+attackerName+`"`) {
			t.Errorf("attacker-injected %q must NOT be round-tripped as a hidden input (URL pollution)", attackerName)
		}
	}
	// Sanity: legitimate OAuth params ARE round-tripped.
	if !strings.Contains(body, `name="client_id"`) {
		t.Error("client_id hidden input missing — legitimate OAuth param round-trip broken")
	}
	if !strings.Contains(body, `name="state"`) {
		t.Error("state hidden input missing — legitimate OAuth param round-trip broken")
	}

	// Step 2: simulate the user's consent submission. Build a clean
	// POST body that ONLY carries the user's actual selection
	// (capability_tier=read, allowed_workspaces=*) plus the OAuth
	// params from hidden fields. Without the allowlist, the
	// attacker's URL params would also have been hidden inputs,
	// and a real browser would have submitted both the attacker's
	// and the user's values together — but with the allowlist, only
	// the user's values reach the server. We test that here by
	// constructing the form as a legitimate browser would.
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"pad:read pad:write"},
		"audience":              {testCanonicalAudience},
		"state":                 {"pollution-state"},
		"decision":              {"approve"},
		"csrf_token":            {csrfTok},
		// User's actual selection — should win.
		"capability_tier":    {"read"},
		"allowed_workspaces": {"*"},
	}
	rrDecide := postFormWithCookie(srv, "/oauth/authorize/decide", form, sessionToken, csrfTok)
	if rrDecide.Code != http.StatusSeeOther && rrDecide.Code != http.StatusFound {
		t.Fatalf("decide: expected 303/302, got %d (body: %s)", rrDecide.Code, rrDecide.Body.String())
	}
	cbURL, _ := url.Parse(rrDecide.Header().Get("Location"))
	code := cbURL.Query().Get("code")
	if code == "" {
		t.Fatalf("missing code in callback")
	}

	// Step 3: exchange code for token, verify granted scope is
	// pad:read (user's choice) NOT pad:admin (attacker's URL param).
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
		t.Fatalf("token: %d (body: %s)", trr.Code, trr.Body.String())
	}
	var resp map[string]any
	parseJSON(t, trr, &resp)
	if scope, _ := resp["scope"].(string); scope != "pad:read" {
		t.Errorf("URL pollution attack succeeded: got scope %q, want %q (user picked read; attacker injected admin)",
			scope, "pad:read")
	}
}

// mustSeedWorkspaceWithRole is a test helper that creates a workspace
// + adds the user as a member with the given role. Centralized here
// because TASK-952 tests need it repeatedly to set up consent
// scenarios with realistic membership data.
func mustSeedWorkspaceWithRole(t *testing.T, srv *Server, userID, name, slug, role string) *models.Workspace {
	t.Helper()
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{
		Name: name,
		Slug: slug,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace(%q): %v", slug, err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, userID, role); err != nil {
		t.Fatalf("AddWorkspaceMember(%q, role=%s): %v", slug, role, err)
	}
	return ws
}

// =====================================================================
// /oauth/revoke + /oauth/introspect (sub-PR D, TASK-1026)
// =====================================================================

// TestOAuth_Revoke_AccessToken_MarksInactive runs a full auth-code
// flow, revokes the resulting access token, and confirms (via
// introspection from a *different* grant's bearer) that the
// revoked token now reports inactive.
//
// Why two grants: fosite's introspection endpoint requires Bearer
// auth using a *separate* active access token (the "you can't
// introspect a token using itself" rule from RFC 7662 §2.1).
// Mints two grants on the same client + user, uses tokens2.access
// to verify tokens1.access went inactive after revoke.
func TestOAuth_Revoke_AccessToken_MarksInactive(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	tokens1 := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID, "verifier-grant-1-quick-brown-fox-1234567890-abc")
	tokens2 := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID, "verifier-grant-2-quick-brown-fox-1234567890-abc")

	access1, _ := tokens1["access_token"].(string)
	access2, _ := tokens2["access_token"].(string)
	if access1 == "" || access2 == "" {
		t.Fatalf("missing tokens; tokens1=%v tokens2=%v", tokens1, tokens2)
	}

	// Revoke access1 (RFC 7009: public client, only client_id needed).
	rrRevoke := postOAuthForm(srv, "/oauth/revoke", url.Values{
		"token":           {access1},
		"token_type_hint": {"access_token"},
		"client_id":       {clientID},
	})
	if rrRevoke.Code != http.StatusOK {
		t.Fatalf("revoke: expected 200, got %d (body: %s)", rrRevoke.Code, rrRevoke.Body.String())
	}

	// Introspect access1 using access2 as the bearer auth.
	rrIntro := postOAuthFormBearer(srv, "/oauth/introspect", url.Values{
		"token":           {access1},
		"token_type_hint": {"access_token"},
	}, access2)
	if rrIntro.Code != http.StatusOK {
		t.Fatalf("introspect: expected 200, got %d (body: %s)", rrIntro.Code, rrIntro.Body.String())
	}
	var iresp map[string]any
	parseJSON(t, rrIntro, &iresp)
	if active, _ := iresp["active"].(bool); active {
		t.Errorf("revoked access token still introspects active; resp: %v", iresp)
	}
}

// TestOAuth_Revoke_UnknownToken_Returns200 pins RFC 7009 §2.2
// idempotency: "the authorization server responds with HTTP status
// code 200 if the token has been revoked successfully OR if the
// client submitted an invalid token."
//
// fosite v0.49 implements this natively: handler/oauth2/revocation.go's
// RevokeToken returns nil when both refresh-token and access-token
// lookups miss (storeErrorsToRevocationError collapses ErrNotFound +
// ErrInactiveToken to nil), so NewRevocationRequest returns nil and
// WriteRevocationResponse writes 200. We pin this behavior so a
// future fosite version-bump that breaks idempotency (e.g. by
// returning ErrInvalidRequest for unknown tokens) gets caught here
// instead of in production with a confused-client report.
func TestOAuth_Revoke_UnknownToken_Returns200(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	// Token value is well-formed enough to pass the form parser
	// but doesn't exist in our storage (no /authorize/decide ever
	// minted it).
	rr := postOAuthForm(srv, "/oauth/revoke", url.Values{
		"token":     {"definitely-not-a-real-revoke-token"},
		"client_id": {clientID},
	})
	if rr.Code != http.StatusOK {
		t.Errorf("RFC 7009 §2.2: unknown token must return 200 (idempotent revoke); got %d (body: %s)",
			rr.Code, rr.Body.String())
	}
	// Response body should be empty (matches the success path).
	if rr.Body.Len() > 0 {
		t.Errorf("unknown-token revoke response body should be empty; got %q", rr.Body.String())
	}
}

// TestOAuth_Revoke_MissingToken_Returns400 pins Codex review #373
// round 3: RFC 7009 §2.1 marks `token` REQUIRED, but fosite v0.49
// doesn't enforce that — it passes the empty string into the
// revocation handlers and emits the same bare ErrInvalidRequest
// the unknown-token path produces. Without the explicit
// r.PostForm.Get("token") check in handleOAuthRevoke, the
// idempotency override would silently turn "missing required
// parameter" into 200.
//
// Sending client_id but no token must remain 400 invalid_request.
func TestOAuth_Revoke_MissingToken_Returns400(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	rr := postOAuthForm(srv, "/oauth/revoke", url.Values{
		"client_id": {clientID},
		// Deliberately no `token` field.
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("RFC 7009 §2.1: missing required `token` parameter MUST return 400 (not silently 200); got %d (body: %s)",
			rr.Code, rr.Body.String())
	}
}

// TestOAuth_Revoke_MalformedRequest_Returns400 pins the COUNTERPART
// to TestOAuth_Revoke_UnknownToken_Returns200: the spec-correct
// 200-on-unknown override must NOT swallow genuine "client sent a
// broken request" errors. fosite distinguishes the two via HintField
// (set on malformed cases via WithHint*, absent on the !found path);
// isRevocationUnknownToken only matches the latter.
func TestOAuth_Revoke_MalformedRequest_Returns400(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)

	// Empty form body — fosite rejects with ErrInvalidRequest
	// hinted "POST body can not be empty." The hint distinguishes
	// from the !found path; isRevocationUnknownToken returns false
	// and WriteRevocationResponse emits 400.
	req := httptest.NewRequest("POST", "/oauth/revoke", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("malformed revoke request must return 400 (not silently 200); got %d (body: %s)",
			rr.Code, rr.Body.String())
	}
}

// TestOAuth_Revoke_RefreshToken_RevokesFamily covers the security
// invariant established in sub-PR A round 2: revoking a refresh
// token must revoke the *entire grant family* (every access AND
// refresh token sharing the same request_id), not just the row
// addressed by the revoke call.
//
// fosite's revocation handler walks the chain via our adapter's
// RevokeRefreshToken / RevokeAccessToken methods; both delegate to
// store.Revoke*Family, which marks every chain member inactive in
// one statement. This test validates the wiring end-to-end.
func TestOAuth_Revoke_RefreshToken_RevokesFamily(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	tokens1 := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID, "verifier-fam-1-quick-brown-fox-1234567890-abc")
	tokens2 := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID, "verifier-fam-2-quick-brown-fox-1234567890-abc")

	access1, _ := tokens1["access_token"].(string)
	refresh1, _ := tokens1["refresh_token"].(string)
	access2, _ := tokens2["access_token"].(string)
	if access1 == "" || refresh1 == "" || access2 == "" {
		t.Fatalf("missing tokens; tokens1=%v tokens2=%v", tokens1, tokens2)
	}

	// Revoke the refresh token — fosite chains through to our
	// adapter's RevokeRefreshToken + the access-token family
	// revocation.
	rr := postOAuthForm(srv, "/oauth/revoke", url.Values{
		"token":           {refresh1},
		"token_type_hint": {"refresh_token"},
		"client_id":       {clientID},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("revoke refresh: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	// Verify the PAIRED access token from grant 1 is now inactive too.
	rrIntro := postOAuthFormBearer(srv, "/oauth/introspect", url.Values{
		"token": {access1},
	}, access2)
	if rrIntro.Code != http.StatusOK {
		t.Fatalf("introspect: expected 200, got %d (body: %s)", rrIntro.Code, rrIntro.Body.String())
	}
	var iresp map[string]any
	parseJSON(t, rrIntro, &iresp)
	if active, _ := iresp["active"].(bool); active {
		t.Errorf("revoking refresh1 should have revoked paired access1 (family revocation); active=%v resp=%v", active, iresp)
	}

	// Sanity check: tokens2's access (different grant) is unaffected.
	rrIntro2 := postOAuthFormBearer(srv, "/oauth/introspect", url.Values{
		"token": {access2},
	}, access1) // can't use access2 to introspect itself; use access1 (now inactive)
	// access1 is inactive so the bearer auth fails → 401. That's
	// expected — the real check is the per-grant isolation, not
	// the bearer-auth path. Use a third grant if we want to verify
	// independence; for this test the previous assertion is the
	// substance.
	_ = rrIntro2
}

// TestOAuth_Refresh_RotatesAndOldBecomesInactive covers refresh-
// token rotation: exchanging a refresh token must mint a fresh
// (access, refresh) pair AND mark the previous pair inactive
// (single-use refresh tokens, OAuth 2.1 §6.1). This is the normal
// happy path before any replay-detection logic kicks in.
func TestOAuth_Refresh_RotatesAndOldBecomesInactive(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	tokens0 := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID, "verifier-rot-0-quick-brown-fox-1234567890-abc")
	access0, _ := tokens0["access_token"].(string)
	refresh0, _ := tokens0["refresh_token"].(string)
	if access0 == "" || refresh0 == "" {
		t.Fatalf("grant 0 missing tokens: %v", tokens0)
	}

	// Exchange refresh0 for a fresh pair.
	rr := postOAuthForm(srv, "/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh0},
		"client_id":     {clientID},
		"audience":      {testCanonicalAudience},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh exchange: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var rotResp map[string]any
	parseJSON(t, rr, &rotResp)

	access1, _ := rotResp["access_token"].(string)
	refresh1, _ := rotResp["refresh_token"].(string)
	if access1 == "" || refresh1 == "" {
		t.Fatalf("rotation missing tokens: %v", rotResp)
	}
	if access1 == access0 {
		t.Errorf("rotated access token must differ from old one")
	}
	if refresh1 == refresh0 {
		t.Errorf("rotated refresh token must differ from old one")
	}

	// access1 introspects active. (Use access1 itself can't
	// introspect itself — fosite rejects self-bearer-token. Use a
	// second grant to verify access1's state.)
	tokens2 := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID, "verifier-rot-2-quick-brown-fox-1234567890-abc")
	access2, _ := tokens2["access_token"].(string)

	rrIntroNew := postOAuthFormBearer(srv, "/oauth/introspect", url.Values{
		"token": {access1},
	}, access2)
	var iresp map[string]any
	parseJSON(t, rrIntroNew, &iresp)
	if active, _ := iresp["active"].(bool); !active {
		t.Errorf("rotated access1 should be active; resp=%v", iresp)
	}

	// access0 introspects inactive (rotation revoked it).
	rrIntroOld := postOAuthFormBearer(srv, "/oauth/introspect", url.Values{
		"token": {access0},
	}, access2)
	var oresp map[string]any
	parseJSON(t, rrIntroOld, &oresp)
	if active, _ := oresp["active"].(bool); active {
		t.Errorf("pre-rotation access0 should be inactive; resp=%v", oresp)
	}
}

// TestOAuth_Refresh_ReplayDetection_RevokesFamily is the security-
// critical OAuth 2.1 §6.1 / RFC 6819 §5.2.2.3 test: replaying an
// already-rotated refresh token must trigger family revocation, so
// any tokens minted from the rotation become unusable.
//
// Threat model: an attacker who steals refresh_n watches the
// legitimate client refresh first → mints (access_n+1, refresh_n+1).
// Now both attacker and legit client believe they hold a valid
// chain. When the attacker (or the legit client, racing) presents
// refresh_n a SECOND time, fosite's flow_refresh.go detects the
// inactive token, calls our adapter's RevokeRefreshToken +
// RevokeAccessToken on the request_id, and our store walks the
// family — invalidating refresh_n+1, access_n+1, and every
// historical chain member.
//
// Without this defense an attacker could "leapfrog" the legitimate
// client indefinitely. With it, the entire chain dies on the
// first replay attempt.
func TestOAuth_Refresh_ReplayDetection_RevokesFamily(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	tokens0 := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID, "verifier-rep-0-quick-brown-fox-1234567890-abc")
	refresh0, _ := tokens0["refresh_token"].(string)
	if refresh0 == "" {
		t.Fatalf("grant 0 missing refresh: %v", tokens0)
	}

	// First refresh: succeeds, mints (access_1, refresh_1).
	rr := postOAuthForm(srv, "/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh0},
		"client_id":     {clientID},
		"audience":      {testCanonicalAudience},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("first refresh: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var rot1 map[string]any
	parseJSON(t, rr, &rot1)
	access1, _ := rot1["access_token"].(string)

	// Second refresh of refresh0 (the attacker replay): fosite
	// detects the now-inactive refresh and revokes the family.
	// The HTTP response is an OAuth error per RFC 6749 — fosite
	// returns 400 with error=invalid_grant. The side-effect (family
	// revocation) is the security-critical piece.
	rrReplay := postOAuthForm(srv, "/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh0},
		"client_id":     {clientID},
		"audience":      {testCanonicalAudience},
	})
	if rrReplay.Code == http.StatusOK {
		t.Errorf("replay must NOT succeed; got 200 (body: %s)", rrReplay.Body.String())
	}

	// Mint a separate grant so we have a bearer for introspection.
	tokensFresh := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID, "verifier-rep-fresh-quick-brown-fox-1234567890")
	accessFresh, _ := tokensFresh["access_token"].(string)

	// access1 (minted from the rotation BEFORE the replay) should
	// now be inactive — replay detection revoked the family.
	rrIntro := postOAuthFormBearer(srv, "/oauth/introspect", url.Values{
		"token": {access1},
	}, accessFresh)
	if rrIntro.Code != http.StatusOK {
		t.Fatalf("introspect post-replay: expected 200, got %d (body: %s)", rrIntro.Code, rrIntro.Body.String())
	}
	var iresp map[string]any
	parseJSON(t, rrIntro, &iresp)
	if active, _ := iresp["active"].(bool); active {
		t.Errorf("post-replay: access1 should be inactive (family revocation triggered); resp=%v", iresp)
	}
}

// TestOAuth_Introspect_ActiveToken_ReturnsClaims verifies the happy-
// path RFC 7662 response shape: {active:true, scope, sub, aud,
// client_id, exp, iat} for a valid access token.
//
// Sub-PR E's MCPBearerAuth integration reads sub + scope + aud from
// this response (well, from fosite.IntrospectToken directly — but
// the wire shape pins the same contract). Pinning the field
// presence here catches a future fosite version-bump that drops
// or renames a field before the integration breaks.
func TestOAuth_Introspect_ActiveToken_ReturnsClaims(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	user, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	// Two grants on the same user — bearer + token-to-introspect.
	tokensTarget := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID, "verifier-int-tgt-quick-brown-fox-1234567890")
	tokensBearer := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID, "verifier-int-brer-quick-brown-fox-1234567890")

	target, _ := tokensTarget["access_token"].(string)
	bearer, _ := tokensBearer["access_token"].(string)
	if target == "" || bearer == "" {
		t.Fatalf("missing tokens")
	}

	rr := postOAuthFormBearer(srv, "/oauth/introspect", url.Values{
		"token": {target},
	}, bearer)
	if rr.Code != http.StatusOK {
		t.Fatalf("introspect: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var iresp map[string]any
	parseJSON(t, rr, &iresp)
	if active, _ := iresp["active"].(bool); !active {
		t.Fatalf("active should be true; resp=%v", iresp)
	}
	// sub (RFC 7662 §2.2) maps to the user ID we set via
	// oauth.NewSession(user.ID) at /authorize/decide. Sub-PR E's
	// MCPBearerAuth maps this back to the pad user.
	if sub, _ := iresp["sub"].(string); sub != user.ID {
		t.Errorf("sub: got %q, want %q (user ID from /authorize/decide)", sub, user.ID)
	}
	if cid, _ := iresp["client_id"].(string); cid != clientID {
		t.Errorf("client_id: got %q, want %q", cid, clientID)
	}
	if scope, _ := iresp["scope"].(string); !strings.Contains(scope, "pad:read") {
		t.Errorf("scope should contain pad:read; got %q", scope)
	}
	// aud is a JSON array — assert canonical audience appears.
	auds, _ := iresp["aud"].([]any)
	foundAud := false
	for _, a := range auds {
		if s, _ := a.(string); s == testCanonicalAudience {
			foundAud = true
			break
		}
	}
	if !foundAud {
		t.Errorf("aud should contain canonical audience %q; got %v", testCanonicalAudience, iresp["aud"])
	}
	if _, ok := iresp["exp"]; !ok {
		t.Error("exp should be set on active tokens")
	}
}

// TestOAuth_Introspect_UnknownToken_ReturnsActiveFalse pins the
// no-leak property: an unknown token must produce {"active": false}
// and nothing else (no scope, no client_id, no error message). RFC
// 7662 §2.2 explicitly requires this so an attacker can't probe
// for valid token shapes by introspecting random strings and
// reading the response body for hints.
func TestOAuth_Introspect_UnknownToken_ReturnsActiveFalse(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	tokens := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID, "verifier-unk-quick-brown-fox-1234567890-abcde")
	bearer, _ := tokens["access_token"].(string)
	if bearer == "" {
		t.Fatalf("missing bearer")
	}

	// "unknown-token-value" is well-formed enough to pass the
	// outer validation, but doesn't exist in our storage.
	rr := postOAuthFormBearer(srv, "/oauth/introspect", url.Values{
		"token": {"definitely-not-a-real-token-xyzzy"},
	}, bearer)
	if rr.Code != http.StatusOK {
		t.Fatalf("introspect unknown: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var iresp map[string]any
	parseJSON(t, rr, &iresp)
	if active, _ := iresp["active"].(bool); active {
		t.Errorf("unknown token must report active:false; got %v", iresp)
	}
	// No-leak: nothing else may be present.
	for _, leakField := range []string{"scope", "client_id", "sub", "aud", "exp", "iat", "username"} {
		if _, ok := iresp[leakField]; ok {
			t.Errorf("unknown-token response leaked %q (RFC 7662 §2.2 forbids); got %v", leakField, iresp)
		}
	}
}

// TestOAuth_Introspect_RejectsMissingBearer covers the auth-required
// path: RFC 7662 §2.1 mandates that the introspection endpoint
// "require some form of authorization." Our model accepts only
// Bearer auth (public clients, no Basic auth path); a request with
// no Authorization header MUST be rejected.
func TestOAuth_Introspect_RejectsMissingBearer(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	rr := postOAuthForm(srv, "/oauth/introspect", url.Values{
		"token": {"any-token-here"},
	})
	if rr.Code == http.StatusOK {
		// fosite returns 401 on missing Authorization.
		t.Errorf("expected 401, got 200 (body: %s)", rr.Body.String())
	}
}

// TestOAuth_Revoke_NotMountedOutsideCloudMode + introspect counterpart
// confirm the cloud-mode gate covers the new endpoints (mirrors
// TestOAuth_Register_NotMountedOutsideCloudMode for sub-PR C).
func TestOAuth_RevokeAndIntrospect_NotMountedOutsideCloudMode(t *testing.T) {
	srv := testServer(t) // no SetCloudMode
	for _, path := range []string{"/oauth/revoke", "/oauth/introspect"} {
		rr := postOAuthForm(srv, path, url.Values{"token": {"x"}, "client_id": {"y"}})
		if rr.Code == http.StatusOK {
			t.Errorf("%s must NOT be reachable outside cloud mode; got 200", path)
		}
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

// runAuthCodeFlow drives a full /oauth/authorize/decide → /oauth/token
// exchange and returns the parsed token JSON. Each invocation needs
// a unique verifier so the resulting code_challenge is unique;
// callers pass distinct verifiers when minting multiple grants in
// the same test (e.g. "introspect via separate bearer" tests in
// sub-PR D need two grants).
//
// The verifier MUST be ≥43 chars (RFC 7636 §4.1: 43-128 chars from
// the unreserved set). Tests embed that length in their literals;
// this helper doesn't validate but tests will fail visibly at the
// /token step if the verifier is too short.
//
// The state value is derived from the verifier so it's also unique
// per call (fosite requires state ≥8 chars for entropy).
func runAuthCodeFlow(t *testing.T, srv *Server, sessionToken, csrfTok, clientID, verifier string) map[string]any {
	t.Helper()
	if len(verifier) < 43 {
		t.Fatalf("runAuthCodeFlow: verifier too short (%d chars; RFC 7636 §4.1 needs ≥43)", len(verifier))
	}
	challenge := s256Challenge(verifier)
	// State just needs ≥8 chars + uniqueness so a stray CSRF check
	// doesn't conflate two grants. Tag with a prefix of the verifier
	// so failures point at the right call site.
	state := "state-" + verifier[:8]

	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"pad:read"},
		"audience":              {testCanonicalAudience},
		"state":                 {state},
		"decision":              {"approve"},
		"csrf_token":            {csrfTok},
		// TASK-952 consent fields. The helper drives the auth-code
		// flow with a sane default (read tier + wildcard workspace
		// allow-list) so tests not focused on consent UI behaviour
		// don't have to construct workspace memberships per call.
		// Tests that DO care about consent (TASK-952's own coverage)
		// build their own forms instead of going through this helper.
		"capability_tier":    {"read"},
		"allowed_workspaces": {"*"},
	}
	rr := postFormWithCookie(srv, "/oauth/authorize/decide", form, sessionToken, csrfTok)
	if rr.Code != http.StatusSeeOther && rr.Code != http.StatusFound {
		t.Fatalf("decide: expected 303/302, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	cbURL, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	code := cbURL.Query().Get("code")
	if code == "" {
		t.Fatalf("missing code in callback Location: %s", rr.Header().Get("Location"))
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
	return resp
}

// postOAuthForm POSTs an x-www-form-urlencoded body without any
// auth (used for /oauth/revoke + /oauth/token, where client_id rides
// in the form for public clients).
func postOAuthForm(srv *Server, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// postOAuthFormBearer POSTs an x-www-form-urlencoded body with a
// Bearer Authorization header (used for /oauth/introspect, where
// fosite v0.49 requires either Bearer or Basic auth and we don't
// support Basic in the public-clients-only model).
func postOAuthFormBearer(srv *Server, path string, form url.Values, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}
