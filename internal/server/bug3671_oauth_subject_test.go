package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BUG-3671: a social sign-in binds to the provider's stable subject. An
// account linked before subjects existed binds the first subject a verified
// sign-in carries (the migration); from then on a different subject is
// refused (TASK-3351). A sign-in carrying NO subject is refused too, bound or
// not, since BUG-3797 (it was refused only once one was bound).
func TestBUG3671_AnEmailAloneNoLongerReachesASubjectBoundAccount(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)
	login := func(email, subject string) *httptest.ResponseRecorder {
		body := map[string]interface{}{"provider": "google", "email": email, "email_verified": true}
		if subject != "" {
			body["subject"] = subject
		}
		return postOAuthLogin(t, srv, body)
	}
	type outcome struct {
		code  int
		error string
	}
	got := func(rr *httptest.ResponseRecorder) outcome {
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
			Token string `json:"token"`
		}
		parseJSON(t, rr, &body)
		if rr.Code != http.StatusOK && body.Token != "" {
			t.Fatalf("a refusal minted a session: %s", rr.Body.String())
		}
		return outcome{rr.Code, body.Error.Code}
	}

	// An account linked before subjects existed: no subject bound.
	legacy, err := srv.store.CreateOAuthUser("owner@example.com", "Owner", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.LinkOAuthProvider(legacy.ID, "google", ""); err != nil {
		t.Fatal(err)
	}
	// Before any subject is bound, an email alone is refused too (BUG-3797).
	if o := got(login("owner@example.com", "")); o != (outcome{http.StatusBadRequest, "oauth_subject_required"}) {
		t.Fatalf("subject-less login before any bind: %+v", o)
	}
	steps := []struct {
		name    string
		subject string
		want    outcome
	}{
		{"the first verified sign-in with a subject binds it (migration)", "g-owner", outcome{http.StatusOK, ""}},
		{"the same subject signs in", "g-owner", outcome{http.StatusOK, ""}},
		{"NO subject is refused (email alone)", "", outcome{http.StatusBadRequest, "oauth_subject_required"}},
		{"a different subject is refused (a reassigned address)", "g-newowner", outcome{http.StatusForbidden, "oauth_subject_mismatch"}},
		{"the bound subject still signs in afterwards", "g-owner", outcome{http.StatusOK, ""}},
	}
	for _, st := range steps {
		if o := got(login("owner@example.com", st.subject)); o != st.want {
			t.Fatalf("%s: got %+v, want %+v", st.name, o, st.want)
		}
	}
	if n := oauthIdentityRows(t, srv, "google", "g-newowner"); n != 0 {
		t.Fatal("the reassigned address's subject was bound")
	}
}

// Lead note (a): a verified account that never linked the provider, receiving
// a subject-bearing sign-in for its address, is neither bound nor signed in.
// It follows today's link rule: oauth_provider_not_linked.
func TestBUG3671_AnUnlinkedAccountIsNotBoundBySignIn(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)
	bootstrapFirstUser(t, srv, "pw@example.com", "Password User")
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE users SET email_verified_at = '2026-01-01T00:00:00Z' WHERE email = ?`), "pw@example.com"); err != nil {
		t.Fatal(err)
	}
	rr := postOAuthLogin(t, srv, map[string]interface{}{
		"provider": "google", "email": "pw@example.com", "email_verified": true, "subject": "g-intruder",
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("got %d %s, want 403 oauth_provider_not_linked", rr.Code, rr.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Token string `json:"token"`
	}
	parseJSON(t, rr, &body)
	if body.Error.Code != "oauth_provider_not_linked" || body.Token != "" {
		t.Fatalf("refusal: %+v", body)
	}
	if n := oauthIdentityRows(t, srv, "google", "g-intruder"); n != 0 {
		t.Fatal("an unlinked account was bound to the subject by a sign-in")
	}
}

// Codex r1 (Low): the refusal comes before the 2FA challenge, so a
// subject-less sign-in to a bound account with TOTP on gets no challenge
// token either, while the bound subject still reaches the challenge. Since
// BUG-3797 that refusal is oauth_subject_required.
func TestBUG3671_ASubjectlessSignInGetsNoTwoFactorChallenge(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)
	linkedTOTPUser(t, srv, "totp@example.com", true)
	withSubject := oauthLoginBody("totp@example.com")
	withSubject["subject"] = "gh-totp"
	rr := postOAuthLogin(t, srv, withSubject)
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "two_factor_required") {
		t.Fatalf("bound subject: got %d %s, want the 2FA challenge", rr.Code, rr.Body.String())
	}
	if n := oauthIdentityRows(t, srv, "github", "gh-totp"); n != 1 {
		t.Fatalf("the subject was not bound before the challenge: %d rows", n)
	}
	subjectless := oauthLoginBody("totp@example.com")
	delete(subjectless, "subject")
	rr = postOAuthLogin(t, srv, subjectless)
	body := rr.Body.String()
	if rr.Code != http.StatusBadRequest || !strings.Contains(body, "oauth_subject_required") || strings.Contains(body, "challenge_token") {
		t.Fatalf("subject-less: got %d %s, want oauth_subject_required and no challenge", rr.Code, body)
	}
}

// Codex r1 (Low): a subject-less oauth-link never changes a bound subject.
func TestBUG3671_ASubjectlessLinkKeepsTheBoundSubject(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)
	if rr := postOAuthLogin(t, srv, map[string]interface{}{"provider": "google", "email": "kept@example.com", "email_verified": true, "subject": "g-kept"}); rr.Code != http.StatusOK {
		t.Fatalf("signup: %d %s", rr.Code, rr.Body.String())
	}
	body := map[string]interface{}{"cloud_secret": oauthProviderTestSecret, "provider": "google", "email": "kept@example.com", "email_verified": true}
	req := cloudAdminReq(t, "POST", "/api/v1/auth/oauth-link", body, map[string]string{"X-Cloud-Secret": oauthProviderTestSecret})
	srv.ServeHTTP(httptest.NewRecorder(), req)
	u, _ := srv.store.GetUserByEmail("kept@example.com")
	if got, err := srv.store.BoundOAuthSubject(u.ID, "google"); err != nil || got != "g-kept" {
		t.Fatalf("bound subject after a subject-less link: %q, %v; want g-kept", got, err)
	}
}
