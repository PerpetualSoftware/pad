package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/config"
	mcpserver "github.com/PerpetualSoftware/pad/internal/mcp"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// PLAN-2310 U2 acceptance (DR-1, DR-4, DR-5). Every server here is built
// through wireMCP, the production binding, over a real migrated store,
// so a route that is gated but never mounted, or constructed but never
// wired, fails here rather than passing a test that wired it by hand
// (CONVE-19).

// capRoute is one row of DR-5's route table.
type capRoute struct {
	method, path, body string
	contentType        string
	auth               string // "", "member", "admin"
	gate               string // "mcp", "oauth", or "history" (never gated)
}

// capRoutes is DR-5's table, all 23 routes. {id} and {ws} are replaced
// per fixture. Unknown connection and client ids are deliberate: each of
// those handlers answers its own 404 ("Connection not found.", "client
// not found"), which isGateRefusal tells apart from the gate's.
func capRoutes() []capRoute {
	form := "application/x-www-form-urlencoded"
	js := "application/json"
	return []capRoute{
		{method: "POST", path: "/mcp", body: `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, contentType: js, gate: "mcp"},
		{method: "POST", path: "/mcp/", body: `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, contentType: js, gate: "mcp"},

		{method: "GET", path: "/.well-known/oauth-protected-resource", gate: "oauth"},
		{method: "GET", path: "/.well-known/oauth-protected-resource/mcp", gate: "oauth"},
		{method: "GET", path: "/.well-known/oauth-protected-resource/mcp/", gate: "oauth"},
		{method: "GET", path: "/.well-known/oauth-authorization-server", gate: "oauth"},

		{method: "POST", path: "/oauth/register", body: `{}`, contentType: js, gate: "oauth"},
		{method: "GET", path: "/oauth/authorize", gate: "oauth"},
		{method: "POST", path: "/oauth/authorize/decide", body: "x=1", contentType: form, gate: "oauth"},
		{method: "POST", path: "/oauth/token", body: "x=1", contentType: form, gate: "oauth"},
		{method: "POST", path: "/oauth/revoke", body: "x=1", contentType: form, gate: "oauth"},
		{method: "POST", path: "/oauth/introspect", body: "x=1", contentType: form, gate: "oauth"},

		{method: "GET", path: "/api/v1/connected-apps", auth: "member", gate: "oauth"},
		{method: "DELETE", path: "/api/v1/connected-apps/no-such-connection", auth: "member", gate: "oauth"},
		{method: "PATCH", path: "/api/v1/connected-apps/no-such-connection/name", body: `{"name":"x"}`, contentType: js, auth: "member", gate: "oauth"},
		{method: "PATCH", path: "/api/v1/connected-apps/no-such-connection/flags", body: `{}`, contentType: js, auth: "member", gate: "oauth"},
		{method: "POST", path: "/api/v1/connected-apps/no-such-connection/workspaces", body: `{"workspace":"{ws}"}`, contentType: js, auth: "member", gate: "oauth"},
		{method: "DELETE", path: "/api/v1/connected-apps/no-such-connection/workspaces/{ws}", auth: "member", gate: "oauth"},
		{method: "GET", path: "/api/v1/connected-apps/no-such-connection/audit", auth: "member", gate: "history"},
		{method: "GET", path: "/api/v1/oauth/clients/no-such-client/public-info", auth: "member", gate: "oauth"},
		{method: "POST", path: "/api/v1/oauth/claim", body: `{}`, contentType: js, auth: "member", gate: "oauth"},
		{method: "GET", path: "/api/v1/workspaces/{ws}/claim-code", auth: "member", gate: "oauth"},
		{method: "GET", path: "/api/v1/admin/mcp-audit", auth: "admin", gate: "history"},
	}
}

type capFixture struct {
	store     *store.Store
	srv       *server.Server
	keyBytes  []byte
	admin     *models.User
	member    *models.User
	ws        *models.Workspace
	adminPAT  string
	memberPAT string
}

