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
