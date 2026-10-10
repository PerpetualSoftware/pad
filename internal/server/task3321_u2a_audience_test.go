package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/oauth"
)

// TASK-3321 U2a (ruling (i)): the authorization server issues tokens for two
// canonical resources, the /mcp URL and the ChatGPT catalog's, but every
// token is bound to ONE, and each mount refuses a token bound to the other.
// A request naming no resource is bound to /mcp's, as it always was.

const testChatGPTResource = testCanonicalAudience + "/mcp/chatgpt"

func twoResourceOAuthServer(t *testing.T) *Server {
	t.Helper()
	srv := testServer(t)
	srv.SetCloudMode("test-secret")
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	srv.SetMCPTransport(stub, testCanonicalAudience, testAuthServerURL, nil)
	o, err := oauth.NewServer(oauth.Config{
		Store:               srv.store,
		HMACSecret:          bytes32ForTest(),
		AllowedAudience:     testCanonicalAudience,
		AdditionalAudiences: []string{testChatGPTResource},
	})
	if err != nil {
		t.Fatalf("oauth.NewServer: %v", err)
	}
	srv.SetOAuthServer(o)
	return srv
}

// mintWithResource runs the authorization-code flow with resource= set to
// resource on BOTH the decide and the token request, or with no resource
// or audience at all when resource is "". It returns the access token, or
// the status of the first refused step.
type oauthSession struct{ sessionToken, csrfTok, clientID string }

func newOAuthSession(t *testing.T, srv *Server) oauthSession {
	t.Helper()
	_, sessionToken := loginTestUser(t, srv)
	return oauthSession{
		sessionToken: sessionToken,
		csrfTok:      readCSRFFromCookie(t, srv, sessionToken),
		clientID:     registerTestClient(t, srv, "https://app.test/cb"),
	}
}

var mintCounter int

func mintWithResource(t *testing.T, srv *Server, sess oauthSession, resource string) (string, int) {
	t.Helper()
	return mintWithResourceTier(t, srv, sess, resource, "pad:read", "read")
}

// mintWithResourceTier is mintWithResource asking for scope and consenting
// to tier (read / write / admin).
func mintWithResourceTier(t *testing.T, srv *Server, sess oauthSession, resource, scope, tier string) (string, int) {
	t.Helper()
	mintCounter++
	tag := string(rune('a' + mintCounter%26))
	sessionToken, csrfTok, clientID := sess.sessionToken, sess.csrfTok, sess.clientID
	verifier := "verifier-u2a-" + tag + "-abcdefghijklmnopqrstuvwxyz0123456789ABCDEF"
	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"code_challenge":        {s256Challenge(verifier)},
		"code_challenge_method": {"S256"},
		"scope":                 {scope},
		"state":                 {"state-u2a-" + tag + "-01"},
		"decision":              {"approve"},
		"csrf_token":            {csrfTok},
		"capability_tier":       {tier},
		"allowed_workspaces":    {"*"},
	}
	if resource != "" {
		form.Set("resource", resource)
	}
	rr := postFormWithCookie(srv, "/oauth/authorize/decide", form, sessionToken, csrfTok)
	if rr.Code != http.StatusSeeOther && rr.Code != http.StatusFound {
		return "", rr.Code
	}
	cb, _ := url.Parse(rr.Header().Get("Location"))
	code := cb.Query().Get("code")
	if code == "" {
		return "", http.StatusBadRequest
	}
	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {"https://app.test/cb"},
		"code_verifier": {verifier},
	}
	if resource != "" {
		tokenForm.Set("resource", resource)
	}
	trr := postOAuthForm(srv, "/oauth/token", tokenForm)
	if trr.Code != http.StatusOK {
		return "", trr.Code
	}
	var resp map[string]any
	parseJSON(t, trr, &resp)
	tok, _ := resp["access_token"].(string)
	lastRefresh, _ = resp["refresh_token"].(string)
	lastClientID = clientID
	return tok, http.StatusOK
}

// lastRefresh and lastClientID are the most recent mint's refresh token and
// client, for the refresh leg.
var lastRefresh, lastClientID string

