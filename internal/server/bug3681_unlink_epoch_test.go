package server

import (
	"net/http"
	"testing"
)

// BUG-3681: POST /api/v1/auth/oauth-unlink advances credential_epoch and fences
// the session rotation on the post-unlink epoch so the caller receives a valid
// replacement session cookie while older sessions are invalidated.
func TestBUG3681_OAuthUnlinkAdvancesEpochAndRotatesSession(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)

	u, err := srv.store.CreateOAuthUser("unlink-epoch@example.com", "Unlink Epoch", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.LinkOAuthProvider(u.ID, "github", "gh-3681"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.LinkOAuthProvider(u.ID, "google", "g-3681"); err != nil {
		t.Fatal(err)
	}

	before, err := srv.store.GetUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}

	oldTok, err := srv.store.CreateSession(u.ID, "web", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}

	// Precondition: initial session authenticates /api/v1/auth/me.
	if rr := doRequestWithCookie(srv, "GET", "/api/v1/auth/me", nil, oldTok); rr.Code != http.StatusOK {
		t.Fatalf("pre-unlink GET /api/v1/auth/me: got %d, want 200: %s", rr.Code, rr.Body.String())
	}

	// Unlink github through the real router.
	rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/oauth-unlink", map[string]any{
		"provider": "github",
	}, oldTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/auth/oauth-unlink: got %d, want 200: %s", rr.Code, rr.Body.String())
	}

	cookieName := sessionCookieName(srv.secureCookies)
	var newTok string
	for _, c := range rr.Result().Cookies() {
		if c.Name == cookieName && c.Value != "" {
			newTok = c.Value
			break
		}
	}
	if newTok == "" || newTok == oldTok {
		t.Fatalf("oauth-unlink did not set a new %s cookie (got %q, old %q)", cookieName, newTok, oldTok)
	}

	// The new session cookie must authenticate a follow-up request.
	if me := doRequestWithCookie(srv, "GET", "/api/v1/auth/me", nil, newTok); me.Code != http.StatusOK {
		t.Fatalf("post-unlink GET /api/v1/auth/me with rotated cookie: got %d, want 200: %s", me.Code, me.Body.String())
	}

	// The old session must be revoked.
	if stale := doRequestWithCookie(srv, "GET", "/api/v1/auth/me", nil, oldTok); stale.Code == http.StatusOK {
		t.Fatal("old session stayed valid after oauth-unlink")
	}

	// The account's credential_epoch must have advanced by exactly 1.
	after, err := srv.store.GetUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.CredentialEpoch != before.CredentialEpoch+1 {
		t.Errorf("CredentialEpoch = %d, want %d", after.CredentialEpoch, before.CredentialEpoch+1)
	}
	if after.HasOAuthProvider("github") || !after.HasOAuthProvider("google") {
		t.Errorf("providers after unlink = %v, want [google]", after.GetOAuthProviders())
	}
}
