package server

import (
	"net/http"
	"strings"
	"testing"
)

// TestPasswordChange_InvalidatesOtherSessions verifies that rotating a
// password (a) deletes every existing session for the user except the
// caller's, and (b) re-issues a fresh session cookie for the caller.
// Before TASK-652 a stolen cookie stayed valid forever after the owner
// "rotated" their password.
func TestPasswordChange_InvalidatesOtherSessions(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	// Log in twice from different devices (same account). The bootstrap
	// call above already established one session; grab its token by logging
	// in again with the known password.
	login := func() string {
		rr := doRequest(srv, "POST", "/api/v1/auth/login", map[string]string{
			"email":    "admin@example.com",
			"password": "correct-horse-battery-staple",
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("login: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp map[string]interface{}
		parseJSON(t, rr, &resp)
		tok, _ := resp["token"].(string)
		if tok == "" {
			t.Fatal("login returned no token")
		}
		return tok
	}

	// Session A: the "other device" session that should die when password changes.
	otherSessionToken := login()

	// Session B: the "current tab" session — the one we use to trigger the change.
	currentSessionToken := login()

	// Sanity: both cookies are valid before the change.
	for _, tok := range []string{otherSessionToken, currentSessionToken} {
		rr := doRequestWithCookie(srv, "GET", "/api/v1/auth/me", nil, tok)
		if rr.Code != http.StatusOK {
			t.Fatalf("pre-change /me with token: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
	}

	// Change password via the "current" session.
	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/auth/me", map[string]string{
		"current_password": "correct-horse-battery-staple",
		"new_password":     "second-strong-test-passphrase-42",
	}, currentSessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("password change: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// The response must set a new session cookie — confirming the caller
	// got re-issued. Scan Set-Cookie headers for the session cookie name.
	foundNewSessionCookie := false
	for _, c := range rr.Result().Cookies() {
		if strings.HasPrefix(c.Name, "pad_session") && c.Value != "" && c.Value != currentSessionToken {
			foundNewSessionCookie = true
			break
		}
	}
	if !foundNewSessionCookie {
		t.Fatalf("password change did not set a fresh session cookie; headers=%v", rr.Header())
	}

	// The OTHER device's session token must now be invalid.
	rr = doRequestWithCookie(srv, "GET", "/api/v1/auth/me", nil, otherSessionToken)
	if rr.Code == http.StatusOK {
		t.Fatal("other session stayed valid after password change — defeats the rotation")
	}

	// For defensive measure, the OLD current-session token should also be
	// invalid (the caller got a NEW token in the response). The test above
	// already proved that — if the old token still worked, both sessions
	// would still be alive.
	rr = doRequestWithCookie(srv, "GET", "/api/v1/auth/me", nil, currentSessionToken)
	if rr.Code == http.StatusOK {
		t.Fatal("old caller token stayed valid after password change")
	}
}
