package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
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

// codex round 2: approving a CLI sign-in from an existing session is not a
// fresh sign-in. The CLI session it mints carries the approver's sign-in
// time, so it cannot confirm-delete; and a PAT cannot approve at all.
func TestBUG3336_CLIApprovalDoesNotMintAFreshSignIn(t *testing.T) {
	srv, u, token := oauthOnlyCloud(t)
	ageAllSessions(t, srv, u.ID, time.Hour)
	pending, err := srv.store.CreateCLIAuthSession()
	if err != nil {
		t.Fatal(err)
	}
	if rr := doRequestWithCookieFrom(srv, "POST", "/api/v1/auth/cli/sessions/"+pending.Code+"/approve", nil, token, "198.51.100.7:5555"); rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}
	approved, err := srv.store.GetCLIAuthSession(pending.Code)
	if err != nil || approved == nil || approved.Token == "" {
		t.Fatalf("approved CLI session: %+v %v", approved, err)
	}
	if rr := doRequestWithBearer(srv, "POST", "/api/v1/auth/delete-account", approved.Token, map[string]any{"confirm": true}); rr.Code != http.StatusForbidden {
		t.Fatalf("confirm-only with a CLI session minted by an old one: %d %s, want 403", rr.Code, rr.Body.String())
	}
	stillThere(t, srv, u.ID)

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Owned", OwnerID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	pat, err := srv.store.CreateAPIToken(u.ID, models.APITokenCreate{Name: "leaked", WorkspaceID: ws.ID}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := srv.store.CreateCLIAuthSession()
	if rr := doRequestWithBearer(srv, "POST", "/api/v1/auth/cli/sessions/"+again.Code+"/approve", pat.Token, nil); rr.Code != http.StatusForbidden || errorCode(t, rr) != "session_required" {
		t.Fatalf("PAT approving a CLI sign-in: %d %s, want 403 session_required", rr.Code, rr.Body.String())
	}
}

// The session a request authenticated with is found the way TokenAuth reads
// the header, padding included; and a rotation with no readable session of
// the user mints nothing that could read as freshly signed in.
func TestBUG3336_RotationNeedsTheRequestSession(t *testing.T) {
	srv, u, token := oauthOnlyCloud(t)
	req := httptest.NewRequest("POST", "/api/v1/auth/oauth-unlink", nil)
	req.Header.Set("Authorization", "Bearer  "+token+" ")
	if info := srv.requestSessionInfo(req); info == nil || info.User.ID != u.ID {
		t.Fatalf("a padded bearer session was not found: %+v", info)
	}
	if _, ok := srv.rotateSessionsAfterCredentialChange(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil), u); ok {
		t.Fatal("a rotation with no request session minted a session")
	}
}

// A rotation never extends a session past SessionMaxLifetime from its
// original sign-in.
func TestBUG3336_RotationKeepsTheLifetimeCap(t *testing.T) {
	srv, u, token := oauthOnlyCloud(t)
	ageAllSessions(t, srv, u.ID, store.SessionMaxLifetime-24*time.Hour)
	req := httptest.NewRequest("POST", "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: token})
	rotated, ok := srv.rotateSessionsAfterCredentialChange(httptest.NewRecorder(), req, u)
	if !ok {
		t.Fatal("rotation failed")
	}
	var expiresAt string
	if err := srv.store.DB().QueryRow(`SELECT expires_at FROM sessions WHERE user_id = ? ORDER BY expires_at DESC LIMIT 1`, u.ID).Scan(&expiresAt); err != nil {
		t.Fatal(err)
	}
	exp, _ := time.Parse(time.RFC3339, expiresAt)
	if until := time.Until(exp); until > 25*time.Hour {
		t.Fatalf("rotation of an 89-day-old session expires in %v; the cap is a day away", until)
	}
	_ = rotated
}
