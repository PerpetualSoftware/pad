package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/config"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/oauth"
)

// TASK-3394 (SPEC-6 U5a): install clients' service tokens, the in-process
// app-token check, the public introspection hardening and the A0 gate.

const testAppAPIAudience = "https://app.test.example/api/app/v1"

// appOAuthServer is an OAuth server that also knows the app API resource.
// cloud=true makes both MCP and apps available; cloud=false is a self-host
// with MCP off (apps follow the apps_enabled setting).
func appOAuthServer(t *testing.T, cloud bool) *Server {
	t.Helper()
	srv := testServer(t)
	if cloud {
		srv.SetCloudMode("test-secret")
	} else {
		// A self-host with an https origin and MCP left off.
		cfg := config.Config{URL: testAuthServerURL}
		srv.SetMCPConfig(cfg.ResolveMCPEndpoints(), nil)
	}
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	srv.SetMCPTransport(stub, testCanonicalAudience, testAuthServerURL, nil)
	o, err := oauth.NewServer(oauth.Config{
		Store:           srv.store,
		HMACSecret:      bytes32ForTest(),
		AllowedAudience: testCanonicalAudience,
		AppAPIAudience:  testAppAPIAudience,
	})
	if err != nil {
		t.Fatalf("oauth.NewServer: %v", err)
	}
	srv.SetOAuthServer(o)
	srv.store.SetAppAPIAudience(testAppAPIAudience)
	return srv
}

type testInstall struct {
	id, clientID, secret string
	bot                  *models.User
	wsID                 string
}

