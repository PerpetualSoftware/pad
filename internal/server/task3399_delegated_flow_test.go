package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3399 (SPEC-6 U5b-1): a person signs in to an installed app with
// authorization code + PKCE, on a consent page fixed to the install.

type delegatedFix struct {
	srv          *Server
	in           testInstall
	person       *models.User
	sessionToken string
	csrf         string
}

func delegatedFixture(t *testing.T, installID, offered string) delegatedFix {
	t.Helper()
	srv := appOAuthServer(t, true)
	in := newTestInstall(t, srv, installID)
	if offered != "" {
		if _, err := srv.store.DB().Exec(`UPDATE app_installs SET delegated_access = ? WHERE id = ?`, offered, installID); err != nil {
			t.Fatal(err)
		}
	}
	person, sessionToken := loginTestUserAs(t, srv, "person-"+installID+"@example.com", "Dana", "password123")
	if err := srv.store.AddWorkspaceMember(in.wsID, person.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	return delegatedFix{srv: srv, in: in, person: person, sessionToken: sessionToken, csrf: readCSRFFromCookie(t, srv, sessionToken)}
}

const delegatedVerifier = "verifier-3399-abcdefghijklmnopqrstuvwxyz0123456789ABCDEF"

func (f delegatedFix) authorizeParams() url.Values {
	return url.Values{
		"client_id":             {f.in.clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://portal.example/cb"},
		"code_challenge":        {s256Challenge(delegatedVerifier)},
		"code_challenge_method": {"S256"},
		"state":                 {"state-3399-0001"},
		"resource":              {testAppAPIAudience},
	}
}

func (f delegatedFix) decide(t *testing.T, params url.Values, decision, access string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	for k, v := range params {
		form[k] = v
	}
	form.Set("decision", decision)
	form.Set("csrf_token", f.csrf)
	if access != "" {
		form.Set("app_access", access)
	}
	return postFormWithCookie(f.srv, "/oauth/authorize/decide", form, f.sessionToken, f.csrf)
}

// codeFrom returns the code a decide redirect carries, or "" with the
// redirect's error.
func codeFrom(rr *httptest.ResponseRecorder) (code, oauthErr string) {
	cb, _ := url.Parse(rr.Header().Get("Location"))
	if cb == nil {
		return "", ""
	}
	return cb.Query().Get("code"), cb.Query().Get("error")
}

func (f delegatedFix) exchange(code string) *httptest.ResponseRecorder {
	return postTokenBasic(f.srv, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"https://portal.example/cb"},
		"code_verifier": {delegatedVerifier}, "resource": {testAppAPIAudience},
	}, f.in.clientID, f.in.secret)
}

func TestTask3399_TheConsentPageIsFixedToTheInstall(t *testing.T) {
	f := delegatedFixture(t, "inst-page", "write")
	q := f.authorizeParams()
	rr := doAuthedRequest(f.srv, "GET", "/oauth/authorize?"+q.Encode(), nil, f.sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("consent page: %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"WS inst-page", `value="read" checked`, `value="write"`, "Portal"} {
		if !strings.Contains(body, want) {
			t.Errorf("consent page lacks %q", want)
		}
	}
	for _, never := range []string{"allowed_workspaces", "may_create_workspaces", "capability_tier", "workspace_access"} {
		if strings.Contains(body, never) {
			t.Errorf("consent page offers %q: the workspace is fixed to the install", never)
		}
	}
	// A manifest that offers read only renders no write choice.
	g := delegatedFixture(t, "inst-page-ro", "read")
	q = g.authorizeParams()
	if rr := doAuthedRequest(g.srv, "GET", "/oauth/authorize?"+q.Encode(), nil, g.sessionToken); strings.Contains(rr.Body.String(), `value="write"`) {
		t.Error("a read-only manifest offered write")
	}
}

func TestTask3399_ADelegatedSignInIssuesAPersonsToken(t *testing.T) {
	f := delegatedFixture(t, "inst-flow", "write")
	connsBefore := count3399(t, f.srv, `SELECT COUNT(*) FROM oauth_connections`)
	rr := f.decide(t, f.authorizeParams(), "approve", "") // no choice: read-only
	code, oerr := codeFrom(rr)
	if code == "" {
		t.Fatalf("decide: %d, error %q", rr.Code, oerr)
	}
	trr := f.exchange(code)
	if trr.Code != http.StatusOK {
		t.Fatalf("exchange: %d %s", trr.Code, trr.Body.String())
	}
	var resp map[string]any
	parseJSON(t, trr, &resp)
	tok, _ := resp["access_token"].(string)
	g, err := f.srv.introspectAppToken(context.Background(), tok)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if g.AuthKind != "delegated" || g.Subject != f.person.ID || g.InstallID != f.in.id || g.WorkspaceID != f.in.wsID {
		t.Errorf("grant = %+v, want a delegated grant for the person", g)
	}
	var access string
	if err := f.srv.store.DB().QueryRow(`SELECT delegated_access FROM app_token_bindings WHERE request_id = ?`, g.RequestID).Scan(&access); err != nil {
		t.Fatal(err)
	}
	if access != "read" {
		t.Errorf("consented access = %q, want the read-only default", access)
	}
	if n := count3399(t, f.srv, `SELECT COUNT(*) FROM oauth_connections`); n != connsBefore {
		t.Errorf("a delegated sign-in wrote %d oauth_connections rows (lead ruling Q1: none)", n-connsBefore)
	}
	// The app refreshes its own grant.
	refresh, _ := resp["refresh_token"].(string)
	if rr := postTokenBasic(f.srv, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "resource": {testAppAPIAudience}},
		f.in.clientID, f.in.secret); rr.Code != http.StatusOK {
		t.Errorf("refresh: %d %s", rr.Code, rr.Body.String())
	}
}

