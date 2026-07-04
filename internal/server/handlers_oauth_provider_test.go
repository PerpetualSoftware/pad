package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tests for the oauth-login provider allowlist (PLAN-1772 / TASK-1773).
// Apple joins github/google as an accepted provider; the find-or-create +
// provider-link path is provider-agnostic, so a fresh apple login must
// create and link a user exactly like the others, while unknown providers
// stay rejected.

const oauthProviderTestSecret = "shh-its-a-secret"

// postOAuthLogin fires a cloud-secret-authenticated oauth-login request and
// returns the recorder. The cloud secret rides the JSON body (the same
// channel validateCloudSecret reads).
func postOAuthLogin(t *testing.T, srv *Server, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	body["cloud_secret"] = oauthProviderTestSecret
	req := cloudAdminReq(t, "POST", "/api/v1/auth/oauth-login", body,
		map[string]string{"X-Cloud-Secret": oauthProviderTestSecret})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func TestOAuthLogin_AppleProvider_CreatesAndLinksUser(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)

	rr := postOAuthLogin(t, srv, map[string]interface{}{
		"provider":       "apple",
		"email":          "applefan@example.com",
		"name":           "Apple Fan",
		"email_verified": true,
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("apple oauth-login: got %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Token   string `json:"token"`
		NewUser bool   `json:"new_user"`
		User    struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Token == "" {
		t.Error("expected a session token in the response")
	}
	if !resp.NewUser {
		t.Error("first apple login should create a new user")
	}

	// The provider must have been linked, otherwise a second apple login
	// would hit the oauth_provider_not_linked gate.
	user, err := srv.store.GetUserByEmail("applefan@example.com")
	if err != nil || user == nil {
		t.Fatalf("user not created: %v", err)
	}
	if !user.HasOAuthProvider("apple") {
		t.Errorf("apple provider not linked on the new user: %q", user.OAuthProviders)
	}

	// And a returning apple login resolves the same account (no 403).
	rr2 := postOAuthLogin(t, srv, map[string]interface{}{
		"provider":       "apple",
		"email":          "applefan@example.com",
		"email_verified": true,
	})
	if rr2.Code != http.StatusOK {
		t.Fatalf("returning apple login: got %d, want 200: %s", rr2.Code, rr2.Body.String())
	}
}

func TestOAuthLogin_AppleProvider_StillRequiresVerifiedEmail(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)

	rr := postOAuthLogin(t, srv, map[string]interface{}{
		"provider":       "apple",
		"email":          "unverified@example.com",
		"email_verified": false,
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("unverified apple email: got %d, want 403: %s", rr.Code, rr.Body.String())
	}
}

// TestOAuthLogin_SessionTTLMatchesWebSession guards B9 (TASK-1932): OAuth
// sessions used to mint a 30-day cookie while every other web login (and
// the underlying store session row) used the shorter webSessionTTL. A
// longer-lived cookie outliving its server-side session produces silent
// 401s once the store row expires but the browser keeps presenting the
// cookie. createAuthSession derives the session cookie's MaxAge from the
// same ttl argument it uses for the store row, so asserting the cookie is
// sufficient to pin both.
func TestOAuthLogin_SessionTTLMatchesWebSession(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)

	rr := postOAuthLogin(t, srv, map[string]interface{}{
		"provider":       "google",
		"email":          "ttlcheck@example.com",
		"name":           "TTL Check",
		"email_verified": true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("oauth-login: got %d, want 200: %s", rr.Code, rr.Body.String())
	}

	var sessionCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookieName(false) {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected oauth-login to set a session cookie")
	}
	wantMaxAge := int(webSessionTTL.Seconds())
	if sessionCookie.MaxAge != wantMaxAge {
		t.Errorf("oauth session cookie MaxAge = %d, want %d (webSessionTTL) — OAuth sessions must not outlive the web session TTL",
			sessionCookie.MaxAge, wantMaxAge)
	}
}

// TestOAuthLink_AppleProvider_Links exercises the second allowlist site
// (handleOAuthLink) through the real handler, proving the shared helper was
// wired there too — not just in oauth-login. (oauth-unlink shares the same
// isSupportedOAuthProvider gate; it's covered by the unit test.)
func TestOAuthLink_AppleProvider_Links(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)

	// Create the account first via a google login, then link apple to it.
	if rr := postOAuthLogin(t, srv, map[string]interface{}{
		"provider":       "google",
		"email":          "linker@example.com",
		"name":           "Linker",
		"email_verified": true,
	}); rr.Code != http.StatusOK {
		t.Fatalf("seed google login: got %d: %s", rr.Code, rr.Body.String())
	}

	req := cloudAdminReq(t, "POST", "/api/v1/auth/oauth-link", map[string]interface{}{
		"provider":       "apple",
		"email":          "linker@example.com",
		"email_verified": true,
		"cloud_secret":   oauthProviderTestSecret,
	}, map[string]string{"X-Cloud-Secret": oauthProviderTestSecret})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("oauth-link apple: got %d, want 200: %s", rr.Code, rr.Body.String())
	}

	user, err := srv.store.GetUserByEmail("linker@example.com")
	if err != nil || user == nil {
		t.Fatalf("user lookup: %v", err)
	}
	if !user.HasOAuthProvider("apple") {
		t.Errorf("apple not linked via oauth-link: %q", user.OAuthProviders)
	}
}

func TestOAuthLogin_UnknownProvider_Rejected(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)

	rr := postOAuthLogin(t, srv, map[string]interface{}{
		"provider":       "facebook",
		"email":          "someone@example.com",
		"email_verified": true,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown provider: got %d, want 400: %s", rr.Code, rr.Body.String())
	}
}

func TestIsSupportedOAuthProvider(t *testing.T) {
	for _, p := range []string{"github", "google", "apple"} {
		if !isSupportedOAuthProvider(p) {
			t.Errorf("%q should be supported", p)
		}
	}
	for _, p := range []string{"", "facebook", "Apple", "GITHUB", "twitter"} {
		if isSupportedOAuthProvider(p) {
			t.Errorf("%q must not be supported (allowlist is exact-match)", p)
		}
	}
}