func newTestInstall(t *testing.T, srv *Server, installID string) testInstall {
	t.Helper()
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "WS " + installID})
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(`INSERT INTO app_installs (id, workspace_id, origin, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		installID, ws.ID, "https://portal.example", ts, ts); err != nil {
		t.Fatal(err)
	}
	tx, err := srv.store.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	bot, err := srv.store.CreateAppUserTx(tx, installID, "Portal")
	if err != nil {
		t.Fatal(err)
	}
	clientID, secret, err := srv.store.CreateInstallClientTx(tx, installID, []string{"https://portal.example/cb"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return testInstall{id: installID, clientID: clientID, secret: secret, bot: bot, wsID: ws.ID}
}

func postTokenBasic(srv *Server, form url.Values, clientID, secret string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(clientID), url.QueryEscape(secret))
	req.RemoteAddr = "192.0.2.1:1234"
	req.Host = "app.test.example" // the configured origin's host (self-host refuses others)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func serviceTokenForm(resource string) url.Values {
	f := url.Values{"grant_type": {"client_credentials"}}
	if resource != "" {
		f.Set("resource", resource)
	}
	return f
}

func mintServiceToken(t *testing.T, srv *Server, in testInstall) string {
	t.Helper()
	rr := postTokenBasic(srv, serviceTokenForm(testAppAPIAudience), in.clientID, in.secret)
	if rr.Code != http.StatusOK {
		t.Fatalf("service token: %d %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	parseJSON(t, rr, &resp)
	tok, _ := resp["access_token"].(string)
	if tok == "" {
		t.Fatalf("no access token: %s", rr.Body.String())
	}
	if _, has := resp["refresh_token"]; has {
		t.Error("a service token came with a refresh token")
	}
	return tok
}

func TestTask3394_ServiceTokenForTheInstallsBot(t *testing.T) {
	srv := appOAuthServer(t, true)
	in := newTestInstall(t, srv, "inst-svc")
	tok := mintServiceToken(t, srv, in)
	g, err := srv.introspectAppToken(context.Background(), tok)
	if err != nil {
		t.Fatalf("introspectAppToken: %v", err)
	}
	if g.Subject != in.bot.ID || g.InstallID != in.id || g.ClientID != in.clientID || g.WorkspaceID != in.wsID ||
		g.AuthKind != "service" || g.AuthEpoch != 1 {
		t.Errorf("grant = %+v", g)
	}
}

func TestTask3394_InstallTokenRequestRules(t *testing.T) {
	srv := appOAuthServer(t, true)
	in := newTestInstall(t, srv, "inst-rules")
	cases := []struct {
		name string
		form url.Values
	}{
		{"no resource", serviceTokenForm("")},
		{"the MCP resource", serviceTokenForm(testCanonicalAudience)},
		{"an unknown resource", serviceTokenForm("https://elsewhere.example/api")},
		{"audience and resource disagree", func() url.Values {
			f := serviceTokenForm(testAppAPIAudience)
			f.Set("audience", testCanonicalAudience)
			return f
		}()},
		{"a delegated grant", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x"}, "resource": {testAppAPIAudience}}},
	}
	for _, tc := range cases {
		rr := postTokenBasic(srv, tc.form, in.clientID, in.secret)
		if rr.Code < 400 || rr.Code >= 500 {
			t.Errorf("%s: %d %s, want a 4xx", tc.name, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "access_token") {
			t.Errorf("%s: a token was issued", tc.name)
		}
	}
	if rr := postTokenBasic(srv, serviceTokenForm(testAppAPIAudience), in.clientID, "padapp_wrong"); rr.Code != http.StatusUnauthorized {
		t.Errorf("wrong secret: %d %s, want 401", rr.Code, rr.Body.String())
	}
	if n := countRows(t, srv, `SELECT COUNT(*) FROM oauth_access_tokens WHERE client_id = ?`, in.clientID); n != 0 {
		t.Errorf("%d tokens persisted by refused requests", n)
	}
}

// client_credentials is for installed apps only, and a bot subject is
// refused for any client but its own install's (lead ruling R1).
func TestTask3394_ClientCredentialsIsForInstallClientsOnly(t *testing.T) {
	srv := appOAuthServer(t, true)
	dcr := registerTestClient(t, srv, "https://app.test/cb")
	rr := postOAuthForm(srv, "/oauth/token", url.Values{"grant_type": {"client_credentials"}, "client_id": {dcr}, "resource": {testCanonicalAudience}})
	if rr.Code < 400 || strings.Contains(rr.Body.String(), "access_token") {
		t.Errorf("a DCR client's client_credentials: %d %s", rr.Code, rr.Body.String())
	}
}

func TestTask3394_IntrospectAppTokenRefusals(t *testing.T) {
	srv := appOAuthServer(t, true)
	ctx := context.Background()

	in := newTestInstall(t, srv, "inst-epoch")
	tok := mintServiceToken(t, srv, in)
	if _, err := srv.store.DB().Exec(`UPDATE app_installs SET auth_epoch = auth_epoch + 1 WHERE id = ?`, in.id); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.introspectAppToken(ctx, tok); !errors.Is(err, errAppTokenEpoch) {
		t.Errorf("after an epoch bump: %v, want errAppTokenEpoch", err)
	}

	in2 := newTestInstall(t, srv, "inst-state")
	tok2 := mintServiceToken(t, srv, in2)
	if _, err := srv.store.DB().Exec(`UPDATE app_installs SET state = 'inactive' WHERE id = ?`, in2.id); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.introspectAppToken(ctx, tok2); !errors.Is(err, errAppTokenInstall) {
		t.Errorf("inactive install: %v, want errAppTokenInstall", err)
	}

	in3 := newTestInstall(t, srv, "inst-cdis")
	tok3 := mintServiceToken(t, srv, in3)
	if _, err := srv.store.DB().Exec(`UPDATE oauth_clients SET disabled_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), in3.clientID); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.introspectAppToken(ctx, tok3); !errors.Is(err, errAppTokenDisabled) {
		t.Errorf("disabled client: %v, want errAppTokenDisabled", err)
	}

	// An MCP token is not an app token, and neither is a refresh token.
	sess := newOAuthSession(t, srv)
	mcpTok, code := mintWithResource(t, srv, sess, testCanonicalAudience)
	if code != http.StatusOK {
		t.Fatalf("mcp mint: %d", code)
	}
	if _, err := srv.introspectAppToken(ctx, mcpTok); !errors.Is(err, errAppTokenAudience) {
		t.Errorf("an MCP token: %v, want errAppTokenAudience", err)
	}
	if _, err := srv.introspectAppToken(ctx, lastRefresh); !errors.Is(err, errAppTokenInactive) {
		t.Errorf("a refresh token: %v, want errAppTokenInactive", err)
	}
	if _, err := srv.introspectAppToken(ctx, "not-a-token"); !errors.Is(err, errAppTokenInactive) {
		t.Errorf("garbage: %v, want errAppTokenInactive", err)
	}
}

// /mcp keeps refusing a bot, its service token included.
func TestTask3394_MCPRefusesABotsServiceToken(t *testing.T) {
	srv := appOAuthServer(t, true)
	in := newTestInstall(t, srv, "inst-mcp")
	tok := mintServiceToken(t, srv, in)
	if rr := postMCP(srv, "/mcp", tok); rr.Code != http.StatusUnauthorized {
		t.Errorf("a bot's service token at /mcp: %d, want 401", rr.Code)
	}
}

