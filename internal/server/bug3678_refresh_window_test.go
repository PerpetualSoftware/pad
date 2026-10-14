package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

// BUG-3678, through fosite's real token endpoint: a revoke or a second
// refresh that lands after fosite validated a refresh token and before it
// rotated it. The store seam runs inside that window.

func bug3678Refresh(srv *Server, clientID, refresh string) (int, map[string]any) {
	rr := postOAuthForm(srv, "/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {clientID},
		"audience":      {testCanonicalAudience},
	})
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return rr.Code, body
}

func bug3678Active(t *testing.T, srv *Server, token, bearer string) bool {
	t.Helper()
	rr := postOAuthFormBearer(srv, "/oauth/introspect", url.Values{"token": {token}}, bearer)
	if rr.Code != http.StatusOK {
		t.Fatalf("introspect: got %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	parseJSON(t, rr, &resp)
	active, _ := resp["active"].(bool)
	return active
}

func TestBUG3678_RevokeInsideRefreshWindowMintsNothing(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	user, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	tokens := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID, "verifier-3678-revoke-quick-brown-fox-1234567890")
	refresh0, _ := tokens["refresh_token"].(string)
	bearer, _ := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID, "verifier-3678-bearer-quick-brown-fox-1234567890")["access_token"].(string)

	fired := false
	restore := srv.store.SetBeforeRotateRefreshHookForTesting(func(requestID string) {
		if fired {
			return
		}
		fired = true
		if err := srv.store.RevokeUserOAuthConnection(user.ID, requestID); err != nil {
			t.Errorf("revoke inside the window: %v", err)
		}
	})
	code, body := bug3678Refresh(srv, clientID, refresh0)
	restore()
	if !fired {
		t.Fatal("the seam never ran: the refresh did not reach rotation")
	}
	if code == http.StatusOK {
		access, _ := body["access_token"].(string)
		t.Errorf("a refresh validated before the revoke got 200; minted access active=%v", bug3678Active(t, srv, access, bearer))
	}
}

func TestBUG3678_SecondRefreshInsideWindowLeavesOneWinner(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	_, sessionToken := loginTestUser(t, srv)
	csrfTok := readCSRFFromCookie(t, srv, sessionToken)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	tokens := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID, "verifier-3678-pair-quick-brown-fox-1234567890")
	refresh0, _ := tokens["refresh_token"].(string)
	bearer, _ := runAuthCodeFlow(t, srv, sessionToken, csrfTok, clientID, "verifier-3678-pbear-quick-brown-fox-1234567890")["access_token"].(string)

	var innerCode int
	var innerBody map[string]any
	fired := false
	restore := srv.store.SetBeforeRotateRefreshHookForTesting(func(string) {
		if fired {
			return
		}
		fired = true
		// The same token, presented again while the first request sits
		// between validation and rotation.
		innerCode, innerBody = bug3678Refresh(srv, clientID, refresh0)
	})
	outerCode, outerBody := bug3678Refresh(srv, clientID, refresh0)
	restore()
	if !fired {
		t.Fatal("the seam never ran: the refresh did not reach rotation")
	}
	if innerCode != http.StatusOK {
		t.Fatalf("the refresh that rotated first: got %d %v, want 200", innerCode, innerBody)
	}
	if outerCode == http.StatusOK {
		t.Errorf("both refreshes of one token succeeded; second body %v", outerBody)
	}
	innerAccess, _ := innerBody["access_token"].(string)
	if !bug3678Active(t, srv, innerAccess, bearer) {
		t.Error("the winning refresh's access token is not active")
	}
}
