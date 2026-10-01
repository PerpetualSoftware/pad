package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BUG-3322: two doors minted a full session for a user with TOTP on, past the
// second factor password login enforces. OAuth social login (the provider
// stands in for the password, not the code) and password reset (the emailed
// link proves the mailbox, not the code). Every other session-minting door was
// swept on the item trail and either checks TOTP, mints for a brand-new
// account, or inherits an existing session.

const bug3322Password = "a-Strong-Passphrase-For-3322!"

// sessionCookieSet reports whether the response set a non-empty session cookie.
func sessionCookieSet(srv *Server, rr *httptest.ResponseRecorder) bool {
	name := sessionCookieName(srv.secureCookies)
	for _, c := range rr.Result().Cookies() {
		if c.Name == name && c.Value != "" && c.MaxAge >= 0 {
			return true
		}
	}
	return false
}

// linkedTOTPUser is an existing account with GitHub linked and, when totpOn,
// TOTP enabled. It returns the user ID and the TOTP secret ("" when off).
func linkedTOTPUser(t *testing.T, srv *Server, email string, totpOn bool) (string, string) {
	t.Helper()
	user, _ := loginTestUserAs(t, srv, email, "Two Factor", bug3322Password)
	if err := srv.store.AddOAuthProvider(user.ID, "github"); err != nil {
		t.Fatalf("AddOAuthProvider: %v", err)
	}
	if !totpOn {
		return user.ID, ""
	}
	return user.ID, enableTOTPForDeleteUser(t, srv, user.ID)
}

func oauthLoginBody(email string) map[string]interface{} {
	return map[string]interface{}{
		"provider":       "github",
		"email":          email,
		"name":           "Two Factor",
		"email_verified": true,
		"avatar_url":     "https://avatars.example.com/u/1.png",
	}
}

// avatarOf reads the stored avatar, which oauth-login fills for an existing
// account that has none, but only once the sign-in has passed TOTP.
func avatarOf(t *testing.T, srv *Server, userID string) string {
	t.Helper()
	u, err := srv.store.GetUser(userID)
	if err != nil || u == nil {
		t.Fatalf("GetUser: %v", err)
	}
	return u.AvatarURL
}