// Lead ruling R2: install clients do not use the public introspection
// endpoint, and it never describes their tokens.
func TestTask3394_PublicIntrospectionAndInstallClients(t *testing.T) {
	srv := appOAuthServer(t, true)
	in := newTestInstall(t, srv, "inst-intro")
	tok := mintServiceToken(t, srv, in)

	// An install client as the caller, by Basic credentials.
	req := httptest.NewRequest("POST", "/oauth/introspect", strings.NewReader(url.Values{"token": {tok}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(in.clientID, in.secret)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized || strings.Contains(rr.Body.String(), `"active":true`) {
		t.Errorf("install client as caller (Basic): %d %s, want 401", rr.Code, rr.Body.String())
	}
	// ... and by its own token as the Bearer.
	if rr := postOAuthFormBearer(srv, "/oauth/introspect", url.Values{"token": {tok}}, tok); rr.Code != http.StatusUnauthorized {
		t.Errorf("install token as the bearer: %d %s, want 401", rr.Code, rr.Body.String())
	}
	// An MCP client asking about an install token learns nothing.
	sess := newOAuthSession(t, srv)
	mcpTok, code := mintWithResource(t, srv, sess, testCanonicalAudience)
	if code != http.StatusOK {
		t.Fatalf("mcp mint: %d", code)
	}
	rr = postOAuthFormBearer(srv, "/oauth/introspect", url.Values{"token": {tok}}, mcpTok)
	if strings.Contains(rr.Body.String(), `"active":true`) || strings.Contains(rr.Body.String(), in.bot.ID) {
		t.Errorf("an install token described to an MCP client: %d %s", rr.Code, rr.Body.String())
	}
	// The endpoint still answers an MCP client about another MCP token
	// (fosite refuses a bearer identical to the token it describes).
	mcpTok2, code := mintWithResource(t, srv, sess, testCanonicalAudience)
	if code != http.StatusOK {
		t.Fatalf("mcp mint 2: %d", code)
	}
	rr = postOAuthFormBearer(srv, "/oauth/introspect", url.Values{"token": {mcpTok2}}, mcpTok)
	if !strings.Contains(rr.Body.String(), `"active":true`) {
		t.Errorf("control: an MCP token's self-introspection: %d %s", rr.Code, rr.Body.String())
	}
}

// A0: with MCP off and apps on, the token endpoint serves install clients
// and nobody else, and the rest of the authorization server stays off.
func TestTask3394_TokenEndpointWithMCPOffAndAppsOn(t *testing.T) {
	srv := appOAuthServer(t, false)
	in := newTestInstall(t, srv, "inst-a0")
	if srv.oauthAvailable() {
		t.Fatal("fixture: MCP is on")
	}
	// Apps off as well: nothing.
	if rr := postTokenBasic(srv, serviceTokenForm(testAppAPIAudience), in.clientID, in.secret); rr.Code != http.StatusNotFound {
		t.Errorf("apps off: %d, want 404", rr.Code)
	}
	if err := srv.store.SetPlatformSetting(settingAppsEnabled, "true"); err != nil {
		t.Fatal(err)
	}
	if !srv.appsAvailable() {
		t.Fatal("fixture: apps are not available")
	}
	mintServiceToken(t, srv, in)
	dcr, err := srv.store.CreateOAuthClient(models.OAuthClientCreate{Name: "DCR", RedirectURIs: []string{"https://x.test/cb"},
		GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: "none",
		Scopes: []string{"pad:read"}, Public: true})
	if err != nil {
		t.Fatal(err)
	}
	dreq := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x"}, "client_id": {dcr.ID}}.Encode()))
	dreq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	dreq.RemoteAddr = "192.0.2.1:1234"
	dreq.Host = "app.test.example"
	drr := httptest.NewRecorder()
	srv.ServeHTTP(drr, dreq)
	if rr := drr; rr.Code != http.StatusNotFound {
		t.Errorf("a DCR client with MCP off: %d, want 404", rr.Code)
	}
	for _, path := range []string{"/oauth/register", "/oauth/introspect", "/oauth/revoke"} {
		if rr := postOAuthForm(srv, path, url.Values{}); rr.Code != http.StatusNotFound {
			t.Errorf("%s with MCP off: %d, want 404", path, rr.Code)
		}
	}
}
