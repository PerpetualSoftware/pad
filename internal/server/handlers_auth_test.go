package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/config"
	"github.com/PerpetualSoftware/pad/internal/models"
)

func bootstrapFirstUser(t *testing.T, srv *Server, email, name string) string {
	t.Helper()

	rr := doLoopbackRequest(srv, "POST", "/api/v1/auth/bootstrap", map[string]string{
		"email":    email,
		"name":     name,
		"password": "correct-horse-battery-staple",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("bootstrap: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	parseJSON(t, rr, &resp)
	token, _ := resp["token"].(string)
	if token == "" {
		t.Fatal("expected bootstrap to return a session token")
	}
	return token
}

func TestAuthBootstrapFlow(t *testing.T) {
	srv := testServer(t)

	rr := doRequest(srv, "GET", "/api/v1/auth/session", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("session check: expected 200, got %d", rr.Code)
	}

	var session map[string]interface{}
	parseJSON(t, rr, &session)
	if session["setup_required"] != true {
		t.Error("expected setup_required=true when no users exist")
	}
	if session["setup_method"] != "local_cli" {
		t.Errorf("expected setup_method=local_cli, got %v", session["setup_method"])
	}
	if _, ok := session["needs_setup"]; ok {
		t.Error("did not expect deprecated needs_setup field")
	}
	// mcp_public_url is always present (even pre-setup) so the web UI can
	// render the right onboarding flow before the first admin exists. Empty
	// while MCP is not available (the default for this test server).
	if got, ok := session["mcp_public_url"]; !ok {
		t.Error("expected mcp_public_url field in setup-state session payload")
	} else if got != "" {
		t.Errorf("expected mcp_public_url='' while MCP is not available, got %v", got)
	}

	token := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr = doRequestWithCookie(srv, "GET", "/api/v1/auth/session", nil, token)
	parseJSON(t, rr, &session)
	if session["authenticated"] != true {
		t.Error("expected authenticated=true after bootstrap")
	}
	authUser, ok := session["user"].(map[string]interface{})
	if !ok || authUser == nil {
		t.Error("expected authenticated session user payload")
	} else {
		if authUser["email"] != "admin@test.com" {
			t.Errorf("expected email admin@test.com, got %v", authUser["email"])
		}
		if authUser["role"] != "admin" {
			t.Errorf("expected admin role after bootstrap, got %v", authUser["role"])
		}
	}
	if session["setup_required"] != false {
		t.Errorf("expected setup_required=false after bootstrap, got %v", session["setup_required"])
	}
	if got, ok := session["mcp_public_url"]; !ok {
		t.Error("expected mcp_public_url field in authenticated session payload")
	} else if got != "" {
		t.Errorf("expected mcp_public_url='' while MCP is not available, got %v", got)
	}
}

// The session and setup payloads carry the MCP URL and its auth methods
// exactly when MCP is available (PLAN-2310 DR-8): the resolved URL with
// ["oauth","pat"] where OAuth is served, ["pat"] on an http self-host, and
// "" with [] otherwise. The connect modal keys on both, so each state is
// driven through the router, on the setup payload and the session payload.
func TestAuthSessionMCPURLAndAuth(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name     string
		cloud    bool
		origin   string
		env      *bool
		oauth    bool
		wantURL  string
		wantAuth []string
	}{
		{name: "self-host off, https origin, OAuth built", origin: "https://pad.example.com", env: nil, oauth: true, wantURL: "", wantAuth: []string{}},
		{name: "self-host forced off", origin: "https://pad.example.com", env: &off, oauth: true, wantURL: "", wantAuth: []string{}},
		{name: "self-host on, no origin", origin: "", env: &on, wantURL: "", wantAuth: []string{}},
		{name: "self-host on, unusable origin", origin: "pad.example.com", env: &on, wantURL: "", wantAuth: []string{}},
		{name: "self-host on, http origin", origin: "http://pad.lan:7777", env: &on, wantURL: "http://pad.lan:7777/mcp", wantAuth: []string{"pat"}},
		{name: "self-host on, https origin, OAuth built", origin: "https://pad.example.com", env: &on, oauth: true, wantURL: "https://pad.example.com/mcp", wantAuth: []string{"oauth", "pat"}},
		// An https origin whose OAuth server was not built serves PATs only:
		// the claim routes and Connected Apps answer 404 there.
		{name: "self-host on, https origin, no OAuth server", origin: "https://pad.example.com", env: &on, wantURL: "https://pad.example.com/mcp", wantAuth: []string{"pat"}},
		{name: "cloud", cloud: true, oauth: true, wantURL: "https://mcp.test.example", wantAuth: []string{"oauth", "pat"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer(t)
			cfg := config.Config{URL: tc.origin}
			ep := cfg.ResolveMCPEndpoints()
			srv.SetMCPConfig(ep, tc.env)
			// Same-package access to the unexported field: what
			// SetMCPTransport sets at startup, without the audit writer.
			srv.mcpPublicURL = ep.ResourceURL
			if tc.cloud {
				srv.SetCloudMode("test-secret")
				srv.mcpPublicURL = "https://mcp.test.example"
			}
			if tc.oauth {
				o, err := newTestOAuthServer(t, srv)
				if err != nil {
					t.Fatalf("oauth.NewServer: %v", err)
				}
				srv.SetOAuthServer(o)
			}

			check := func(which string, rr *httptest.ResponseRecorder) {
				t.Helper()
				if rr.Code != http.StatusOK {
					t.Fatalf("%s: status %d", which, rr.Code)
				}
				var session map[string]interface{}
				parseJSON(t, rr, &session)
				if got, ok := session["mcp_public_url"]; !ok || got != tc.wantURL {
					t.Errorf("%s: mcp_public_url = %v (present %v), want %q", which, got, ok, tc.wantURL)
				}
				raw, ok := session["mcp_auth"].([]interface{})
				if !ok {
					t.Fatalf("%s: mcp_auth = %#v, want an array", which, session["mcp_auth"])
				}
				got := make([]string, len(raw))
				for i, v := range raw {
					got[i], _ = v.(string)
				}
				if strings.Join(got, ",") != strings.Join(tc.wantAuth, ",") {
					t.Errorf("%s: mcp_auth = %v, want %v", which, got, tc.wantAuth)
				}
			}

			// No users yet: the setup payload.
			check("setup", doRequest(srv, "GET", "/api/v1/auth/session", nil))
			token := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
			check("session", doRequestWithCookie(srv, "GET", "/api/v1/auth/session", nil, token))
		})
	}
}

// billing_available is false by default and only true when BOTH cloudMode AND
// billingAvailable are set. Tests for both the default (off) and the enabled
// state so a future refactor can't accidentally always-expose the CTA. TASK-800.
func TestAuthSessionBillingAvailable(t *testing.T) {
	// Default: both flags off — billing_available must be false.
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/auth/session", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("session check: expected 200, got %d", rr.Code)
	}
	var session map[string]interface{}
	parseJSON(t, rr, &session)
	if v, ok := session["billing_available"]; !ok {
		t.Error("billing_available field missing from session payload")
	} else if v != false {
		t.Errorf("expected billing_available=false by default, got %v", v)
	}

	// Cloud mode only (no billingAvailable) — still false.
	srv2 := testServer(t)
	srv2.cloudMode = true
	rr2 := doRequest(srv2, "GET", "/api/v1/auth/session", nil)
	parseJSON(t, rr2, &session)
	if session["billing_available"] != false {
		t.Errorf("expected billing_available=false when cloudMode=true but billingAvailable=false, got %v", session["billing_available"])
	}

	// Both flags set — billing_available must be true.
	srv3 := testServer(t)
	srv3.cloudMode = true
	srv3.billingAvailable = true
	rr3 := doRequest(srv3, "GET", "/api/v1/auth/session", nil)
	parseJSON(t, rr3, &session)
	if session["billing_available"] != true {
		t.Errorf("expected billing_available=true when cloudMode+billingAvailable both set, got %v", session["billing_available"])
	}
}

// version is surfaced on /auth/session (IDEA-1826 / TASK-1839) so the mobile
// shells can read the server build version in the call they already make on
// connect and warn when it's below their minimum. It must appear in BOTH the
// pre-setup payload (no users yet — the shell validates fresh servers too) and
// the post-setup authenticated/unauthenticated payload.
func TestAuthSessionEmitsVersion(t *testing.T) {
	srv := testServer(t)
	srv.version = "1.2.3"

	// Pre-setup (no users): setupStatePayload must carry version.
	rr := doRequest(srv, "GET", "/api/v1/auth/session", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("session check: expected 200, got %d", rr.Code)
	}
	var session map[string]interface{}
	parseJSON(t, rr, &session)
	if session["setup_required"] != true {
		t.Fatal("expected setup_required=true when no users exist")
	}
	if got := session["version"]; got != "1.2.3" {
		t.Errorf("expected version=1.2.3 in setup-state payload, got %v", got)
	}

	// Post-setup, authenticated: sessionStatePayload must carry version too.
	token := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	rr = doRequestWithCookie(srv, "GET", "/api/v1/auth/session", nil, token)
	parseJSON(t, rr, &session)
	if session["authenticated"] != true {
		t.Fatal("expected authenticated=true after bootstrap")
	}
	if got := session["version"]; got != "1.2.3" {
		t.Errorf("expected version=1.2.3 in authenticated payload, got %v", got)
	}
}

func TestAuthBootstrapRequiresLoopback(t *testing.T) {
	srv := testServer(t)

	rr := doRequest(srv, "POST", "/api/v1/auth/bootstrap", map[string]string{
		"email":    "admin@test.com",
		"name":     "Admin",
		"password": "correct-horse-battery-staple",
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("remote bootstrap: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAuthLoginFlow(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "user@test.com", "Test User")

	rr := doRequest(srv, "POST", "/api/v1/auth/login", map[string]string{
		"email":    "user@test.com",
		"password": "correct-horse-battery-staple",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("login: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var loginResp map[string]interface{}
	parseJSON(t, rr, &loginResp)
	user := loginResp["user"].(map[string]interface{})
	if user["name"] != "Test User" {
		t.Errorf("expected name 'Test User', got %v", user["name"])
	}

	rr = doRequest(srv, "POST", "/api/v1/auth/login", map[string]string{
		"email":    "user@test.com",
		"password": "wrongpassword",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: expected 401, got %d", rr.Code)
	}

	rr = doRequest(srv, "POST", "/api/v1/auth/login", map[string]string{
		"email":    "nobody@test.com",
		"password": "anything",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("non-existent email: expected 401, got %d", rr.Code)
	}
}

func TestAuthLoginRequiresSetupWhenNoUsers(t *testing.T) {
	srv := testServer(t)

	rr := doRequest(srv, "POST", "/api/v1/auth/login", map[string]string{
		"email":    "nobody@test.com",
		"password": "correct-horse-battery-staple",
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("login without users: expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestFreshInstanceRegistrationIsForbidden(t *testing.T) {
	srv := testServer(t)

	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "admin@test.com",
		"name":     "Admin",
		"password": "correct-horse-battery-staple",
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("fresh registration: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestInvitationRegistrationFlow(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Test"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	admin, err := srv.store.GetUserByEmail("admin@test.com")
	if err != nil || admin == nil {
		t.Fatalf("load admin user: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, admin.ID, "owner"); err != nil {
		t.Fatalf("add admin workspace membership: %v", err)
	}
	inv, err := srv.store.CreateInvitation(ws.ID, "invitee@test.com", "viewer", admin.ID)
	if err != nil {
		t.Fatalf("create invitation: %v", err)
	}

	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":           "invitee@test.com",
		"name":            "Invitee",
		"password":        "correct-horse-battery-staple",
		"invitation_code": inv.Code,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("invitation registration: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	parseJSON(t, rr, &resp)
	user := resp["user"].(map[string]interface{})
	if user["role"] != "member" {
		t.Errorf("expected invitation signup to create member role, got %v", user["role"])
	}
	// BUG-3284: the signup names the workspace it joined, so the client can
	// land in it.
	accepted, ok := resp["accepted_invitation"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected accepted_invitation on an invitation signup, got %v", resp["accepted_invitation"])
	}
	if accepted["workspace_slug"] != ws.Slug || accepted["workspace_id"] != ws.ID || accepted["role"] != "viewer" {
		t.Errorf("accepted_invitation = %v, want slug %q, id %q, role viewer", accepted, ws.Slug, ws.ID)
	}

	invitee, err := srv.store.GetUserByEmail("invitee@test.com")
	if err != nil || invitee == nil {
		t.Fatalf("load invitee: %v", err)
	}
	member, err := srv.store.GetWorkspaceMember(ws.ID, invitee.ID)
	if err != nil {
		t.Fatalf("get workspace member: %v", err)
	}
	if member == nil || member.Role != "viewer" {
		t.Fatalf("expected invitee to be added to workspace as viewer, got %#v", member)
	}

	rr = doRequestWithCookie(srv, "POST", "/api/v1/auth/logout", nil, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("logout admin: expected 200, got %d", rr.Code)
	}
}

func TestAuthRegistrationValidation(t *testing.T) {
	srv := testServer(t)

	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"name":     "Test",
		"password": "correct-horse-battery-staple",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("missing email: expected 400, got %d", rr.Code)
	}

	rr = doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "not-an-email",
		"name":     "Test",
		"password": "correct-horse-battery-staple",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("invalid email: expected 400, got %d", rr.Code)
	}

	rr = doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "test@test.com",
		"name":     "Test",
		"password": "short",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("short password: expected 400, got %d", rr.Code)
	}

	rr = doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "test@test.com",
		"password": "correct-horse-battery-staple",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("missing name: expected 400, got %d", rr.Code)
	}
}

func TestAuthLogout(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "user@test.com", "Test")

	rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/logout", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("logout: expected 200, got %d", rr.Code)
	}

	rr = doRequestWithCookie(srv, "GET", "/api/v1/auth/session", nil, token)
	var session map[string]interface{}
	parseJSON(t, rr, &session)
	if session["authenticated"] != false {
		t.Error("expected authenticated=false after logout")
	}
}

func TestAuthRequiredWhenUsersExist(t *testing.T) {
	srv := testServer(t)

	rr := doRequest(srv, "GET", "/api/v1/workspaces", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("no users: expected 200, got %d", rr.Code)
	}

	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr = doRequest(srv, "GET", "/api/v1/workspaces", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("with users: expected 401, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(srv, "GET", "/api/v1/auth/session", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("auth/session should be exempt: expected 200, got %d", rr.Code)
	}

	rr = doRequest(srv, "GET", "/api/v1/health", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("health should be exempt: expected 200, got %d", rr.Code)
	}
}

func TestAuthMeEndpoint(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "me@test.com", "Me Test")

	rr := doRequestWithCookie(srv, "GET", "/api/v1/auth/me", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("me: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var user map[string]interface{}
	parseJSON(t, rr, &user)
	if user["name"] != "Me Test" {
		t.Errorf("expected name 'Me Test', got %v", user["name"])
	}
	if user["email"] != "me@test.com" {
		t.Errorf("expected email 'me@test.com', got %v", user["email"])
	}
	// A bootstrapped user set a password, so password_set must be true. The
	// delete-account UI keys off this to show a password prompt vs a
	// confirm-only flow (TASK-1957).
	if set, ok := user["password_set"].(bool); !ok || !set {
		t.Errorf("expected password_set=true for a password user, got %v", user["password_set"])
	}
}

// TestAuthMeReportsPasswordSetForOAuthUser pins the OAuth-only branch of
// TASK-1957: a user created via OAuth (no password) must report
// password_set=false so the client can offer confirm-only account deletion.
func TestAuthMeReportsPasswordSetForOAuthUser(t *testing.T) {
	srv := testServer(t)
	// Bootstrap a first user so the instance is initialized and auth is enforced.
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	oauthUser, err := srv.store.CreateOAuthUser("oauth@test.com", "OAuth Only", "")
	if err != nil {
		t.Fatalf("CreateOAuthUser: %v", err)
	}
	if oauthUser.HasPassword() {
		t.Fatal("expected freshly created OAuth user to have no password")
	}
	token, err := srv.store.CreateSession(oauthUser.ID, "go-test", "192.0.2.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	rr := doRequestWithCookie(srv, "GET", "/api/v1/auth/me", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("me: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var user map[string]interface{}
	parseJSON(t, rr, &user)
	if set, ok := user["password_set"].(bool); !ok || set {
		t.Errorf("expected password_set=false for an OAuth-only user, got %v", user["password_set"])
	}
}

func TestDuplicateRegistration(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "dup@test.com",
		"name":     "First",
		"password": "correct-horse-battery-staple",
	}, adminToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("admin register: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequestWithCookie(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "dup@test.com",
		"name":     "Second",
		"password": "password456",
	}, adminToken)
	if rr.Code == http.StatusCreated {
		t.Error("duplicate registration should not succeed")
	}
}

// doRequestWithCookie is like doRequest but adds a session cookie.
func doRequestWithCookie(srv *Server, method, path string, body interface{}, token string) *httptest.ResponseRecorder {
	var bodyReader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{
		Name:  "pad_session",
		Value: token,
	})
	// Include CSRF token for the double-submit cookie pattern
	// Must be csrfTokenLen*2 hex chars to pass the length check added
	// in TASK-659. Any fixed 64-char hex string works for the test.
	const testCSRF = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req.AddCookie(&http.Cookie{
		Name:  "pad_csrf",
		Value: testCSRF,
	})
	req.Header.Set("X-CSRF-Token", testCSRF)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}