type bug3322ErrorBody struct {
	Token string `json:"token"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details struct {
			ChallengeToken string `json:"challenge_token"`
		} `json:"details"`
	} `json:"error"`
}

func TestOAuthLogin_TOTPUser_GetsChallengeNotSession(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)
	userID, secret := linkedTOTPUser(t, srv, "totp-oauth@example.com", true)

	rr := postOAuthLogin(t, srv, oauthLoginBody("totp-oauth@example.com"))
	if got := avatarOf(t, srv, userID); got != "" {
		t.Errorf("a refused sign-in changed the account: avatar = %q", got)
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("oauth-login for a TOTP user: got %d, want 403: %s", rr.Code, rr.Body.String())
	}
	var body bug3322ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "two_factor_required" {
		t.Errorf("code = %q, want two_factor_required", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "2FA code") {
		t.Errorf("message %q does not tell a native shell what to do", body.Error.Message)
	}
	if body.Token != "" {
		t.Error("a session token was returned past the second factor")
	}
	if sessionCookieSet(srv, rr) {
		t.Error("a session cookie was set past the second factor")
	}
	challenge := body.Error.Details.ChallengeToken
	if challenge == "" {
		t.Fatal("no challenge_token in details: the web flow cannot continue to the code step")
	}

	// The challenge is the same one password login issues: login-verify with
	// a valid code from the same address completes the sign-in.
	rr = doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/2fa/login-verify", map[string]any{
		"challenge_token": challenge,
		"code":            validTOTPCode(t, secret),
	}, nil, "192.0.2.1:1")
	if rr.Code != http.StatusOK {
		t.Fatalf("login-verify with the oauth challenge = %d, want 200: %s", rr.Code, rr.Body.String())
	}
}

// The control: the same door for the same kind of account WITHOUT TOTP still
// mints the session, so the refusal above is the TOTP check and not some
// other refusal this fixture would hit anyway.
func TestOAuthLogin_NonTOTPUser_StillGetsSession(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)
	userID, _ := linkedTOTPUser(t, srv, "plain-oauth@example.com", false)

	rr := postOAuthLogin(t, srv, oauthLoginBody("plain-oauth@example.com"))
	if rr.Code != http.StatusOK {
		t.Fatalf("oauth-login without TOTP: got %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if got := avatarOf(t, srv, userID); got == "" {
		t.Error("a successful sign-in did not fill the missing avatar")
	}
	if !sessionCookieSet(srv, rr) {
		t.Error("oauth-login without TOTP set no session cookie")
	}
}

// The challenge binds to clientIP. The sidecar calls pad server-side, so the
// browser's address arrives only as a forwarded header, which pad must honour
// from a TRUSTED peer and ignore from anyone else.
func TestOAuthLogin_TOTPChallenge_BindsForwardedIPOnlyFromTrustedPeer(t *testing.T) {
	const browser = "198.51.100.7"
	mint := func(t *testing.T, srv *Server, peer string) string {
		t.Helper()
		body := oauthLoginBody("totp-ip@example.com")
		body["cloud_secret"] = oauthProviderTestSecret
		rr := doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/oauth-login", body, map[string]string{
			"X-Cloud-Secret":  oauthProviderTestSecret,
			"X-Forwarded-For": browser,
		}, peer)
		var b bug3322ErrorBody
		_ = json.Unmarshal(rr.Body.Bytes(), &b)
		if b.Error.Details.ChallengeToken == "" {
			t.Fatalf("no challenge minted (%d): %s", rr.Code, rr.Body.String())
		}
		return b.Error.Details.ChallengeToken
	}
	verifyFrom := func(t *testing.T, srv *Server, challenge, secret, addr string) int {
		t.Helper()
		return doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/2fa/login-verify", map[string]any{
			"challenge_token": challenge,
			"code":            validTOTPCode(t, secret),
		}, nil, addr).Code
	}

	t.Run("trusted sidecar forwards the browser address", func(t *testing.T) {
		srv := testServer(t)
		srv.SetCloudMode(oauthProviderTestSecret)
		srv.SetTrustedProxies("172.28.0.0/16")
		_, secret := linkedTOTPUser(t, srv, "totp-ip@example.com", true)
		challenge := mint(t, srv, "172.28.0.5:4000")
		if got := verifyFrom(t, srv, challenge, secret, browser+":5555"); got != http.StatusOK {
			t.Errorf("verify from the forwarded browser address = %d, want 200", got)
		}
	})

	t.Run("untrusted peer's forwarded header is ignored", func(t *testing.T) {
		srv := testServer(t)
		srv.SetCloudMode(oauthProviderTestSecret)
		srv.SetTrustedProxies("172.28.0.0/16")
		_, secret := linkedTOTPUser(t, srv, "totp-ip@example.com", true)
		challenge := mint(t, srv, "203.0.113.9:4000")
		if got := verifyFrom(t, srv, challenge, secret, browser+":5555"); got == http.StatusOK {
			t.Error("a challenge minted with a spoofed X-Forwarded-For from an untrusted peer verified from the spoofed address")
		}
	})
}

func TestResetPassword_TOTPUser_MintsNoSession(t *testing.T) {
	srv := testServer(t)
	user, oldTok := loginTestUserAs(t, srv, "totp-reset@example.com", "Reset", bug3322Password)
	enableTOTPForDeleteUser(t, srv, user.ID)
	resetTok, err := srv.store.CreatePasswordReset(user.ID)
	if err != nil {
		t.Fatalf("CreatePasswordReset: %v", err)
	}
	const newPassword = "a-Brand-New-Passphrase-3322!"

	rr := doRequest(srv, "POST", "/api/v1/auth/reset-password", map[string]string{
		"token": resetTok, "password": newPassword,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("reset-password: got %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["requires_login"] != true {
		t.Errorf("response lacks requires_login: true (keys: %v)", sortedKeys(body))
	}
	if tok, _ := body["token"].(string); tok != "" {
		t.Error("reset returned a session token past the second factor")
	}
	if sessionCookieSet(srv, rr) {
		t.Error("reset set a session cookie past the second factor")
	}
	// The other half of a reset still happens: every prior session is gone.
	if sessionAlive(t, srv, oldTok) {
		t.Error("the user's pre-reset session survived the reset")
	}

	// The new password is live, and signing in with it asks for the code.
	rr = doRequest(srv, "POST", "/api/v1/auth/login", map[string]string{
		"email": "totp-reset@example.com", "password": newPassword,
	})
	var login map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &login)
	if rr.Code != http.StatusOK || login["requires_2fa"] != true {
		t.Errorf("login with the new password = %d %v, want 200 with requires_2fa", rr.Code, sortedKeys(login))
	}
}

// Control: without TOTP the reset still signs the user in, as before.
func TestResetPassword_NonTOTPUser_StillMintsSession(t *testing.T) {
	srv := testServer(t)
	user, _ := loginTestUserAs(t, srv, "plain-reset@example.com", "Reset", bug3322Password)
	resetTok, err := srv.store.CreatePasswordReset(user.ID)
	if err != nil {
		t.Fatalf("CreatePasswordReset: %v", err)
	}
	rr := doRequest(srv, "POST", "/api/v1/auth/reset-password", map[string]string{
		"token": resetTok, "password": "a-Brand-New-Passphrase-3322!",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("reset-password: got %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if !sessionCookieSet(srv, rr) {
		t.Error("reset without TOTP set no session cookie")
	}
}