func TestTask3399_WriteIsChosenOnlyWhenOffered(t *testing.T) {
	f := delegatedFixture(t, "inst-w", "write")
	code, _ := codeFrom(f.decide(t, f.authorizeParams(), "approve", "write"))
	if code == "" || f.exchange(code).Code != http.StatusOK {
		t.Fatal("a write choice on a write manifest was refused")
	}
	g := delegatedFixture(t, "inst-ro", "read")
	if rr := g.decide(t, g.authorizeParams(), "approve", "write"); rr.Code != http.StatusBadRequest {
		t.Errorf("write on a read-only manifest: %d, want 400", rr.Code)
	}
}

// The lead's required race: a disable or rotate between the consent
// decision (the code is minted) and the token exchange refuses the exchange.
func TestTask3399_ADisableBetweenAuthorizeAndExchangeRefusesTheToken(t *testing.T) {
	for name, q := range map[string]string{
		"disable": `UPDATE app_installs SET state = 'disabling', auth_epoch = auth_epoch + 1 WHERE id = ?`,
		"rotate":  `UPDATE app_installs SET auth_epoch = auth_epoch + 1 WHERE id = ?`,
	} {
		t.Run(name, func(t *testing.T) {
			f := delegatedFixture(t, "inst-race-"+name, "write")
			code, _ := codeFrom(f.decide(t, f.authorizeParams(), "approve", "read"))
			if code == "" {
				t.Fatal("no code")
			}
			if _, err := f.srv.store.DB().Exec(q, f.in.id); err != nil {
				t.Fatal(err)
			}
			rr := f.exchange(code)
			if rr.Code == http.StatusOK || strings.Contains(rr.Body.String(), "access_token") {
				t.Errorf("exchange after a %s: %d %s, want a refusal", name, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestTask3399_SignInRefusals(t *testing.T) {
	// A non-member is refused, with no code.
	f := delegatedFixture(t, "inst-nonmember", "write")
	if _, err := f.srv.store.DB().Exec(`DELETE FROM workspace_members WHERE user_id = ? AND workspace_id = ?`, f.person.ID, f.in.wsID); err != nil {
		t.Fatal(err)
	}
	if code, oerr := codeFrom(f.decide(t, f.authorizeParams(), "approve", "read")); code != "" || oerr != "access_denied" {
		t.Errorf("a non-member: code %q, error %q, want access_denied", code, oerr)
	}
	// An install offering no delegated access.
	g := delegatedFixture(t, "inst-nodeleg", "")
	if code, oerr := codeFrom(g.decide(t, g.authorizeParams(), "approve", "read")); code != "" || oerr != "access_denied" {
		t.Errorf("no delegated access offered: code %q, error %q, want access_denied", code, oerr)
	}
	// No PKCE.
	h := delegatedFixture(t, "inst-nopkce", "write")
	p := h.authorizeParams()
	p.Del("code_challenge")
	p.Del("code_challenge_method")
	if code, _ := codeFrom(h.decide(t, p, "approve", "read")); code != "" {
		t.Error("a sign-in without PKCE minted a code")
	}
	// The MCP resource, not the app API's.
	k := delegatedFixture(t, "inst-mcpres", "write")
	p = k.authorizeParams()
	p.Set("resource", testCanonicalAudience)
	if code, _ := codeFrom(k.decide(t, p, "approve", "read")); code != "" {
		t.Error("a sign-in for the MCP resource minted a code")
	}
	// A disabled install.
	m := delegatedFixture(t, "inst-inactive", "write")
	if _, err := m.srv.store.DB().Exec(`UPDATE app_installs SET state = 'inactive' WHERE id = ?`, m.in.id); err != nil {
		t.Fatal(err)
	}
	if code, oerr := codeFrom(m.decide(t, m.authorizeParams(), "approve", "read")); code != "" || oerr != "access_denied" {
		t.Errorf("an inactive install: code %q, error %q", code, oerr)
	}
}

func count3399(t *testing.T, srv *Server, q string, args ...any) int {
	t.Helper()
	var n int
	if err := srv.store.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// signIn runs a whole delegated sign-in and returns the access token and the
// grant's request id.
func (f delegatedFix) signIn(t *testing.T, access string) (string, string) {
	t.Helper()
	code, oerr := codeFrom(f.decide(t, f.authorizeParams(), "approve", access))
	if code == "" {
		t.Fatalf("sign-in: no code (%s)", oerr)
	}
	rr := f.exchange(code)
	if rr.Code != http.StatusOK {
		t.Fatalf("exchange: %d %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	parseJSON(t, rr, &resp)
	tok, _ := resp["access_token"].(string)
	g, err := f.srv.introspectAppToken(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	return tok, g.RequestID
}

// Lead ruling Q1: the console lists an app grant on its own, read-only plus
// revoke; every connection mutation refuses it and changes nothing.
func TestTask3399_TheConsoleShowsAndRevokesAppGrants(t *testing.T) {
	f := delegatedFixture(t, "inst-console", "write")
	tok, reqID := f.signIn(t, "write")

	rr := doAuthedJSON(f.srv, "GET", "/api/v1/connected-apps", nil, f.sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	var list struct {
		Items     []map[string]any `json:"items"`
		AppGrants []map[string]any `json:"app_grants"`
	}
	parseJSON(t, rr, &list)
	for _, it := range list.Items {
		if it["id"] == reqID || it["request_id"] == reqID {
			t.Errorf("the app grant is listed as an MCP connection: %v", it)
		}
	}
	if len(list.AppGrants) != 1 || list.AppGrants[0]["id"] != reqID || list.AppGrants[0]["access"] != "write" ||
		list.AppGrants[0]["app_name"] != "Portal" {
		t.Fatalf("app_grants = %v", list.AppGrants)
	}

	// Read-only: every connection mutation refuses the app grant.
	for _, m := range []struct {
		method, path string
		body         any
	}{
		{"PATCH", "/api/v1/connected-apps/" + reqID + "/name", map[string]any{"name": "x"}},
		{"PATCH", "/api/v1/connected-apps/" + reqID + "/flags", map[string]any{"may_create_workspaces": true}},
		{"POST", "/api/v1/connected-apps/" + reqID + "/limit-to-current", nil},
		{"POST", "/api/v1/connected-apps/" + reqID + "/workspaces", map[string]any{"slug": "anything"}},
		{"DELETE", "/api/v1/connected-apps/" + reqID + "/workspaces/anything", nil},
	} {
		if rr := doAuthedJSON(f.srv, m.method, m.path, m.body, f.sessionToken); rr.Code < 400 {
			t.Errorf("%s %s on an app grant: %d, want a refusal", m.method, m.path, rr.Code)
		}
	}
	if n := count3399(t, f.srv, `SELECT COUNT(*) FROM oauth_connections WHERE request_id = ?`, reqID); n != 0 {
		t.Error("a mutation wrote a connection row for the app grant")
	}
	if _, err := f.srv.introspectAppToken(context.Background(), tok); err != nil {
		t.Fatalf("control: the grant died before the revoke: %v", err)
	}

	// Revoke: the token stops at introspection.
	if rr := doAuthedJSON(f.srv, "DELETE", "/api/v1/connected-apps/"+reqID, nil, f.sessionToken); rr.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := f.srv.introspectAppToken(context.Background(), tok); err == nil {
		t.Error("a revoked app grant's token still introspects")
	}
	// Someone else cannot revoke it, and the answer is the plain 404.
	g := delegatedFixture(t, "inst-console2", "write")
	_, other := g.signIn(t, "read")
	_, stranger := loginTestUserAs(t, g.srv, "stranger@example.com", "Stranger", "password123")
	if rr := doAuthedJSON(g.srv, "DELETE", "/api/v1/connected-apps/"+other, nil, stranger); rr.Code != http.StatusNotFound {
		t.Errorf("a stranger's revoke: %d, want 404", rr.Code)
	}
}
