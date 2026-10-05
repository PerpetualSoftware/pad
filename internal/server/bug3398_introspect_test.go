package server

import (
	"net/http"
	"net/url"
	"testing"
)

// BUG-3398: the public /oauth/introspect describes a token only to that
// token's own client, and never describes a refresh token. Either refusal is
// the bare {"active": false} an unknown token gets, so neither is an oracle.
func TestBug3398_IntrospectionIsOwnClientAccessTokensOnly(t *testing.T) {
	t.Parallel()
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientA := registerTestClient(t, srv, "https://app.test/cb")
	clientB := registerTestClient(t, srv, "https://app.test/cb")

	a1 := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientA, "verifier-b3398-a1-quick-brown-fox-1234567890")
	a2 := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientA, "verifier-b3398-a2-quick-brown-fox-1234567890")
	b1 := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientB, "verifier-b3398-b1-quick-brown-fox-1234567890")
	str := func(m map[string]any, k string) string {
		v, _ := m[k].(string)
		if v == "" {
			t.Fatalf("token response lacks %s: %v", k, m)
		}
		return v
	}

	introspect := func(token, bearer string) map[string]any {
		t.Helper()
		rr := postOAuthFormBearer(srv, "/oauth/introspect", url.Values{"token": {token}}, bearer)
		if rr.Code != http.StatusOK {
			t.Fatalf("introspect: %d %s", rr.Code, rr.Body.String())
		}
		var out map[string]any
		parseJSON(t, rr, &out)
		return out
	}
	inactive := func(where string, got map[string]any) {
		t.Helper()
		if len(got) != 1 || got["active"] != false {
			t.Errorf("%s: want exactly {\"active\": false}, got %v", where, got)
		}
	}

	// The control: the same client's other access token is described.
	if got := introspect(str(a1, "access_token"), str(a2, "access_token")); got["active"] != true || got["client_id"] != clientA {
		t.Fatalf("same client: want active with client_id %s, got %v", clientA, got)
	}
	// Another client's token, though the caller's own token is active.
	inactive("another client's access token", introspect(str(a1, "access_token"), str(b1, "access_token")))
	// A refresh token, even the caller's own client's.
	inactive("own client's refresh token", introspect(str(a1, "refresh_token"), str(a2, "access_token")))
	inactive("another client's refresh token", introspect(str(a1, "refresh_token"), str(b1, "access_token")))
}
