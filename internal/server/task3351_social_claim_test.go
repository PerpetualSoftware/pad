package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TASK-3351 PR 2: the sidecar OAuth routes answer only in cloud mode.

// A self-hosted install that happens to carry a cloud secret (copied
// config, a leftover env var) used to answer oauth-login, which creates
// accounts and mints sessions on the caller's word about an email.
func TestTASK3351_SidecarOAuthRoutesAreCloudOnly(t *testing.T) {
	for _, path := range []string{"/api/v1/auth/oauth-login", "/api/v1/auth/oauth-link", "/api/v1/auth/oauth-unlink"} {
		t.Run(path, func(t *testing.T) {
			srv := testServer(t)
			// Signed in, so link and unlink reach their route rather than
			// stopping at authentication.
			sess := bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
			srv.cloudSecrets = []string{oauthProviderTestSecret} // a secret, but not cloud mode
			body := map[string]interface{}{
				"cloud_secret": oauthProviderTestSecret,
				"provider":     "google",
				// An existing, verified account: ungated, link would answer 200
				// rather than a coincidental 404 for an unknown address.
				"email":          "admin@example.com",
				"email_verified": true,
			}
			req := cloudAdminReq(t, "POST", path, body, map[string]string{"X-Cloud-Secret": oauthProviderTestSecret})
			req.Header.Set("Authorization", "Bearer "+sess)
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != http.StatusNotFound {
				t.Fatalf("%s off cloud: got %d, want 404: %s", path, rr.Code, rr.Body.String())
			}
		})
	}
	// Control: the same login in cloud mode is answered.
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)
	rr := postOAuthLogin(t, srv, map[string]interface{}{
		"provider": "google", "email": "someone@example.com", "email_verified": true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("control, cloud mode: got %d: %s", rr.Code, rr.Body.String())
	}
}

func oauthIdentityRows(t *testing.T, srv *Server, provider, subject string) int {
	t.Helper()
	return count(t, srv, `SELECT COUNT(*) FROM user_oauth_identities WHERE provider = ? AND subject = ?`, provider, subject)
}

