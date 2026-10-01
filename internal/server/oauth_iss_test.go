package server

import (
	"net/http"
	"net/url"
	"testing"
)

// RFC 9207 (TASK-3321 U0a): every authorize response that redirects to the
// client carries iss equal to the metadata's issuer, byte for byte. These
// cover the three kinds of redirect the authorize endpoints produce: an
// approval (code), a denial (error from the consent decision), and a fosite
// validation error on GET /oauth/authorize.

func metadataIssuer(t *testing.T, srv *Server) string {
	t.Helper()
	rr := doRequest(srv, "GET", "/.well-known/oauth-authorization-server", nil)
	var doc map[string]any
	parseJSON(t, rr, &doc)
	iss, _ := doc["issuer"].(string)
	if iss == "" {
		t.Fatalf("metadata has no issuer: %s", rr.Body.String())
	}
	return iss
}

func redirectParams(t *testing.T, rr interface {
	Header() http.Header
}) url.Values {
	t.Helper()
	u, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location does not parse: %v", err)
	}
	return u.Query()
}

func TestOAuth_AuthorizeRedirectsCarryIss(t *testing.T) {
	t.Parallel()
	srv, _ := oauthEnabledTestServer(t)
	issuer := metadataIssuer(t, srv)
	_, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)

	decide := func(decision string) url.Values {
		t.Helper()
		form := url.Values{
			"client_id":             {clientID},
			"response_type":         {"code"},
			"redirect_uri":          {"https://app.test/cb"},
			"scope":                 {"pad:read"},
			"code_challenge":        {s256Challenge("abc-12345-the-quick-brown-fox-1234567890")},
			"code_challenge_method": {"S256"},
			"audience":              {testCanonicalAudience},
			"state":                 {"state-" + decision},
			"decision":              {decision},
			"csrf_token":            {csrfTok},
			"capability_tier":       {"read"},
			"allowed_workspaces":    {"*"},
		}
		rr := postFormWithCookie(srv, "/oauth/authorize/decide", form, sessionToken, csrfTok)
		if rr.Code != http.StatusSeeOther && rr.Code != http.StatusFound {
			t.Fatalf("%s: expected a redirect, got %d: %s", decision, rr.Code, rr.Body.String())
		}
		return redirectParams(t, rr)
	}

	t.Run("approve", func(t *testing.T) {
		q := decide("approve")
		if q.Get("code") == "" {
			t.Fatalf("approval redirect has no code: %v", q)
		}
		if got := q.Get("iss"); got != issuer {
			t.Errorf("approval iss = %q, want %q", got, issuer)
		}
		if got := q.Get("state"); got != "state-approve" {
			t.Errorf("state lost: %q", got)
		}
	})
	t.Run("deny", func(t *testing.T) {
		q := decide("deny")
		if q.Get("error") != "access_denied" {
			t.Fatalf("denial redirect: %v", q)
		}
		if got := q.Get("iss"); got != issuer {
			t.Errorf("denial iss = %q, want %q", got, issuer)
		}
	})
	t.Run("authorize validation error", func(t *testing.T) {
		q := url.Values{
			"client_id":             {clientID},
			"response_type":         {"token"}, // refused in NewAuthorizeRequest: code only
			"redirect_uri":          {"https://app.test/cb"},
			"scope":                 {"pad:read"},
			"code_challenge":        {s256Challenge("abc-12345-the-quick-brown-fox-1234567890")},
			"code_challenge_method": {"S256"},
			"audience":              {testCanonicalAudience},
			"state":                 {"state-err"},
		}
		rr := doRequestWithCookie(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sessionToken)
		if rr.Code != http.StatusSeeOther && rr.Code != http.StatusFound {
			t.Fatalf("expected an error redirect, got %d: %s", rr.Code, rr.Body.String())
		}
		p := redirectParams(t, rr)
		if p.Get("error") == "" {
			t.Fatalf("no error in redirect: %v", p)
		}
		if got := p.Get("iss"); got != issuer {
			t.Errorf("error redirect iss = %q, want %q", got, issuer)
		}
	})
}

// form_post would answer with an HTML page carrying no iss, so it is refused
// at both authorize endpoints before fosite writes anything in that mode.
func TestOAuth_AuthorizeRefusesFormPost(t *testing.T) {
	t.Parallel()
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	params := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"response_mode":         {"form_post"},
		"redirect_uri":          {"https://app.test/cb"},
		"scope":                 {"pad:read"},
		"code_challenge":        {s256Challenge("abc-12345-the-quick-brown-fox-1234567890")},
		"code_challenge_method": {"S256"},
		"audience":              {testCanonicalAudience},
		"state":                 {"state-form-post"},
	}
	rr := doRequestWithCookie(srv, "GET", "/oauth/authorize?"+params.Encode(), nil, sessionToken)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("GET authorize with form_post = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	form := url.Values{}
	for k, v := range params {
		form[k] = v
	}
	form.Set("decision", "approve")
	form.Set("csrf_token", csrfTok)
	form.Set("capability_tier", "read")
	form.Set("allowed_workspaces", "*")
	rr = postFormWithCookie(srv, "/oauth/authorize/decide", form, sessionToken, csrfTok)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("decide with form_post = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "" {
		t.Errorf("a refused form_post still redirected to %q", loc)
	}
}

func TestWithIssParam(t *testing.T) {
	const iss = "https://app.getpad.dev"
	cases := []struct{ in, want string }{
		{"https://c.test/cb?code=a&state=s", "https://c.test/cb?code=a&iss=https%3A%2F%2Fapp.getpad.dev&state=s"},
		{"https://c.test/cb?error=access_denied&iss=https%3A%2F%2Fevil.test", "https://c.test/cb?error=access_denied&iss=https%3A%2F%2Fapp.getpad.dev"},
		{"https://c.test/cb#code=a&state=s", "https://c.test/cb#code=a&iss=https%3A%2F%2Fapp.getpad.dev&state=s"},
		{"com.example.app:/cb?code=a", "com.example.app:/cb?code=a&iss=https%3A%2F%2Fapp.getpad.dev"},
	}
	for _, tc := range cases {
		if got := withIssParam(tc.in, iss); got != tc.want {
			t.Errorf("withIssParam(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TASK-3321 U0a: a /.well-known/ path no route owns answers a JSON 404 rather
// than the SPA's 200 text/html, and the owned documents still answer.
func TestWellKnown_UnownedPathIsJSON404(t *testing.T) {
	t.Parallel()
	srv, _ := oauthEnabledTestServer(t)
	// Mount the SPA the way production does: its catch-all is what answered
	// these paths with 200 text/html.
	srv.SetWebUI(webFS(map[string]string{"index.html": "<!doctype html><html></html>"}))
	for _, p := range []string{
		"/.well-known/openid-configuration",
		"/.well-known/openai-apps-challenge",
		"/.well-known/anything/deeper",
	} {
		rr := doRequest(srv, "GET", p, nil)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", p, rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); ct == "" || ct[:16] != "application/json" {
			t.Errorf("%s Content-Type = %q, want application/json", p, ct)
		}
	}
	for _, p := range []string{
		"/.well-known/oauth-authorization-server",
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
	} {
		if rr := doRequest(srv, "GET", p, nil); rr.Code != http.StatusOK {
			t.Errorf("owned document %s = %d, want 200", p, rr.Code)
		}
	}
}
