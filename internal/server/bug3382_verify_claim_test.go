package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3382: a squatter registers the victim's address; the verification
// link goes to the victim. Clicking it used to verify the SQUATTER's account
// (keeping the squatter's password and session). Now a link verifies only for
// a request signed in as that account, and the mailbox owner can instead
// CLAIM the address: every credential is reset and they set their own.

type squatFixture struct {
	srv       *Server
	mails     chan capturedMail
	squatter  *models.User
	sessTok   string // the squatter's session
	token     string // the verification token, as mailed to the victim
	adminTok  string
	adminWS   *models.Workspace
	adminItem *models.Item
}

func newSquatFixture(t *testing.T) squatFixture {
	t.Helper()
	srv := testServer(t)
	adminTok := bootstrapFirstUser(t, srv, "admin@pad.test", "Admin")
	sender, mails := newMailSink(t)
	srv.baseURL = "https://app.getpad.dev"
	srv.SetEmailSender(sender)
	srv.cloudMode = true

	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email": "victim@example.com", "name": "Squatter", "password": "squatter-password-123",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("squatter register: %d %s", rr.Code, rr.Body.String())
	}
	var reg struct {
		Token string `json:"token"`
	}
	parseJSON(t, rr, &reg)
	token := extractVerifyToken(t, waitMail(t, mails))
	squatter, err := srv.store.GetUserByEmail("victim@example.com")
	if err != nil || squatter == nil {
		t.Fatalf("lookup: %v", err)
	}
	admin, _ := srv.store.GetUserByEmail("admin@pad.test")
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Admin WS", OwnerID: admin.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, admin.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	item := mustItem(t, srv, ws.ID, mustCollection(t, srv, ws.ID, "Tasks").ID, "shared")
	return squatFixture{srv, mails, squatter, reg.Token, token, adminTok, ws, item}
}

