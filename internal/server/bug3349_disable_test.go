package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3349 (b, c): disabling an account ends every credential it holds, and a
// disabled account is refused everywhere but sign-out and the session check.

func countRows(t *testing.T, srv *Server, query string, args ...any) int {
	t.Helper()
	var n int
	if err := srv.store.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// (b) Admin disable revokes sessions, PATs and OAuth (MCP) grants in one go,
// and re-enabling restores none of them.
func TestBUG3349_DisableRevokesEveryCredential(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	oauthSess := newOAuthSession(t, srv)
	mcpTok, _ := mintWithResource(t, srv, oauthSess, testCanonicalAudience)
	target, err := srv.store.GetUserByEmail("oauth-test@example.com")
	if err != nil || target == nil {
		t.Fatal(err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Theirs", OwnerID: target.ID})
	if err != nil {
		t.Fatal(err)
	}
	pat, err := srv.store.CreateAPIToken(target.ID, models.APITokenCreate{Name: "pat", WorkspaceID: ws.ID}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rr := postMCP(srv, "/mcp", mcpTok); rr.Code != http.StatusOK {
		t.Fatalf("fixture: the OAuth grant does not work at /mcp (%d)", rr.Code)
	}
	if rr := doRequestWithBearer(srv, "GET", "/api/v1/workspaces", pat.Token, nil); rr.Code != http.StatusOK {
		t.Fatalf("fixture: the PAT does not work (%d)", rr.Code)
	}

	admin, err := srv.store.CreateUser(models.UserCreate{Email: "admin-3349@example.com", Name: "Admin", Password: "pw-test-12345", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	adminSess, err := srv.store.CreateSession(admin.ID, "web", "192.0.2.9", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rr := doRequestWithCookieFrom(srv, "POST", "/api/v1/admin/users/"+target.ID+"/disable", nil, adminSess, "192.0.2.9:1"); rr.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rr.Code, rr.Body.String())
	}

	check := func(when string) {
		t.Helper()
		if rr := doRequestWithBearer(srv, "GET", "/api/v1/workspaces", pat.Token, nil); rr.Code == http.StatusOK {
			t.Errorf("%s: the PAT still works", when)
		}
		if rr := postMCP(srv, "/mcp", mcpTok); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s: the OAuth grant still works at /mcp (%d)", when, rr.Code)
		}
		if n := countRows(t, srv, `SELECT COUNT(*) FROM oauth_connections WHERE user_id = ?`, target.ID); n != 0 {
			t.Errorf("%s: %d OAuth connection rows survive", when, n)
		}
		if n := countRows(t, srv, `SELECT COUNT(*) FROM oauth_access_tokens WHERE subject = ? AND active = 1`, target.ID); n != 0 {
			t.Errorf("%s: %d active OAuth access tokens survive", when, n)
		}
		if n := countRows(t, srv, `SELECT COUNT(*) FROM oauth_refresh_tokens WHERE subject = ? AND active = 1`, target.ID); n != 0 {
			t.Errorf("%s: %d active OAuth refresh tokens survive", when, n)
		}
		if n := countRows(t, srv, `SELECT COUNT(*) FROM api_tokens WHERE user_id = ?`, target.ID); n != 0 {
			t.Errorf("%s: %d API tokens survive", when, n)
		}
	}
	check("after disable")
	if u, _ := srv.store.GetUser(target.ID); u == nil || !u.IsDisabled() {
		t.Fatal("admin disable did not set disabled_at")
	}
	if info, _ := srv.store.ValidateSession(oauthSess.sessionToken); info != nil {
		t.Fatal("the account's session survived the disable")
	}
	if rr := doRequestWithCookieFrom(srv, "POST", "/api/v1/admin/users/"+target.ID+"/enable", nil, adminSess, "192.0.2.9:1"); rr.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rr.Code, rr.Body.String())
	}
	check("after re-enable")
	if info, _ := srv.store.ValidateSession(oauthSess.sessionToken); info != nil {
		t.Fatal("re-enabling revived the account's old session")
	}
}

// (c) A disabled account whose credential survived (minted between the
// disable and the revoke, say) is refused before the /api/v1/auth/*
// exemption. Sign-out and the session check stay open, and the session
// check says why.
func TestBUG3349_DisabledAccountIsRefusedBeforeTheAuthExemption(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	user, err := srv.store.CreateUser(models.UserCreate{Email: "disabled-3349@example.com", Name: "Gone", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Theirs", OwnerID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	pat, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{Name: "survivor", WorkspaceID: ws.ID}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Disabled, but the credential not revoked: the survivor case.
	if err := srv.store.DisableUser(user.ID); err != nil {
		t.Fatal(err)
	}

	if rr := doRequestWithBearer(srv, "GET", "/api/v1/auth/export", pat.Token, nil); rr.Code != http.StatusForbidden || errorCode(t, rr) != "account_disabled" {
		t.Fatalf("disabled account's PAT on /auth/export: %d %s, want 403 account_disabled", rr.Code, rr.Body.String())
	}
	rr := doRequestWithBearer(srv, "GET", "/api/v1/auth/session", pat.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("session check: %d %s", rr.Code, rr.Body.String())
	}
	var state map[string]any
	parseJSON(t, rr, &state)
	if state["authenticated"] != false || state["account_disabled"] != true {
		t.Fatalf("session check for a disabled account: %v, want authenticated=false account_disabled=true", state)
	}
	if rr := doRequestWithBearer(srv, "POST", "/api/v1/auth/logout", pat.Token, nil); rr.Code == http.StatusForbidden {
		t.Fatalf("sign-out refused for a disabled account: %d %s", rr.Code, rr.Body.String())
	}
	if rr := postMCP(srv, "/mcp", pat.Token); rr.Code != http.StatusUnauthorized {
		t.Fatalf("disabled account's PAT at /mcp: %d, want 401", rr.Code)
	}
}

// (c) at the MCP door, for an OAuth grant that survived the revoke.
func TestBUG3349_DisabledAccountsOAuthGrantIsRefusedAtMCP(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	sess := newOAuthSession(t, srv)
	tok, _ := mintWithResource(t, srv, sess, testCanonicalAudience)
	user, err := srv.store.GetUserByEmail("oauth-test@example.com")
	if err != nil || user == nil {
		t.Fatal(err)
	}
	if err := srv.store.DisableUser(user.ID); err != nil {
		t.Fatal(err)
	}
	if rr := postMCP(srv, "/mcp", tok); rr.Code != http.StatusUnauthorized {
		t.Fatalf("disabled account's OAuth grant at /mcp: %d, want 401", rr.Code)
	}
}

// codex review: a refresh token (or an authorization code) issued before the
// disable does not exchange for new tokens after it.
func TestBUG3349_TokenEndpointRefusesADisabledSubject(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	sess := newOAuthSession(t, srv)
	if _, code := mintWithResource(t, srv, sess, testCanonicalAudience); code != http.StatusOK {
		t.Fatalf("mint: %d", code)
	}
	refresh, client := lastRefresh, lastClientID
	user, _ := srv.store.GetUserByEmail("oauth-test@example.com")
	if err := srv.store.DisableUser(user.ID); err != nil {
		t.Fatal(err)
	}
	rr := postOAuthForm(srv, "/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {client},
	})
	// The token endpoint's own refusal, not a storage failure behind it
	// (codex review): a 500 would mean the check was skipped and the store's
	// gate caught it.
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid_grant") {
		t.Fatalf("a disabled account's refresh exchange: %d %s, want 400 invalid_grant", rr.Code, rr.Body.String())
	}
}

// codex review: the consent step (outside RequireAuth) does not treat a
// disabled account's session as signed in.
func TestBUG3349_ConsentRefusesADisabledSession(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	sess := newOAuthSession(t, srv)
	user, _ := srv.store.GetUserByEmail("oauth-test@example.com")
	if err := srv.store.DisableUser(user.ID); err != nil {
		t.Fatal(err)
	}
	if _, code := mintWithResource(t, srv, sess, testCanonicalAudience); code == http.StatusOK {
		t.Fatal("a disabled account's session completed an OAuth grant")
	}
	if n := countRows(t, srv, `SELECT COUNT(*) FROM oauth_connections WHERE user_id = ?`, user.ID); n != 0 {
		t.Fatalf("%d OAuth connections were created for a disabled account", n)
	}
}

// codex review: a 2FA challenge issued before the disable mints no session
// after it.
func TestBUG3349_TOTPChallengeMintsNothingForADisabledAccount(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)
	userID, secret := linkedTOTPUser(t, srv, "totp-3349@example.com", true)
	body := oauthLoginBody("totp-3349@example.com")
	body["cloud_secret"] = oauthProviderTestSecret
	rr := doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/oauth-login", body,
		map[string]string{"X-Cloud-Secret": oauthProviderTestSecret}, "198.51.100.7:1")
	var b bug3322ErrorBody
	_ = json.Unmarshal(rr.Body.Bytes(), &b)
	if b.Error.Details.ChallengeToken == "" {
		t.Fatalf("no challenge minted (%d): %s", rr.Code, rr.Body.String())
	}
	if err := srv.store.DisableUser(userID); err != nil {
		t.Fatal(err)
	}
	before := countRows(t, srv, `SELECT COUNT(*) FROM sessions WHERE user_id = ?`, userID)
	vr := doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/2fa/login-verify", map[string]any{
		"challenge_token": b.Error.Details.ChallengeToken,
		"code":            validTOTPCode(t, secret),
	}, nil, "198.51.100.7:1")
	if vr.Code != http.StatusForbidden || errorCode(t, vr) != "account_disabled" {
		t.Fatalf("2FA verify for a disabled account: %d %s, want 403 account_disabled", vr.Code, vr.Body.String())
	}
	if n := countRows(t, srv, `SELECT COUNT(*) FROM sessions WHERE user_id = ?`, userID); n != before {
		t.Fatalf("sessions %d -> %d: 2FA verify minted one for a disabled account", before, n)
	}
}