// The first sign-in that carries a subject binds it; a different provider
// account asserting the same address is refused, and the refusal names the
// way back in (TASK-3351 U2).
func TestTASK3351_ProviderSubjectBindsOnNextLoginThenMustMatch(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)
	login := func(subject string) *httptest.ResponseRecorder {
		body := map[string]interface{}{"provider": "google", "email": "owner@example.com", "email_verified": true}
		if subject != "" {
			body["subject"] = subject
		}
		return postOAuthLogin(t, srv, body)
	}
	// An account linked before subjects existed: no subject sent, no row.
	if rr := login(""); rr.Code != http.StatusOK {
		t.Fatalf("first login: %d %s", rr.Code, rr.Body.String())
	}
	if n := oauthIdentityRows(t, srv, "google", "g-111"); n != 0 {
		t.Fatalf("precondition: %d rows", n)
	}
	// The next login carrying a subject populates it.
	if rr := login("g-111"); rr.Code != http.StatusOK {
		t.Fatalf("populating login: %d %s", rr.Code, rr.Body.String())
	}
	if n := oauthIdentityRows(t, srv, "google", "g-111"); n != 1 {
		t.Fatalf("subject not bound: %d rows", n)
	}
	// The same subject again: fine.
	if rr := login("g-111"); rr.Code != http.StatusOK {
		t.Fatalf("matching login: %d %s", rr.Code, rr.Body.String())
	}
	// A different provider account with the same address (deleted and
	// recreated at the provider): refused, naming the recovery route, and no
	// session.
	rr := login("g-222")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("mismatched subject: %d %s", rr.Code, rr.Body.String())
	}
	var refusal struct {
		Error struct {
			Code    string                 `json:"code"`
			Message string                 `json:"message"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
		Token string `json:"token"`
	}
	parseJSON(t, rr, &refusal)
	if refusal.Error.Code != "oauth_subject_mismatch" || refusal.Token != "" {
		t.Fatalf("refusal: %+v", refusal)
	}
	if refusal.Error.Details["recovery"] != "password_reset" || refusal.Error.Details["recovery_path"] != "/forgot-password" ||
		!strings.Contains(refusal.Error.Message, "Forgot password") {
		t.Errorf("the refusal must name the recovery route: %+v", refusal.Error)
	}
	if n := oauthIdentityRows(t, srv, "google", "g-222"); n != 0 {
		t.Error("the mismatched subject was bound")
	}
}

// A provider account already bound to one pad account signs in to no other,
// and creates no account for the new address it asserts.
func TestTASK3351_SubjectBoundElsewhereCreatesNothing(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)
	if rr := postOAuthLogin(t, srv, map[string]interface{}{"provider": "google", "email": "first@example.com", "email_verified": true, "subject": "g-1"}); rr.Code != http.StatusOK {
		t.Fatalf("first: %d %s", rr.Code, rr.Body.String())
	}
	rr := postOAuthLogin(t, srv, map[string]interface{}{"provider": "google", "email": "moved@example.com", "email_verified": true, "subject": "g-1"})
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "oauth_subject_mismatch") {
		t.Fatalf("subject bound elsewhere: %d %s", rr.Code, rr.Body.String())
	}
	if u, _ := srv.store.GetUserByEmail("moved@example.com"); u != nil {
		t.Error("an account was created for the moved address")
	}
}

// Unlinking forgets the bound provider account, so a relink binds the new
// one.
func TestTASK3351_UnlinkForgetsTheSubject(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)
	if rr := postOAuthLogin(t, srv, map[string]interface{}{"provider": "google", "email": "u@example.com", "email_verified": true, "subject": "g-old"}); rr.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rr.Code, rr.Body.String())
	}
	u, _ := srv.store.GetUserByEmail("u@example.com")
	if err := srv.store.RemoveOAuthProvider(u.ID, "google"); err != nil {
		t.Fatal(err)
	}
	if n := oauthIdentityRows(t, srv, "google", "g-old"); n != 0 {
		t.Fatalf("unlink kept the binding: %d", n)
	}
	if err := srv.store.AddOAuthProvider(u.ID, "google"); err != nil {
		t.Fatal(err)
	}
	if rr := postOAuthLogin(t, srv, map[string]interface{}{"provider": "google", "email": "u@example.com", "email_verified": true, "subject": "g-new"}); rr.Code != http.StatusOK {
		t.Fatalf("relinked login with the new subject: %d %s", rr.Code, rr.Body.String())
	}
}

// oauth-link binds too, and refuses a provider account bound elsewhere
// before linking anything.
func TestTASK3351_LinkBindsAndRefusesASubjectBoundElsewhere(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)
	if rr := postOAuthLogin(t, srv, map[string]interface{}{"provider": "google", "email": "a@example.com", "email_verified": true, "subject": "g-a"}); rr.Code != http.StatusOK {
		t.Fatalf("a: %d %s", rr.Code, rr.Body.String())
	}
	if rr := postOAuthLogin(t, srv, map[string]interface{}{"provider": "github", "email": "b@example.com", "email_verified": true}); rr.Code != http.StatusOK {
		t.Fatalf("b: %d %s", rr.Code, rr.Body.String())
	}
	link := func(subject string) *httptest.ResponseRecorder {
		body := map[string]interface{}{"cloud_secret": oauthProviderTestSecret, "provider": "google", "email": "b@example.com", "email_verified": true, "subject": subject}
		req := cloudAdminReq(t, "POST", "/api/v1/auth/oauth-link", body, map[string]string{"X-Cloud-Secret": oauthProviderTestSecret})
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}
	if rr := link("g-a"); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "oauth_subject_mismatch") {
		t.Fatalf("link with a's subject: %d %s", rr.Code, rr.Body.String())
	}
	b, _ := srv.store.GetUserByEmail("b@example.com")
	if b.HasOAuthProvider("google") {
		t.Error("the refused link linked the provider")
	}
	if rr := link("g-b"); rr.Code != http.StatusOK {
		t.Fatalf("link with b's own subject: %d %s", rr.Code, rr.Body.String())
	}
	if n := oauthIdentityRows(t, srv, "google", "g-b"); n != 1 {
		t.Fatalf("link did not bind: %d", n)
	}
}

func TestTASK3351_SubjectMustBePrintable(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)
	for _, bad := range []string{"g\x00x", "g\nx", strings.Repeat("x", 256)} {
		rr := postOAuthLogin(t, srv, map[string]interface{}{"provider": "google", "email": "p@example.com", "email_verified": true, "subject": bad})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("subject %q: %d", bad, rr.Code)
		}
	}
}

// --- U3: social claim ---

type socialClaimResp struct {
	Token              string   `json:"token"`
	Claimed            bool     `json:"claimed"`
	StrippedWorkspaces []string `json:"stripped_workspaces"`
	DeletedWorkspaces  []string `json:"deleted_workspaces"`
}

func newSocialSquat(t *testing.T) squatFixture {
	t.Helper()
	f := newSquatFixture(t)
	f.srv.cloudSecrets = []string{oauthProviderTestSecret}
	return f
}

// The address's owner signs in with a provider that vouches for the address,
// on an account somebody else registered and never verified. The account is
// claimed: every credential reset, access elsewhere stripped, owned
// workspaces deleted, and the owner signed in, with a notice by email.
func TestTASK3351_GoogleClaimsAnUnverifiedAccount(t *testing.T) {
	f := newSocialSquat(t)
	f.plant(t)
	s := f.srv.store
	owned := count(t, f.srv, `SELECT COUNT(*) FROM workspaces WHERE owner_id = ? AND deleted_at IS NULL`, f.squatter.ID)

	rr := postOAuthLogin(t, f.srv, map[string]interface{}{
		"provider": "google", "email": "victim@example.com", "email_verified": true, "subject": "g-victim", "name": "Real Owner",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("social claim: %d %s", rr.Code, rr.Body.String())
	}
	var resp socialClaimResp
	parseJSON(t, rr, &resp)
	if !resp.Claimed || resp.Token == "" {
		t.Fatalf("response: %+v", resp)
	}
	if len(resp.StrippedWorkspaces) != 1 || resp.StrippedWorkspaces[0] != "Admin WS" || len(resp.DeletedWorkspaces) != owned {
		t.Errorf("reported %+v (owned %d)", resp, owned)
	}

	// The claimant's session works; the squatter's credentials do not.
	if rr := doRequestWithCookie(f.srv, "GET", "/api/v1/auth/me", nil, resp.Token); rr.Code != http.StatusOK {
		t.Errorf("claimant session: %d", rr.Code)
	}
	if rr := doRequestWithCookie(f.srv, "GET", "/api/v1/auth/me", nil, f.sessTok); rr.Code == http.StatusOK {
		t.Error("the squatter's session survived")
	}
	if ok, _ := s.ValidatePassword("victim@example.com", "squatter-password-123"); ok != nil {
		t.Error("the squatter's password survived")
	}
	for what, q := range map[string]string{
		"api tokens":    `SELECT COUNT(*) FROM api_tokens WHERE user_id = ?`,
		"cli handoffs":  `SELECT COUNT(*) FROM cli_auth_sessions WHERE user_id = ?`,
		"memberships":   `SELECT COUNT(*) FROM workspace_members WHERE user_id = ? AND workspace_id = '` + f.adminWS.ID + `'`,
		"item grants":   `SELECT COUNT(*) FROM item_grants WHERE user_id = ?`,
		"watches":       `SELECT COUNT(*) FROM watches WHERE user_id = ?`,
		"owned live ws": `SELECT COUNT(*) FROM workspaces WHERE owner_id = ? AND deleted_at IS NULL`,
	} {
		if n := count(t, f.srv, q, f.squatter.ID); n != 0 {
			t.Errorf("%s survived the claim: %d", what, n)
		}
	}
	u, _ := s.GetUser(f.squatter.ID)
	if u.Name != "Real Owner" {
		t.Errorf("the registrant's display name survived: %q", u.Name)
	}
	if !u.IsEmailVerified() || !u.HasOAuthProvider("google") || u.PasswordSet {
		t.Errorf("after: verified=%v google=%v password_set=%v", u.IsEmailVerified(), u.HasOAuthProvider("google"), u.PasswordSet)
	}
	if n := oauthIdentityRows(t, f.srv, "google", "g-victim"); n != 1 {
		t.Errorf("subject not bound: %d", n)
	}

	// The notice reaches the mailbox owner and names what was removed.
	m := waitMail(t, f.mails)
	if m.to != "victim@example.com" || !strings.Contains(m.body, "Admin WS") {
		t.Errorf("notice: to=%q subject=%q", m.to, m.subject)
	}
}

// GitHub claims only for the account's primary address (TASK-3351 ruling
// 2). A sidecar that does not say so claims nothing.
func TestTASK3351_GitHubClaimsOnlyThePrimaryAddress(t *testing.T) {
	f := newSocialSquat(t)
	rr := postOAuthLogin(t, f.srv, map[string]interface{}{
		"provider": "github", "email": "victim@example.com", "email_verified": true,
	})
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "oauth_provider_not_linked") {
		t.Fatalf("github without email_primary: %d %s", rr.Code, rr.Body.String())
	}
	rr = postOAuthLogin(t, f.srv, map[string]interface{}{
		"provider": "github", "email": "victim@example.com", "email_verified": true, "email_primary": false,
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("github, secondary address: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doRequestWithCookie(f.srv, "GET", "/api/v1/auth/me", nil, f.sessTok); rr.Code != http.StatusOK {
		t.Fatalf("a refused claim changed the account: squatter session %d", rr.Code)
	}
	rr = postOAuthLogin(t, f.srv, map[string]interface{}{
		"provider": "github", "email": "victim@example.com", "email_verified": true, "email_primary": true,
	})
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"claimed":true`) {
		t.Fatalf("github primary: %d %s", rr.Code, rr.Body.String())
	}
}