// What a squatter can plant before the claim: membership elsewhere with a
// grant, a watch and a tab there, and credentials of every kind.
func (f squatFixture) plant(t *testing.T) {
	t.Helper()
	s := f.srv.store
	admin, _ := s.GetUserByEmail("admin@pad.test")
	if err := s.AddWorkspaceMember(f.adminWS.ID, f.squatter.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateItemGrant(f.adminWS.ID, f.adminItem.ID, f.squatter.ID, "view", admin.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateWatch(f.adminWS.ID, f.squatter.ID, f.adminItem.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenWorkspaceTab(f.squatter.ID, f.adminWS.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAPIToken(f.squatter.ID, models.APITokenCreate{Name: "planted"}, 30, 0); err != nil {
		t.Fatal(err)
	}
	cli, err := s.CreateCLIAuthSession()
	if err != nil {
		t.Fatal(err)
	}
	cliTok, err := s.CreateSession(f.squatter.ID, "cli-browser-auth", "192.0.2.1", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveCLIAuthSession(cli.Code, cliTok, f.squatter.ID); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, srv *Server, query string, args ...any) int {
	t.Helper()
	var n int
	if err := srv.store.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestBUG3382_LinkWithoutTheAccountsSessionDoesNotVerify(t *testing.T) {
	f := newSquatFixture(t)

	// The victim clicks with no session: nothing is verified, the token is
	// not spent, and the squatter still holds what they held.
	rr := doRequest(f.srv, "POST", "/api/v1/auth/verify-email", map[string]string{"token": f.token})
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "verify_needs_session") {
		t.Fatalf("verify without a session: %d %s", rr.Code, rr.Body.String())
	}
	if u, _ := f.srv.store.GetUser(f.squatter.ID); u.IsEmailVerified() {
		t.Fatal("a link clicked without the account's session verified it")
	}

	// Signed in as someone else: the same answer.
	rr = doRequestWithCookie(f.srv, "POST", "/api/v1/auth/verify-email", map[string]string{"token": f.token}, f.adminTok)
	if rr.Code != http.StatusConflict {
		t.Fatalf("verify as another account: %d %s", rr.Code, rr.Body.String())
	}

	// Control: the account's own session verifies, so the token survived.
	rr = doRequestWithCookie(f.srv, "POST", "/api/v1/auth/verify-email", map[string]string{"token": f.token}, f.sessTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("verify with the account's session: %d %s", rr.Code, rr.Body.String())
	}
}

func TestBUG3382_MailboxOwnerClaimsTheAddress(t *testing.T) {
	f := newSquatFixture(t)
	f.plant(t)
	s := f.srv.store
	owned := count(t, f.srv, `SELECT COUNT(*) FROM workspaces WHERE owner_id = ? AND deleted_at IS NULL`, f.squatter.ID)
	if owned == 0 {
		t.Fatal("precondition: signup should have created the squatter's workspace")
	}

	rr := doRequest(f.srv, "POST", "/api/v1/auth/verify-email/claim", map[string]string{"token": f.token})
	if rr.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", rr.Code, rr.Body.String())
	}
	var claim struct {
		ResetPath          string   `json:"reset_path"`
		StrippedWorkspaces []string `json:"stripped_workspaces"`
		DeletedWorkspaces  []string `json:"deleted_workspaces"`
	}
	parseJSON(t, rr, &claim)
	if !strings.HasPrefix(claim.ResetPath, "/reset-password/") {
		t.Fatalf("no set-password path: %+v", claim)
	}
	if len(claim.StrippedWorkspaces) != 1 || claim.StrippedWorkspaces[0] != "Admin WS" || len(claim.DeletedWorkspaces) != owned {
		t.Errorf("claim reported %+v", claim)
	}

	u, _ := s.GetUser(f.squatter.ID)
	if !u.IsEmailVerified() || u.PasswordSet || u.TOTPEnabled {
		t.Errorf("after the claim: verified=%v password_set=%v totp=%v", u.IsEmailVerified(), u.PasswordSet, u.TOTPEnabled)
	}
	// The squatter's session, password, PAT and CLI handoff are dead.
	if rr := doRequestWithCookie(f.srv, "GET", "/api/v1/auth/me", nil, f.sessTok); rr.Code == http.StatusOK {
		t.Error("the squatter's session survived the claim")
	}
	if ok, _ := s.ValidatePassword("victim@example.com", "squatter-password-123"); ok != nil {
		t.Error("the squatter's password survived the claim")
	}
	for what, q := range map[string]string{
		"sessions":      `SELECT COUNT(*) FROM sessions WHERE user_id = ?`,
		"api tokens":    `SELECT COUNT(*) FROM api_tokens WHERE user_id = ?`,
		"cli handoffs":  `SELECT COUNT(*) FROM cli_auth_sessions WHERE user_id = ?`,
		"memberships":   `SELECT COUNT(*) FROM workspace_members WHERE user_id = ? AND workspace_id = '` + f.adminWS.ID + `'`,
		"item grants":   `SELECT COUNT(*) FROM item_grants WHERE user_id = ?`,
		"watches":       `SELECT COUNT(*) FROM watches WHERE user_id = ?`,
		"tabs":          `SELECT COUNT(*) FROM user_workspace_tabs WHERE user_id = ?`,
		"owned live ws": `SELECT COUNT(*) FROM workspaces WHERE owner_id = ? AND deleted_at IS NULL`,
	} {
		if n := count(t, f.srv, q, f.squatter.ID); n != 0 {
			t.Errorf("%s survived the claim: %d", what, n)
		}
	}

	// The claimant sets a password through the link and is signed in.
	resetToken := strings.TrimPrefix(claim.ResetPath, "/reset-password/")
	rr = doRequest(f.srv, "POST", "/api/v1/auth/reset-password", map[string]string{"token": resetToken, "password": "the-real-owners-password-9"})
	if rr.Code != http.StatusOK {
		t.Fatalf("set password: %d %s", rr.Code, rr.Body.String())
	}
	var cookie string
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookieName(f.srv.secureCookies) {
			cookie = c.Value
		}
	}
	if rr := doRequestWithCookie(f.srv, "GET", "/api/v1/auth/me", nil, cookie); rr.Code != http.StatusOK {
		t.Errorf("the claimant's new session does not work: %d", rr.Code)
	}

	// The token is spent: a second claim is refused.
	if rr := doRequest(f.srv, "POST", "/api/v1/auth/verify-email/claim", map[string]string{"token": f.token}); rr.Code != http.StatusBadRequest {
		t.Errorf("a spent token claimed again: %d", rr.Code)
	}
}

func TestBUG3382_ClaimRefusals(t *testing.T) {
	t.Run("billing attached", func(t *testing.T) {
		f := newSquatFixture(t)
		if _, err := f.srv.store.DB().Exec(`UPDATE users SET stripe_customer_id = 'cus_x' WHERE id = ?`, f.squatter.ID); err != nil {
			t.Fatal(err)
		}
		rr := doRequest(f.srv, "POST", "/api/v1/auth/verify-email/claim", map[string]string{"token": f.token})
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "account_claim_needs_support") {
			t.Fatalf("claim with billing: %d %s", rr.Code, rr.Body.String())
		}
		if rr := doRequestWithCookie(f.srv, "GET", "/api/v1/auth/me", nil, f.sessTok); rr.Code != http.StatusOK {
			t.Error("a refused claim still reset the account")
		}
	})
	t.Run("already verified", func(t *testing.T) {
		f := newSquatFixture(t)
		if err := f.srv.store.SetUserEmailVerified(f.squatter.ID); err != nil {
			t.Fatal(err)
		}
		if rr := doRequest(f.srv, "POST", "/api/v1/auth/verify-email/claim", map[string]string{"token": f.token}); rr.Code != http.StatusBadRequest {
			t.Fatalf("claim of a verified account: %d %s", rr.Code, rr.Body.String())
		}
	})
}

// The credential epoch: a sign-in that checked its credential before a
// credential change mints nothing after it.
func TestBUG3382_StaleCredentialCheckMintsNoSession(t *testing.T) {
	f := newSquatFixture(t)
	s := f.srv.store
	checked, _ := s.GetUser(f.squatter.ID) // a sign-in read the account here

	if _, err := s.UpdateUser(f.squatter.ID, models.UserUpdate{Password: ptr("a-new-password-123")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSessionFenced(checked.ID, checked.CredentialEpoch, "web", "192.0.2.1", "", time.Hour, time.Time{}); err != store.ErrCredentialsChanged {
		t.Errorf("a session minted on a stale credential check: err=%v", err)
	}
	fresh, _ := s.GetUser(f.squatter.ID)
	if _, err := s.CreateSessionFenced(fresh.ID, fresh.CredentialEpoch, "web", "192.0.2.1", "", time.Hour, time.Time{}); err != nil {
		t.Errorf("control: a current check could not mint: %v", err)
	}

	// The 2FA challenge carries the first factor's epoch.
	ch := generateTwoFAChallengeAt(checked.ID, "192.0.2.1", checked.CredentialEpoch, []byte("k"))
	if id, epoch, err := validateTwoFAChallengeEpoch(ch, "192.0.2.1", []byte("k")); err != nil || id != checked.ID || epoch != checked.CredentialEpoch {
		t.Errorf("challenge round trip: %q %d %v", id, epoch, err)
	}
}

// The cookie fallbacks in PATCH /auth/me and CLI approval do not let an
// unverified account write (codex: SessionAuth resolves the cookie first).
func TestBUG3382_UnverifiedCookieCannotUseFallbackRoutes(t *testing.T) {
	f := newSquatFixture(t)
	rr := doRequestWithCookie(f.srv, "PATCH", "/api/v1/auth/me", map[string]string{"name": "Renamed"}, f.sessTok)
	if rr.Code != http.StatusForbidden || veErrorCode(rr) != "email_not_verified" {
		t.Errorf("PATCH /auth/me as an unverified account: %d %s", rr.Code, rr.Body.String())
	}
	cli, err := f.srv.store.CreateCLIAuthSession()
	if err != nil {
		t.Fatal(err)
	}
	rr = doRequestWithCookie(f.srv, "POST", "/api/v1/auth/cli/sessions/"+cli.Code+"/approve", nil, f.sessTok)
	if rr.Code != http.StatusForbidden || veErrorCode(rr) != "email_not_verified" {
		t.Errorf("CLI approval as an unverified account: %d %s", rr.Code, rr.Body.String())
	}
}

func ptr[T any](v T) *T { return &v }