// A refreshed token keeps its ONE resource: refreshing a ChatGPT token (even
// asking for the /mcp resource) yields a token still refused at /mcp.
func TestU2a_RefreshKeepsTheBinding(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	sess := newOAuthSession(t, srv)
	if _, code := mintWithResource(t, srv, sess, testChatGPTResource); code != http.StatusOK {
		t.Fatalf("mint: %d", code)
	}
	refreshed := 0
	for _, ask := range []string{"", testCanonicalAudience} {
		form := url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {lastRefresh},
			"client_id":     {lastClientID},
		}
		if ask != "" {
			form.Set("resource", ask)
		}
		rr := postOAuthForm(srv, "/oauth/token", form)
		if rr.Code != http.StatusOK {
			// Refusing to widen is also a pass; it must not be a /mcp token.
			continue
		}
		var resp map[string]any
		parseJSON(t, rr, &resp)
		tok, _ := resp["access_token"].(string)
		lastRefresh, _ = resp["refresh_token"].(string)
		refreshed++
		if got := atMount(srv, "", tok); got != http.StatusUnauthorized {
			t.Errorf("refresh (resource=%q) gave a token accepted at /mcp: %d", ask, got)
		}
		if got := atMount(srv, testChatGPTResource, tok); got != http.StatusOK {
			t.Errorf("refresh (resource=%q) gave a token refused at its own mount: %d", ask, got)
		}
	}
	if refreshed == 0 {
		t.Fatal("no refresh succeeded, so this test measured nothing")
	}
}

// atMount presents token to an MCP mount: /mcp (no resource stamped) or the
// ChatGPT catalog's (its resource stamped, as its route will).
func atMount(srv *Server, resource, token string) int {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := srv.MCPBearerAuth(ok)
	if resource != "" {
		h = WithMCPResource(resource)(h)
	}
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

func TestU2a_TokensAreBoundToOneResourceAndRefusedAtTheOther(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	sess := newOAuthSession(t, srv)

	mcpTok, code := mintWithResource(t, srv, sess, testCanonicalAudience)
	if code != http.StatusOK {
		t.Fatalf("minting a /mcp token: %d", code)
	}
	gptTok, code := mintWithResource(t, srv, sess, testChatGPTResource)
	if code != http.StatusOK {
		t.Fatalf("minting a ChatGPT token: %d", code)
	}
	// No resource on the request: refused since TASK-3363 phase 2, so no
	// token exists to be bound to a default.
	if _, code := mintWithResource(t, srv, sess, ""); code == http.StatusOK {
		t.Fatalf("minting a no-resource token succeeded; TASK-3363 phase 2 refuses it")
	}

	cases := []struct {
		name     string
		token    string
		resource string
		want     int
	}{
		{"/mcp token at /mcp", mcpTok, "", http.StatusOK},
		{"/mcp token at the ChatGPT mount: refused", mcpTok, testChatGPTResource, http.StatusUnauthorized},
		{"ChatGPT token at the ChatGPT mount", gptTok, testChatGPTResource, http.StatusOK},
		{"ChatGPT token at /mcp: refused", gptTok, "", http.StatusUnauthorized},
	}
	for _, c := range cases {
		if got := atMount(srv, c.resource, c.token); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// A request naming BOTH resources, or one this server does not serve, is
// refused before any code is issued: a two-resource token would pass both
// mounts' checks.
func TestU2a_ARequestForTwoResourcesOrAForeignOneIsRefused(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	_, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	for name, resources := range map[string][]string{
		"both":    {testCanonicalAudience, testChatGPTResource},
		"foreign": {"https://evil.example/mcp"},
	} {
		form := url.Values{
			"client_id":             {clientID},
			"response_type":         {"code"},
			"redirect_uri":          {"https://app.test/cb"},
			"code_challenge":        {s256Challenge("verifier-u2a-refusal-abcdefghijklmnopqrstuvwxyz0123456789")},
			"code_challenge_method": {"S256"},
			"scope":                 {"pad:read"},
			"state":                 {"state-u2a-02"},
			"decision":              {"approve"},
			"csrf_token":            {csrfTok},
			"capability_tier":       {"read"},
			"allowed_workspaces":    {"*"},
			"resource":              resources,
		}
		rr := postFormWithCookie(srv, "/oauth/authorize/decide", form, sessionToken, csrfTok)
		if loc := rr.Header().Get("Location"); rr.Code == http.StatusSeeOther || rr.Code == http.StatusFound {
			u, _ := url.Parse(loc)
			if u.Query().Get("code") != "" {
				t.Errorf("%s: a code was issued for resources %v", name, resources)
			}
		}
	}
}