// codex review: no session is created for a disabled account, wherever the
// mint is reached from; the store refuses it under the users row.
func TestBUG3349_NoSessionForADisabledAccount(t *testing.T) {
	srv := testServer(t)
	u, err := srv.store.CreateUser(models.UserCreate{Email: "mint-3349@example.com", Name: "M", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.DisableUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.CreateSession(u.ID, "cli-browser-auth", "192.0.2.1", "", time.Hour); !errors.Is(err, store.ErrUserDisabled) {
		t.Fatalf("CreateSession for a disabled account: %v, want ErrUserDisabled", err)
	}
}

// Share links a disabled account created are suspended: the same answer as
// an unknown link while disabled, serving again once re-enabled (ruling).
func TestBUG3349_ShareLinksAreSuspendedWhileTheCreatorIsDisabled(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Shared", `{"status":"open"}`)
	ws, _ := srv.store.GetWorkspaceBySlug(slug)
	creator, err := srv.store.CreateUser(models.UserCreate{Email: "sharer-3349@example.com", Name: "S", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	link, err := srv.store.CreateShareLink(ws.ID, "item", item.ID, "view", creator.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rr := doRequest(srv, "GET", "/api/v1/s/"+link.Token, nil); rr.Code != http.StatusOK {
		t.Fatalf("fixture: the link does not serve (%d)", rr.Code)
	}
	if err := srv.store.DisableUser(creator.ID); err != nil {
		t.Fatal(err)
	}
	suspended := doRequest(srv, "GET", "/api/v1/s/"+link.Token, nil)
	unknown := doRequest(srv, "GET", "/api/v1/s/not-a-real-share-token-3349", nil)
	if suspended.Code != unknown.Code || suspended.Body.String() != unknown.Body.String() {
		t.Fatalf("suspended link %d %s vs unknown %d %s, want byte-identical", suspended.Code, suspended.Body.String(), unknown.Code, unknown.Body.String())
	}
	if err := srv.store.EnableUser(creator.ID); err != nil {
		t.Fatal(err)
	}
	if rr := doRequest(srv, "GET", "/api/v1/s/"+link.Token, nil); rr.Code != http.StatusOK {
		t.Fatalf("re-enabled creator's link: %d, want it serving again", rr.Code)
	}
}

// codex review round 2: email verification is an account acting; a disabled
// one is refused, and the token is not spent.
func TestBUG3349_EmailVerificationRefusesADisabledAccount(t *testing.T) {
	srv := testServer(t)
	u, err := srv.store.CreateUser(models.UserCreate{Email: "verify-3349@example.com", Name: "V", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := srv.store.CreateEmailVerification(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.DisableUser(u.ID); err != nil {
		t.Fatal(err)
	}
	rr := doRequest(srv, "POST", "/api/v1/auth/verify-email", map[string]any{"token": tok})
	if rr.Code != http.StatusForbidden || errorCode(t, rr) != "account_disabled" {
		t.Fatalf("verify-email for a disabled account: %d %s, want 403 account_disabled", rr.Code, rr.Body.String())
	}
	if pending, _ := srv.store.LookupEmailVerification(tok); pending == nil {
		t.Fatal("the refused verification spent its token")
	}
}

// codex review round 2: the authorize step itself (GET, before consent)
// treats a disabled account's session as signed out.
func TestBUG3349_AuthorizeTreatsADisabledSessionAsSignedOut(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	sess := newOAuthSession(t, srv)
	user, _ := srv.store.GetUserByEmail("oauth-test@example.com")
	q := url.Values{
		"client_id": {sess.clientID}, "response_type": {"code"}, "redirect_uri": {"https://app.test/cb"},
		"scope": {"pad:read"}, "code_challenge": {s256Challenge("verifier-3349-abcdefghijklmnopqrstuvwxyz0123456789")},
		"code_challenge_method": {"S256"}, "state": {"state-3349-01"}, "resource": {testCanonicalAudience},
	}
	if rr := doRequestWithCookie(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sess.sessionToken); rr.Code != http.StatusOK {
		t.Fatalf("fixture: the consent page does not render for an enabled session (%d)", rr.Code)
	}
	if err := srv.store.DisableUser(user.ID); err != nil {
		t.Fatal(err)
	}
	rr := doRequestWithCookie(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sess.sessionToken)
	if rr.Code != http.StatusFound || !strings.HasPrefix(rr.Header().Get("Location"), "/login") {
		t.Fatalf("authorize with a disabled session: %d → %q, want a redirect to sign in", rr.Code, rr.Header().Get("Location"))
	}
}
