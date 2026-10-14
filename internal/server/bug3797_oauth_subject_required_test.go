package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3797: an OAuth sign-in without the provider account's subject is
// refused, whatever the account's binding state. BUG-3671 refused it only once
// a subject was bound, so every account unbound until its first
// subject-bearing sign-in still signed in on its email alone, and a new
// address still made an account.
func TestBUG3797_ASignInWithoutASubjectIsRefusedForEveryProvider(t *testing.T) {
	for _, provider := range []string{"github", "google", "apple"} {
		t.Run(provider, func(t *testing.T) {
			srv := testServer(t)
			srv.SetCloudMode(oauthProviderTestSecret)

			// A linked account with no subject bound (linked before subjects).
			unbound, err := srv.store.CreateOAuthUser("unbound@example.com", "Unbound", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := srv.store.LinkOAuthProvider(unbound.ID, provider, ""); err != nil {
				t.Fatal(err)
			}
			// A linked account with one bound.
			bound, err := srv.store.CreateOAuthUser("bound@example.com", "Bound", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := srv.store.LinkOAuthProvider(bound.ID, provider, provider+"-bound"); err != nil {
				t.Fatal(err)
			}
			// A never-verified registrant: the claim path.
			if _, err := srv.store.CreateUser(models.UserCreate{Email: "registrant@example.com", Name: "Registrant", Password: "correct-horse-battery-staple"}); err != nil {
				t.Fatal(err)
			}

			for _, email := range []string{"new@example.com", "unbound@example.com", "bound@example.com", "registrant@example.com"} {
				for _, subject := range []any{nil, ""} {
					body := map[string]interface{}{"provider": provider, "email": email, "email_verified": true, "email_primary": true}
					if subject != nil {
						body["subject"] = subject
					}
					rr := postOAuthLogin(t, srv, body)
					var resp struct {
						Error struct {
							Code string `json:"code"`
						} `json:"error"`
						Token string `json:"token"`
					}
					parseJSON(t, rr, &resp)
					if rr.Code != http.StatusBadRequest || resp.Error.Code != "oauth_subject_required" || resp.Token != "" {
						t.Fatalf("%s subject %#v: got %d %s, want 400 oauth_subject_required", email, subject, rr.Code, rr.Body.String())
					}
				}
			}
			if u, _ := srv.store.GetUserByEmail("new@example.com"); u != nil {
				t.Fatal("a subject-less sign-in created an account")
			}
			if n := count(t, srv, `SELECT COUNT(*) FROM sessions`); n != 0 {
				t.Fatalf("%d sessions minted", n)
			}

			// The control: the unbound account signs in once a subject is sent,
			// and binds it.
			rr := postOAuthLogin(t, srv, map[string]interface{}{"provider": provider, "email": "unbound@example.com", "email_verified": true, "subject": provider + "-1"})
			if rr.Code != http.StatusOK {
				t.Fatalf("control: %d %s", rr.Code, rr.Body.String())
			}
			if n := oauthIdentityRows(t, srv, provider, provider+"-1"); n != 1 {
				t.Fatalf("control: the subject was not bound (%d rows)", n)
			}
		})
	}
}