// newCapFixture builds a store the way an existing install has it —
// migrated, users present, a workspace, PATs, and NO mcp_enabled row —
// then a server over it wired exactly as `pad server` wires it:
// SetMCPConfig with the resolved endpoints, then wireMCP. setting, when
// non-empty, is written to platform_settings BEFORE the server is built.
func newCapFixture(t *testing.T, cfg config.Config, cloud bool, env *bool, setting string) *capFixture {
	t.Helper()
	s := storetest.NewSQLite(t)
	f := &capFixture{store: s, keyBytes: make([]byte, 32)}
	for i := range f.keyBytes {
		f.keyBytes[i] = byte(i + 7)
	}

	var err error
	if f.admin, err = s.CreateUser(models.UserCreate{Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple"}); err != nil {
		t.Fatalf("CreateUser admin: %v", err)
	}
	if err := s.SetUserRole(f.admin.ID, "admin"); err != nil {
		t.Fatalf("SetUserRole: %v", err)
	}
	if f.member, err = s.CreateUser(models.UserCreate{Email: "member@example.com", Name: "Member", Password: "correct-horse-battery-staple"}); err != nil {
		t.Fatalf("CreateUser member: %v", err)
	}
	if f.ws, err = s.CreateWorkspace(models.WorkspaceCreate{Name: "Cap WS", Slug: "cap-ws", OwnerID: f.member.ID}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := s.AddWorkspaceMember(f.ws.ID, f.member.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	for _, u := range []struct {
		user *models.User
		dst  *string
	}{{f.admin, &f.adminPAT}, {f.member, &f.memberPAT}} {
		tok, err := s.CreateAPIToken(u.user.ID, models.APITokenCreate{Name: "cap"}, 30, 0)
		if err != nil {
			t.Fatalf("CreateAPIToken: %v", err)
		}
		*u.dst = tok.Token
	}
	if setting != "" {
		if err := s.SetPlatformSetting("mcp_enabled", setting); err != nil {
			t.Fatalf("SetPlatformSetting: %v", err)
		}
	}

	f.srv = server.New(s)
	t.Cleanup(f.srv.Stop)
	ep := cfg.ResolveMCPEndpoints()
	f.srv.SetMCPConfig(ep, env)
	if cloud {
		f.srv.SetCloudMode("cap-test-secret")
	}
	if err := wireMCP(newRootCmd(), f.srv, s, ep, f.keyBytes); err != nil {
		t.Fatalf("wireMCP: %v", err)
	}
	return f
}

func (f *capFixture) do(t *testing.T, rt capRoute) *httptest.ResponseRecorder {
	t.Helper()
	path := strings.ReplaceAll(rt.path, "{ws}", f.ws.Slug)
	body := strings.ReplaceAll(rt.body, "{ws}", f.ws.Slug)
	req := httptest.NewRequest(rt.method, path, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.10:4242"
	if rt.contentType != "" {
		req.Header.Set("Content-Type", rt.contentType)
	}
	switch rt.auth {
	case "member":
		req.Header.Set("Authorization", "Bearer "+f.memberPAT)
	case "admin":
		req.Header.Set("Authorization", "Bearer "+f.adminPAT)
	}
	rr := httptest.NewRecorder()
	f.srv.ServeHTTP(rr, req)
	return rr
}

// isGateRefusal reports whether rr is the DR-5 gate's answer: exactly
// the JSON 404 requireMCPAvailable / requireOAuthAvailable write. A
// handler's own 404 names what was not found, and the SPA answers HTML,
// so neither can pass for it.
func isGateRefusal(rr *httptest.ResponseRecorder) bool {
	if rr.Code != http.StatusNotFound {
		return false
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		return false
	}
	return env.Error.Code == "not_found" && env.Error.Message == "Not found"
}

func boolPtr(b bool) *bool { return &b }

var (
	cfgHTTPS  = config.Config{URL: "https://pad.example.com"}
	cfgHTTP   = config.Config{URL: "http://pad.lan:7777"}
	cfgCloud  = config.Config{PublicURL: "https://app.getpad.dev", MCPPublicURL: "https://mcp.getpad.dev", AuthServerURL: "https://app.getpad.dev"}
	cfgNone   = config.Config{Host: "0.0.0.0", Port: 7777}
	cfgBadURL = config.Config{URL: "https://pad.example.com/not-an-origin"}
)

// TestMCPCapability_RouteTable drives every DR-5 route, through the
// router, in each state PLAN-2310's acceptance names, and asserts the
// gate refuses exactly the routes whose predicate is false.
func TestMCPCapability_RouteTable(t *testing.T) {
	states := []struct {
		name           string
		cfg            config.Config
		cloud          bool
		env            *bool
		setting        string
		mcpOn, oauthOn bool
	}{
		{name: "cloud", cfg: cfgCloud, cloud: true, mcpOn: true, oauthOn: true},
		{name: "self-host off", cfg: cfgHTTPS},
		{name: "on with no origin", cfg: cfgNone, setting: "true"},
		{name: "on with an unusable origin", cfg: cfgBadURL, setting: "true"},
		{name: "on with an http origin", cfg: cfgHTTP, setting: "true", mcpOn: true},
		{name: "on with an https origin", cfg: cfgHTTPS, setting: "true", mcpOn: true, oauthOn: true},
		{name: "env-forced on", cfg: cfgHTTPS, env: boolPtr(true), mcpOn: true, oauthOn: true},
		{name: "env-forced off with the setting on", cfg: cfgHTTPS, env: boolPtr(false), setting: "true"},
	}
	for _, st := range states {
		t.Run(st.name, func(t *testing.T) {
			f := newCapFixture(t, st.cfg, st.cloud, st.env, st.setting)
			for _, rt := range capRoutes() {
				rr := f.do(t, rt)
				var available bool
				switch rt.gate {
				case "mcp":
					available = st.mcpOn
				case "oauth":
					available = st.oauthOn
				case "history":
					available = true
				}
				if got := !isGateRefusal(rr); got != available {
					t.Errorf("%s %s: reachable=%v, want %v (status %d, body %.200s)",
						rt.method, rt.path, got, available, rr.Code, rr.Body.String())
				}
				if !available && strings.Contains(rr.Header().Get("Content-Type"), "text/html") {
					t.Errorf("%s %s: the SPA answered an unavailable route", rt.method, rt.path)
				}
			}
			if st.mcpOn {
				f.checkChallenge(t, st.cfg.ResolveMCPEndpoints(), st.oauthOn)
			}
		})
	}
}

// checkChallenge asserts the /mcp 401's WWW-Authenticate (DR-5): with
// OAuth available it carries resource_metadata, and following that URL's
// path through the same router yields the protected-resource document
// naming the configured MCP URL; with MCP on over http it is exactly
// `Bearer realm="pad"`. The request carries a hostile Host, so nothing in
// either answer may come from it (DR-3).
func (f *capFixture) checkChallenge(t *testing.T, ep config.MCPEndpoints, oauthOn bool) {
	t.Helper()
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
	req.Host = "evil.example"
	req.RemoteAddr = "192.0.2.10:4242"
	rr := httptest.NewRecorder()
	f.srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("/mcp with no token: status %d, want 401", rr.Code)
	}
	got := rr.Header().Get("WWW-Authenticate")
	if strings.Contains(got, "evil.example") {
		t.Errorf("WWW-Authenticate names the request's Host: %q", got)
	}
	if !oauthOn {
		if got != `Bearer realm="pad"` {
			t.Errorf("WWW-Authenticate = %q, want exactly %q with OAuth unavailable", got, `Bearer realm="pad"`)
		}
		return
	}
	const marker = `resource_metadata="`
	i := strings.Index(got, marker)
	if i < 0 {
		t.Fatalf("WWW-Authenticate %q has no resource_metadata with OAuth available", got)
	}
	metaURL := strings.TrimSuffix(got[i+len(marker):], `"`)
	u, err := url.Parse(metaURL)
	if err != nil {
		t.Fatalf("resource_metadata %q does not parse: %v", metaURL, err)
	}
	doc := httptest.NewRecorder()
	f.srv.ServeHTTP(doc, httptest.NewRequest("GET", u.Path, nil))
	if doc.Code != http.StatusOK {
		t.Fatalf("resource_metadata %q: GET %s answered %d; the pointer must resolve", metaURL, u.Path, doc.Code)
	}
	var body struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := json.Unmarshal(doc.Body.Bytes(), &body); err != nil {
		t.Fatalf("protected-resource document: %v", err)
	}
	if body.Resource != ep.ResourceURL || len(body.AuthorizationServers) != 1 || body.AuthorizationServers[0] != ep.AuthServerURL {
		t.Errorf("protected-resource document = %+v, want resource %q and authorization server %q", body, ep.ResourceURL, ep.AuthServerURL)
	}
}

// TestMCPCapability_UpgradeChangesNothingWhileOff is the unit's headline
// evidence. It builds a store as an existing install has it (migrated,
// users present, no mcp_enabled row) with an https origin configured, so
// wireMCP constructs OAuth and sets the claim secret: everything an
// upgraded install now has. While MCP is off:
//
//   - every DR-5 route answers the gate's 404, except the two audit
//     history routes, which answer exactly as they do with MCP on;
//   - the in-process dispatcher's claim call, with a code that is valid
//     for the configured key, is refused.
//
// The control leg is the same store with the setting turned on: every
// route is reachable and the same claim succeeds, so none of the
// refusals above is vacuous (a route that was never mounted, or a code
// that was never valid, would be refused in both legs).
func TestMCPCapability_UpgradeChangesNothingWhileOff(t *testing.T) {
	f := newCapFixture(t, cfgHTTPS, false, nil, "")
	if v, err := f.store.GetPlatformSetting("mcp_enabled"); err != nil || v != "" {
		t.Fatalf("fixture must have no mcp_enabled row: v=%q err=%v", v, err)
	}

	history := map[string]string{}
	for _, rt := range capRoutes() {
		rr := f.do(t, rt)
		if rt.gate == "history" {
			if rr.Code != http.StatusOK {
				t.Errorf("off: %s %s: status %d, want 200 (history is not gated)", rt.method, rt.path, rr.Code)
			}
			history[rt.path] = rr.Body.String()
			continue
		}
		if !isGateRefusal(rr) {
			t.Errorf("off: %s %s: status %d body %.200s, want the gate's 404", rt.method, rt.path, rr.Code, rr.Body.String())
		}
	}
	if res := f.dispatchClaim(t); !res.IsError {
		t.Errorf("off: the dispatcher's claim call succeeded; want it refused")
	}

	// Control: the same store and server, the setting turned on. The
	// setting is read per request, so no rebuild is involved.
	if err := f.store.SetPlatformSetting("mcp_enabled", "true"); err != nil {
		t.Fatalf("SetPlatformSetting: %v", err)
	}
	for _, rt := range capRoutes() {
		rr := f.do(t, rt)
		if isGateRefusal(rr) {
			t.Errorf("control: %s %s: still the gate's 404 with MCP on", rt.method, rt.path)
		}
		if rt.gate == "history" && rr.Body.String() != history[rt.path] {
			t.Errorf("control: %s %s: body changed with the setting:\noff: %s\non:  %s", rt.method, rt.path, history[rt.path], rr.Body.String())
		}
	}
	if res := f.dispatchClaim(t); res.IsError {
		t.Errorf("control: the dispatcher's claim call was refused with MCP on: %v", res.Content)
	}
}

// dispatchClaim redeems a claim code that is valid for the fixture's key
// through an HTTPHandlerDispatcher built the way wireMCP builds it: in
// process, over the server's own router, with an empty Host.
func (f *capFixture) dispatchClaim(t *testing.T) *mcpserver.CallToolResult {
	t.Helper()
	code := server.DeriveClaimCode(f.keyBytes, f.member.ID, f.ws.ID, time.Now())
	d := &mcpserver.HTTPHandlerDispatcher{
		Handler:      f.srv,
		UserResolver: func(context.Context) *models.User { return f.member },
	}
	ctx := mcpserver.WithDispatchInput(context.Background(), map[string]any{
		"workspace": f.ws.Slug,
		"code":      code,
	})
	res, err := d.Dispatch(ctx, []string{"workspace", "claim"}, nil)
	if err != nil {
		t.Fatalf("Dispatch(workspace claim): %v", err)
	}
	return res
}

// TestMCPOAuthAudience covers DR-4's construction cases: OAuth is built
// exactly when the resolved auth-server URL is https, for the audience the
// resolved MCP URL names. mcpOAuthAudience takes only config.MCPEndpoints,
// which is the proof that no runtime-writable source (a platform setting,
// the store) feeds the audience: there is nothing else it could read.
func TestMCPOAuthAudience(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Config
		want string
	}{
		{name: "PAD_URL https", cfg: config.Config{URL: "https://pad.example.com"}, want: "https://pad.example.com/mcp"},
		{name: "origin from PUBLIC_URL", cfg: config.Config{PublicURL: "https://pub.example.com"}, want: "https://pub.example.com/mcp"},
		{name: "PAD_AUTH_SERVER_URL https over an http origin", cfg: config.Config{URL: "http://pad.lan:7777", AuthServerURL: "https://auth.example.com"}, want: "http://pad.lan:7777/mcp"},
		{name: "http origin is PAT-only", cfg: config.Config{URL: "http://pad.lan:7777"}, want: ""},
		{name: "https origin with an http auth server is PAT-only", cfg: config.Config{URL: "https://pad.example.com", AuthServerURL: "http://auth.internal"}, want: ""},
		{name: "cloud's env", cfg: cfgCloud, want: "https://mcp.getpad.dev"},
		{name: "cloud's env, non-canonical PAD_MCP_PUBLIC_URL keeps its spelling", cfg: config.Config{PublicURL: "https://app.getpad.dev", MCPPublicURL: "https://MCP.GetPad.dev:443/", AuthServerURL: "https://app.getpad.dev"}, want: "https://MCP.GetPad.dev:443"},
		{name: "nothing configured", cfg: cfgNone, want: ""},
		{name: "an unusable origin", cfg: cfgBadURL, want: ""},
	}
	for _, tc := range cases {
		if got := mcpOAuthAudience(tc.cfg.ResolveMCPEndpoints()); got != tc.want {
			t.Errorf("%s: audience %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The origin may come from the toml `url` key, not only PAD_URL: the
// same field, loaded by config.Load from the data directory's
// config.toml.
func TestMCPOAuthAudience_OriginFromTomlURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PAD_DATA_DIR", dir)
	for _, v := range []string{"PAD_URL", "PUBLIC_URL", "PAD_MCP_PUBLIC_URL", "PAD_AUTH_SERVER_URL", "PAD_MODE", "PAD_CLOUD"} {
		t.Setenv(v, "")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("url = \"https://toml.example.com\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if got, want := mcpOAuthAudience(cfg.ResolveMCPEndpoints()), "https://toml.example.com/mcp"; got != want {
		t.Errorf("audience %q, want %q (cfg.URL=%q)", got, want, cfg.URL)
	}
}

// TestMCPCapability_HTTPSelfHostPATAndAudit: on an http self-host with MCP
// on, a PAT reaches the real transport through /mcp, and the call is
// recorded in mcp_audit_log, which proves the audit writer wireMCP starts
// runs off cloud (DR-4).
func TestMCPCapability_HTTPSelfHostPATAndAudit(t *testing.T) {
	f := newCapFixture(t, cfgHTTP, false, nil, "true")
	rr := f.do(t, capRoute{
		method: "POST", path: "/mcp", auth: "member", contentType: "application/json",
		body: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"cap-test","version":"0"}}}`,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("PAT initialize over http: status %d body %.300s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"serverInfo"`) {
		t.Fatalf("initialize did not reach the MCP server: %.300s", rr.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := f.store.ListMCPAuditByUser(f.member.ID, 10, 0)
		if err != nil {
			t.Fatalf("ListMCPAuditByUser: %v", err)
		}
		if len(rows) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no mcp_audit_log row for the PAT call within 5s: the audit writer is not running off cloud")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
