package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// BUG-3671: a social sign-in binds to the provider's stable subject. An
// account linked before subjects existed binds the first subject a verified
// sign-in carries (the migration); from then on a different subject is
// refused (TASK-3351), and so is a sign-in carrying NO subject: an email
// match alone no longer reaches an account tied to a provider account.
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
	if o := got(login("owner@example.com", "")); o.code != http.StatusOK {
		t.Fatalf("legacy signup: %+v", o)
	}
	// Before any subject is bound, the email path is unchanged.
	if o := got(login("owner@example.com", "")); o.code != http.StatusOK {
		t.Fatalf("subject-less login before any bind: %+v", o)
	}
	steps := []struct {
		name    string
		subject string
		want    outcome
	}{
		{"the first verified sign-in with a subject binds it (migration)", "g-owner", outcome{http.StatusOK, ""}},
		{"the same subject signs in", "g-owner", outcome{http.StatusOK, ""}},
		{"NO subject is now refused (email alone)", "", outcome{http.StatusForbidden, "oauth_subject_mismatch"}},
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
