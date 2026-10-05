package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// BUG-3400: fosite revokes a reused refresh token's (or code's) whole family
// BEFORE checking that the token belongs to the requesting client. Pad's
// storage answers "not found" when a client other than the token's own
// presents a spent token, so another client's replay revokes nothing.
//
// These tests are also the positive control that the check is live: it reads
// the requesting client from fosite.AccessRequestContextKey, and if a fosite
// bump stopped putting the access request in the context, the check would
// quietly stop firing and the cross-client cases below would revoke A's
// family again, failing them.

func bug3400Refresh(srv *Server, refresh, clientID string) *httpResponse {
	rr := postOAuthForm(srv, "/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {clientID},
		"audience":      {testCanonicalAudience},
	})
	return &httpResponse{code: rr.Code, body: rr.Body.String()}
}

type httpResponse struct {
	code int
	body string
}

func TestBug3400_RefreshReuseByAnotherClientRevokesNothing(t *testing.T) {
	t.Parallel()
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientA := registerTestClient(t, srv, "https://app.test/cb")
	clientB := registerTestClient(t, srv, "https://app.test/cb")

	tok := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientA, "verifier-b3400-r-quick-brown-fox-1234567890-abc")
	refresh0, _ := tok["refresh_token"].(string)
	// A rotates once: refresh0 is now spent, refresh1 is A's live token.
	rot := bug3400Refresh(srv, refresh0, clientA)
	if rot.code != http.StatusOK {
		t.Fatalf("A's first rotation: %d %s", rot.code, rot.body)
	}
	var r1 map[string]any
	if err := jsonUnmarshalString(rot.body, &r1); err != nil {
		t.Fatal(err)
	}
	refresh1, _ := r1["refresh_token"].(string)

	// B presents A's SPENT token under its own client id: refused, and
	// nothing of A's is revoked.
	if got := bug3400Refresh(srv, refresh0, clientB); got.code == http.StatusOK {
		t.Fatalf("B redeemed A's spent refresh token: %s", got.body)
	}
	// B presents A's LIVE token: fosite's own client check refuses it, as
	// before, and revokes nothing either.
	if got := bug3400Refresh(srv, refresh1, clientB); got.code == http.StatusOK {
		t.Fatalf("B redeemed A's live refresh token: %s", got.body)
	}
	// A's family is intact: A's live token still rotates.
	alive := bug3400Refresh(srv, refresh1, clientA)
	if alive.code != http.StatusOK {
		t.Fatalf("A's family was revoked by another client's replay: A's live refresh answered %d %s", alive.code, alive.body)
	}
	var r2 map[string]any
	if err := jsonUnmarshalString(alive.body, &r2); err != nil {
		t.Fatal(err)
	}
	refresh2, _ := r2["refresh_token"].(string)

	// Control: A's OWN replay of a spent token still revokes the family.
	if got := bug3400Refresh(srv, refresh1, clientA); got.code == http.StatusOK {
		t.Fatalf("A's own replay of a spent token succeeded: %s", got.body)
	}
	if got := bug3400Refresh(srv, refresh2, clientA); got.code == http.StatusOK {
		t.Fatalf("A's own replay did not revoke its family: the newest refresh still rotates (%s)", got.body)
	}
}

func TestBug3400_CodeReuseByAnotherClientRevokesNothing(t *testing.T) {
	t.Parallel()
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientA := registerTestClient(t, srv, "https://app.test/cb")
	clientB := registerTestClient(t, srv, "https://app.test/cb")
	const verifier = "verifier-b3400-c-quick-brown-fox-1234567890-abc"

	code := authCodeFor(t, srv, sessionToken, csrfTok, clientA, verifier, testCanonicalAudience)
	exchange := func(clientID string) *httpResponse {
		rr := postOAuthForm(srv, "/oauth/token", url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"client_id":     {clientID},
			"redirect_uri":  {"https://app.test/cb"},
			"code_verifier": {verifier},
			"audience":      {testCanonicalAudience},
		})
		return &httpResponse{code: rr.Code, body: rr.Body.String()}
	}
	first := exchange(clientA)
	if first.code != http.StatusOK {
		t.Fatalf("A's exchange: %d %s", first.code, first.body)
	}
	var tok map[string]any
	if err := jsonUnmarshalString(first.body, &tok); err != nil {
		t.Fatal(err)
	}
	refresh, _ := tok["refresh_token"].(string)

	// B presents A's USED code: refused, and A's grant survives.
	if got := exchange(clientB); got.code == http.StatusOK {
		t.Fatalf("B exchanged A's used code: %s", got.body)
	}
	alive := bug3400Refresh(srv, refresh, clientA)
	if alive.code != http.StatusOK {
		t.Fatalf("A's grant was revoked by another client's code replay: %d %s", alive.code, alive.body)
	}
	var r1 map[string]any
	if err := jsonUnmarshalString(alive.body, &r1); err != nil {
		t.Fatal(err)
	}
	refresh1, _ := r1["refresh_token"].(string)

	// Control: A's OWN replay of the used code still revokes the grant.
	if got := exchange(clientA); got.code == http.StatusOK {
		t.Fatalf("A's own code replay succeeded: %s", got.body)
	}
	if got := bug3400Refresh(srv, refresh1, clientA); got.code == http.StatusOK {
		t.Fatalf("A's own code replay did not revoke its grant: its refresh still rotates (%s)", got.body)
	}
}

// The case the check fully closes: the token's own client is CONFIDENTIAL (an
// installed app's client, here holding a delegated grant), so nobody else can
// name it without its secret. Another client presenting the app's spent
// refresh token under its own id must not revoke the app's family. (Another
// INSTALL client is refused earlier, by the token handler's ownership
// preflight, so the presenter here is a public DCR client, which reaches
// fosite and therefore this check.)
func TestBug3400_SpentTokenOfAConfidentialClientCannotBeBurnedByAnother(t *testing.T) {
	f := delegatedFixture(t, "inst-3400", "read")
	code, _ := codeFrom(f.decide(t, f.authorizeParams(), "approve", ""))
	if code == "" {
		t.Fatal("no delegated code")
	}
	first := f.exchange(code)
	if first.Code != http.StatusOK {
		t.Fatalf("exchange: %d %s", first.Code, first.Body.String())
	}
	var tok map[string]any
	parseJSON(t, first, &tok)
	refresh0, _ := tok["refresh_token"].(string)
	appRefresh := func(refresh string) *httptest.ResponseRecorder {
		return postTokenBasic(f.srv, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "resource": {testAppAPIAudience}},
			f.in.clientID, f.in.secret)
	}
	rot := appRefresh(refresh0)
	if rot.Code != http.StatusOK {
		t.Fatalf("the app's rotation: %d %s", rot.Code, rot.Body.String())
	}
	var r1 map[string]any
	parseJSON(t, rot, &r1)
	refresh1, _ := r1["refresh_token"].(string)

	dcr := registerTestClient(t, f.srv, "https://app.test/cb")
	if got := bug3400Refresh(f.srv, refresh0, dcr); got.code == http.StatusOK {
		t.Fatalf("a public client redeemed the app's spent refresh token: %s", got.body)
	}
	if rr := appRefresh(refresh1); rr.Code != http.StatusOK {
		t.Fatalf("the app's family was revoked by another client's replay: %d %s", rr.Code, rr.Body.String())
	}
}
