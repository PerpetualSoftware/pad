package server

import (
	"net/http"
	"testing"
)

// BUG-3679: a reset link issued before an ordinary password change is dead
// after it. Driven through the HTTP profile change and the HTTP reset, so the
// test vouches for the wiring, not only the store.
func TestBUG3679_ProfilePasswordChangeKillsOutstandingResetLink(t *testing.T) {
	srv := testServer(t)
	const oldPwd = "an-Old-Passphrase-For-3679!"
	const ownerPwd = "the-Owners-New-Passphrase-3679!"
	user, sess := loginTestUserAs(t, srv, "bug3679@example.com", "Owner", oldPwd)

	link, err := srv.store.CreatePasswordReset(user.ID)
	if err != nil {
		t.Fatalf("CreatePasswordReset: %v", err)
	}

	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/auth/me", map[string]string{
		"current_password": oldPwd,
		"new_password":     ownerPwd,
	}, sess)
	if rr.Code != http.StatusOK {
		t.Fatalf("password change: got %d, want 200: %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(srv, "POST", "/api/v1/auth/reset-password", map[string]string{
		"token": link, "password": "an-Attackers-Passphrase-3679!",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("reset with pre-change link: got %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if got, _ := srv.store.ValidatePassword(user.Email, ownerPwd); got == nil {
		t.Fatal("the owner's new password no longer signs in")
	}
}
