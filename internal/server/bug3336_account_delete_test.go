package server

import (
	"net/http"
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