// A verified account is never claimed: its owner proved the address.
func TestTASK3351_VerifiedAccountIsNotClaimable(t *testing.T) {
	f := newSocialSquat(t)
	rr := postOAuthLogin(t, f.srv, map[string]interface{}{
		"provider": "google", "email": "admin@pad.test", "email_verified": true,
	})
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "oauth_provider_not_linked") {
		t.Fatalf("verified account: %d %s", rr.Code, rr.Body.String())
	}
}

// Billing is never moved by a claim (ruling 4): refused with a support
// contact, and nothing changes.
func TestTASK3351_SocialClaimRefusesBilling(t *testing.T) {
	f := newSocialSquat(t)
	if _, err := f.srv.store.DB().Exec(`UPDATE users SET stripe_customer_id = 'cus_x' WHERE id = ?`, f.squatter.ID); err != nil {
		t.Fatal(err)
	}
	rr := postOAuthLogin(t, f.srv, map[string]interface{}{
		"provider": "google", "email": "victim@example.com", "email_verified": true,
	})
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "account_claim_needs_support") {
		t.Fatalf("billing: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doRequestWithCookie(f.srv, "GET", "/api/v1/auth/me", nil, f.sessTok); rr.Code != http.StatusOK {
		t.Fatalf("the refused claim changed the account: %d", rr.Code)
	}
}
