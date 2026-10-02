package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3336: account deletion is irreversible and soft-deletes every owned
// workspace. Confirm-only (no password) is for the one account that has no
// password to give, an OAuth-only cloud account; and an API token never
// deletes the account it belongs to.

func stillThere(t *testing.T, srv *Server, id string) {
	t.Helper()
	if u, _ := srv.store.GetUser(id); u == nil {
		t.Fatal("the account was deleted")
	}
}

// A cloud account WITH a password cannot skip it by sending confirm.
func TestBUG3336_ConfirmOnlyRefusedForAPasswordAccount(t *testing.T) {
	srv := testServer(t)
	srv.cloudMode = true
	id, token := bootstrapAccountDeleteUser(t, srv, "")
	rr := deleteAccountReq(srv, map[string]any{"confirm": true}, token)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("confirm-only delete of a password account: %d %s, want 400", rr.Code, rr.Body.String())
	}
	stillThere(t, srv, id)
}

// A PAT is refused whatever it sends, before any identity check, so it can
// neither delete nor probe the password.
func TestBUG3336_APITokenCannotDeleteTheAccount(t *testing.T) {
	for _, cloud := range []bool{false, true} {
		srv := testServer(t)
		srv.cloudMode = cloud
		id, _ := bootstrapAccountDeleteUser(t, srv, "")
		ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Owned", OwnerID: id})
		if err != nil {
			t.Fatal(err)
		}
		pat, err := srv.store.CreateAPIToken(id, models.APITokenCreate{Name: "leaked", WorkspaceID: ws.ID}, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range []map[string]any{
			{"password": "correct-horse-battery-staple"},
			{"confirm": true},
		} {
			rr := doRequestWithBearer(srv, "POST", "/api/v1/auth/delete-account", pat.Token, body)
			if rr.Code != http.StatusForbidden || errorCode(t, rr) != "session_required" {
				t.Fatalf("cloud=%v PAT delete %v: %d %s, want 403 session_required", cloud, body, rr.Code, rr.Body.String())
			}
		}
		stillThere(t, srv, id)
	}
}

// The path confirm-only exists for still works: an OAuth-only cloud account.
func TestBUG3336_OAuthOnlyCloudAccountDeletesWithConfirm(t *testing.T) {
	srv := testServer(t)
	srv.cloudMode = true
	bootstrapFirstUser(t, srv, "admin-3336@test.com", "Admin")
	u, err := srv.store.CreateOAuthUser("oauth-3336@test.com", "OAuth Only", "")
	if err != nil {
		t.Fatal(err)
	}
	token, err := srv.store.CreateSession(u.ID, "go-test", "198.51.100.7", "", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rr := deleteAccountReq(srv, map[string]any{"confirm": true}, token); rr.Code != http.StatusOK {
		t.Fatalf("OAuth-only confirm delete: %d %s, want 200", rr.Code, rr.Body.String())
	}
	if got, _ := srv.store.GetUser(u.ID); got != nil {
		t.Fatal("the OAuth-only account survived its confirmed delete")
	}
}

// oauthOnlyCloud is a cloud server with an initialized instance and an
// OAuth-only account holding a fresh session.
func oauthOnlyCloud(t *testing.T) (*Server, *models.User, string) {
	t.Helper()
	srv := testServer(t)
	srv.cloudMode = true
	bootstrapFirstUser(t, srv, "admin-3336@test.com", "Admin")
	u, err := srv.store.CreateOAuthUser("oauth-age-3336@test.com", "OAuth Only", "")
	if err != nil {
		t.Fatal(err)
	}
	token, err := srv.store.CreateSession(u.ID, "go-test", "198.51.100.7", "", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return srv, u, token
}

// ageAllSessions dates every session of the user d into the past.
func ageAllSessions(t *testing.T, srv *Server, userID string, d time.Duration) {
	t.Helper()
	at := time.Now().UTC().Add(-d).Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(`UPDATE sessions SET created_at = ? WHERE user_id = ?`, at, userID); err != nil {
		t.Fatal(err)
	}
}

// Confirm-only needs a recent sign-in: an 11-minute-old session is refused
// with reauth_required and the account survives.
func TestBUG3336_ConfirmOnlyNeedsARecentSignIn(t *testing.T) {
	srv, u, token := oauthOnlyCloud(t)
	ageAllSessions(t, srv, u.ID, accountDeleteReauthWindow+time.Minute)
	rr := deleteAccountReq(srv, map[string]any{"confirm": true}, token)
	if rr.Code != http.StatusForbidden || errorCode(t, rr) != "reauth_required" {
		t.Fatalf("confirm-only with an old session: %d %s, want 403 reauth_required", rr.Code, rr.Body.String())
	}
	stillThere(t, srv, u.ID)
}

// A legacy hybrid account (a real password, password_set false) is held to
// the same window: a fresh sign-in may delete it, an old session may not.
func TestBUG3336_HybridAccountConfirmOnlyIsWindowed(t *testing.T) {
	srv := testServer(t)
	srv.cloudMode = true
	id, token := bootstrapAccountDeleteUser(t, srv, "")
	if _, err := srv.store.DB().Exec(`UPDATE users SET password_set = ? WHERE id = ?`, false, id); err != nil {
		t.Fatal(err)
	}
	ageAllSessions(t, srv, id, time.Hour)
	if rr := deleteAccountReq(srv, map[string]any{"confirm": true}, token); rr.Code != http.StatusForbidden {
		t.Fatalf("hybrid account, old session, confirm-only: %d %s, want 403", rr.Code, rr.Body.String())
	}
	stillThere(t, srv, id)
	fresh, err := srv.store.CreateSession(id, "go-test", "198.51.100.7", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rr := deleteAccountReq(srv, map[string]any{"confirm": true}, fresh); rr.Code != http.StatusOK {
		t.Fatalf("hybrid account, fresh sign-in, confirm-only: %d %s, want 200", rr.Code, rr.Body.String())
	}
}

// A credential-change rotation does not restart the sign-in clock: the new
// session inherits the replaced one's created_at, and cannot confirm-delete.
func TestBUG3336_RotationKeepsTheSignInTime(t *testing.T) {
	srv, u, token := oauthOnlyCloud(t)
	ageAllSessions(t, srv, u.ID, time.Hour)
	req := httptest.NewRequest("POST", "/api/v1/auth/totp/enable", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: token})
	rotated, ok := srv.rotateSessionsAfterCredentialChange(httptest.NewRecorder(), req, u)
	if !ok {
		t.Fatal("rotation failed")
	}
	info, err := srv.store.ValidateSession(rotated)
	if err != nil || info == nil {
		t.Fatalf("rotated session: %v", err)
	}
	if age := time.Since(info.CreatedAt); age < 50*time.Minute {
		t.Fatalf("the rotated session reads %v old; it should keep the original sign-in time", age)
	}
	if rr := deleteAccountReq(srv, map[string]any{"confirm": true}, rotated); rr.Code != http.StatusForbidden {
		t.Fatalf("confirm-only with a rotated old session: %d %s, want 403", rr.Code, rr.Body.String())
	}
}
