package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3392 (SPEC-6 U4): every sign-in door refuses an installed app's bot,
// before its credential, with the same answer an unknown account gets.

const task3392Password = "correct-horse-battery-staple"

// task3392Bot creates a bot through its one real door, then makes it
// CREDENTIAL-ELIGIBLE by SQL: a valid bcrypt hash of a known password and a
// verified email. The real bot has a sentinel hash bcrypt refuses to compare
// and no verified address, so a door test against it passes with NO kind gate
// at all; these tests remove every other reason a door could say no, so what
// they measure is the kind.
func task3392Bot(t *testing.T, srv *Server, installID string) *models.User {
	t.Helper()
	tx, err := srv.store.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	bot, err := srv.store.CreateAppUserTx(tx, installID, "Support Portal")
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("CreateAppUserTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(task3392Password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.DB().Exec(`UPDATE users SET password_hash = ?, password_set = 1, email_verified_at = ? WHERE id = ?`,
		string(hash), time.Now().UTC().Format(time.RFC3339), bot.ID); err != nil {
		t.Fatal(err)
	}
	return bot
}

func task3392SetKind(t *testing.T, srv *Server, userID, kind string) {
	t.Helper()
	if _, err := srv.store.DB().Exec(`UPDATE users SET kind = ? WHERE id = ?`, kind, userID); err != nil {
		t.Fatal(err)
	}
}

// sameAnswer asserts two responses are byte-identical in status and body.
func task3392SameAnswer(t *testing.T, door string, bot, unknown interface {
	Result() *http.Response
}) {
	t.Helper()
	b, u := bot.Result(), unknown.Result()
	bb, _ := io.ReadAll(b.Body)
	ub, _ := io.ReadAll(u.Body)
	if b.StatusCode != u.StatusCode || string(bb) != string(ub) {
		t.Errorf("%s: a bot answered %d %s, an unknown account %d %s", door, b.StatusCode, bb, u.StatusCode, ub)
	}
}

func TestTask3392_PasswordLoginAnswersABotAsUnknown(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin-3392@example.com", "Admin")
	bot := task3392Bot(t, srv, "inst-login")
	rb := doRequest(srv, "POST", "/api/v1/auth/login", map[string]string{"email": bot.Email, "password": task3392Password})
	ru := doRequest(srv, "POST", "/api/v1/auth/login", map[string]string{"email": "nobody-3392@example.com", "password": task3392Password})
	task3392SameAnswer(t, "login", rb, ru)
	if n := countRows(t, srv, `SELECT COUNT(*) FROM sessions WHERE user_id = ?`, bot.ID); n != 0 {
		t.Errorf("%d sessions minted for a bot", n)
	}
}

func TestTask3392_ResetAndVerificationDoorsAnswerABotAsUnknown(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin-3392r@example.com", "Admin")
	// With email configured, forgot-password and resend would mint a link
	// for any account they accept. The store's mints refuse a bot too, and
	// the door would then still answer 200, so the answer alone cannot tell
	// the two gates apart: the log can. A door that refuses first never
	// attempts the mint.
	sender, _ := newMailSink(t)
	srv.baseURL = "https://app.getpad.dev"
	srv.SetEmailSender(sender)
	logs := captureLogs(t)
	bot := task3392Bot(t, srv, "inst-reset")

	task3392SameAnswer(t, "forgot-password",
		doRequestFromRemoteAddr(srv, "POST", "/api/v1/auth/forgot-password", map[string]string{"email": bot.Email}, fmt.Sprintf("192.0.2.%d:1234", 1)),
		doRequestFromRemoteAddr(srv, "POST", "/api/v1/auth/forgot-password", map[string]string{"email": "nobody-3392@example.com"}, fmt.Sprintf("192.0.2.%d:1234", 2)))
	task3392SameAnswer(t, "local-reset",
		doRequestFromRemoteAddr(srv, "POST", "/api/v1/auth/local-reset", map[string]string{"email": bot.Email}, fmt.Sprintf("127.0.0.%d:1234", 5)),
		doRequestFromRemoteAddr(srv, "POST", "/api/v1/auth/local-reset", map[string]string{"email": "nobody-3392@example.com"}, fmt.Sprintf("127.0.0.%d:1234", 6)))
	task3392SameAnswer(t, "local-reset temp password",
		doRequestFromRemoteAddr(srv, "POST", "/api/v1/auth/local-reset", map[string]any{"email": bot.Email, "temp_password": true}, fmt.Sprintf("127.0.0.%d:1234", 7)),
		doRequestFromRemoteAddr(srv, "POST", "/api/v1/auth/local-reset", map[string]any{"email": "nobody-3392@example.com", "temp_password": true}, fmt.Sprintf("127.0.0.%d:1234", 8)))
	// Unverified, so the resend door would otherwise mint for it.
	if _, err := srv.store.DB().Exec(`UPDATE users SET email_verified_at = NULL WHERE id = ?`, bot.ID); err != nil {
		t.Fatal(err)
	}
	task3392SameAnswer(t, "resend-verification",
		doRequestFromRemoteAddr(srv, "POST", "/api/v1/auth/resend-verification", map[string]string{"email": bot.Email}, fmt.Sprintf("192.0.2.%d:1234", 3)),
		doRequestFromRemoteAddr(srv, "POST", "/api/v1/auth/resend-verification", map[string]string{"email": "nobody-3392@example.com"}, fmt.Sprintf("192.0.2.%d:1234", 4)))
	for _, table := range []string{"password_reset_tokens", "email_verification_tokens"} {
		if n := countRows(t, srv, `SELECT COUNT(*) FROM `+table+` WHERE user_id = ?`, bot.ID); n != 0 {
			t.Errorf("%d %s minted for a bot", n, table)
		}
	}
	if strings.Contains(logs.String(), "failed to create") {
		t.Errorf("a door attempted a link mint for a bot before refusing it:\n%s", logs.String())
	}
	after, err := srv.store.GetUser(bot.ID)
	if err != nil || after == nil {
		t.Fatal(err)
	}
	if bcrypt.CompareHashAndPassword([]byte(after.PasswordHash), []byte(task3392Password)) != nil {
		t.Error("the bot's password was replaced (local-reset temp password)")
	}
}

// Nobody can register, bootstrap with, be invited at, or sign in through a
// provider with an address in the bots' domain.
func TestTask3392_TheReservedDomainIsNoPersonsAddress(t *testing.T) {
	srv := testServer(t)
	task3392SameAnswer(t, "bootstrap",
		doLoopbackRequest(srv, "POST", "/api/v1/auth/bootstrap", map[string]string{"email": "app+x@apps.pad.invalid", "name": "X", "password": task3392Password}),
		doLoopbackRequest(srv, "POST", "/api/v1/auth/bootstrap", map[string]string{"email": "not-an-email", "name": "X", "password": task3392Password}))
	token := bootstrapFirstUser(t, srv, "owner-3392@example.com", "Owner")
	task3392SameAnswer(t, "register",
		doRequestWithCookie(srv, "POST", "/api/v1/auth/register", map[string]string{"email": "app+x@apps.pad.invalid", "name": "X", "password": task3392Password}, token),
		doRequestWithCookie(srv, "POST", "/api/v1/auth/register", map[string]string{"email": "not-an-email", "name": "X", "password": task3392Password}, token))
	ws := createWSForTestWithCookie(t, srv, token)
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws+"/members/invite", map[string]string{"email": "app+x@apps.pad.invalid", "role": "editor"}, token)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("invite at the reserved domain: %d %s, want 400", rr.Code, rr.Body.String())
	}
	if n := countRows(t, srv, `SELECT COUNT(*) FROM workspace_invitations WHERE email LIKE '%apps.pad.invalid'`); n != 0 {
		t.Errorf("%d invitations to the reserved domain", n)
	}
}

// A 2FA challenge minted before the row became a bot mints nothing after.
func TestTask3392_TOTPVerifyRefusesABot(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)
	userID, secret := linkedTOTPUser(t, srv, "totp-3392@example.com", true)
	body := oauthLoginBody("totp-3392@example.com")
	body["cloud_secret"] = oauthProviderTestSecret
	rr := doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/oauth-login", body,
		map[string]string{"X-Cloud-Secret": oauthProviderTestSecret}, "198.51.100.7:1")
	var b bug3322ErrorBody
	_ = json.Unmarshal(rr.Body.Bytes(), &b)
	if b.Error.Details.ChallengeToken == "" {
		t.Fatalf("no challenge minted (%d): %s", rr.Code, rr.Body.String())
	}
	task3392SetKind(t, srv, userID, models.UserKindApp)
	before := countRows(t, srv, `SELECT COUNT(*) FROM sessions WHERE user_id = ?`, userID)
	vr := doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/2fa/login-verify", map[string]any{
		"challenge_token": b.Error.Details.ChallengeToken,
		"code":            validTOTPCode(t, secret),
	}, nil, "198.51.100.7:1")
	if vr.Code != http.StatusUnauthorized || errorCode(t, vr) != "unauthorized" {
		t.Fatalf("2FA verify for a bot: %d %s, want the 401 an unknown account gets", vr.Code, vr.Body.String())
	}
	if n := countRows(t, srv, `SELECT COUNT(*) FROM sessions WHERE user_id = ?`, userID); n != before {
		t.Fatalf("sessions %d -> %d: 2FA verify minted one for a bot", before, n)
	}
}

// The token endpoint refuses a refresh whose subject is a bot, with its own
// invalid_grant (a 500 would mean only the store's gate caught it).
func TestTask3392_TokenEndpointRefusesABotSubject(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	sess := newOAuthSession(t, srv)
	if _, code := mintWithResource(t, srv, sess, testCanonicalAudience); code != http.StatusOK {
		t.Fatalf("mint: %d", code)
	}
	refresh, client := lastRefresh, lastClientID
	user, _ := srv.store.GetUserByEmail("oauth-test@example.com")
	task3392SetKind(t, srv, user.ID, models.UserKindApp)
	rr := postOAuthForm(srv, "/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {client},
	})
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid_grant") {
		t.Fatalf("a bot subject's refresh exchange: %d %s, want 400 invalid_grant", rr.Code, rr.Body.String())
	}
}

// Consent, CLI approval and invitation acceptance: a session whose row became
// a bot is no signed-in person. The session resolution refuses it first; the
// doors' own checks stand behind it.
func TestTask3392_SessionDoorsTreatABotAsSignedOut(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	sess := newOAuthSession(t, srv)
	user, _ := srv.store.GetUserByEmail("oauth-test@example.com")
	q := url.Values{
		"client_id": {sess.clientID}, "response_type": {"code"}, "redirect_uri": {"https://app.test/cb"},
		"scope": {"pad:read"}, "code_challenge": {s256Challenge("verifier-3392-abcdefghijklmnopqrstuvwxyz0123456789")},
		"code_challenge_method": {"S256"}, "state": {"state-3392-01"}, "resource": {testCanonicalAudience},
	}
	if rr := doRequestWithCookie(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sess.sessionToken); rr.Code != http.StatusOK {
		t.Fatalf("fixture: consent does not render for a person (%d)", rr.Code)
	}
	cli := doRequest(srv, "POST", "/api/v1/auth/cli/sessions", nil)
	var cliResp struct {
		SessionCode string `json:"session_code"`
	}
	parseJSON(t, cli, &cliResp)
	if cliResp.SessionCode == "" {
		t.Fatalf("no CLI session: %s", cli.Body.String())
	}

	task3392SetKind(t, srv, user.ID, models.UserKindApp)

	rr := doRequestWithCookie(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sess.sessionToken)
	if rr.Code != http.StatusFound || !strings.HasPrefix(rr.Header().Get("Location"), "/login") {
		t.Errorf("authorize as a bot: %d → %q, want a redirect to sign in", rr.Code, rr.Header().Get("Location"))
	}
	if _, code := mintWithResource(t, srv, sess, testCanonicalAudience); code == http.StatusOK {
		t.Error("a bot's session completed an OAuth grant")
	}
	if rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/cli/sessions/"+cliResp.SessionCode+"/approve", nil, sess.sessionToken); rr.Code != http.StatusUnauthorized {
		t.Errorf("CLI approve as a bot: %d %s, want 401", rr.Code, rr.Body.String())
	}
	if rr := doRequestWithCookie(srv, "POST", "/api/v1/invitations/0123456789abcdef0123456789abcdef/accept", nil, sess.sessionToken); rr.Code == http.StatusOK {
		t.Errorf("invitation accept as a bot: %d", rr.Code)
	}
	if rr := doRequestWithCookie(srv, "GET", "/api/v1/auth/me", nil, sess.sessionToken); rr.Code != http.StatusUnauthorized {
		t.Errorf("/auth/me as a bot: %d, want 401", rr.Code)
	}
}

func TestTask3392_AdminRoutesAnswerABotAsUnknown(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "admin-3392a@example.com", "Admin")
	bot := task3392Bot(t, srv, "inst-admin")
	list := doRequestWithCookie(srv, "GET", "/api/v1/admin/users", nil, token)
	if strings.Contains(list.Body.String(), bot.ID) {
		t.Error("the admin user list carries the bot")
	}
	unknown := "00000000-0000-0000-0000-000000000000"
	for _, rt := range []struct{ method, suffix string }{
		{"GET", ""}, {"PATCH", ""}, {"POST", "/reset-password"}, {"GET", "/detail"},
		{"GET", "/activity"}, {"GET", "/metrics"}, {"POST", "/disable"}, {"POST", "/enable"}, {"POST", "/verify-email"},
	} {
		body := map[string]any{}
		if rt.method == "PATCH" {
			body = map[string]any{"role": "admin"}
		}
		task3392SameAnswer(t, rt.method+" /admin/users/{id}"+rt.suffix,
			doRequestWithCookie(srv, rt.method, "/api/v1/admin/users/"+bot.ID+rt.suffix, body, token),
			doRequestWithCookie(srv, rt.method, "/api/v1/admin/users/"+unknown+rt.suffix, body, token))
	}
	if rr := doRequestWithCookie(srv, "GET", "/api/v1/admin/users/"+bot.ID+"/workspaces", nil, token); rr.Code != http.StatusNotFound {
		t.Errorf("GET /admin/users/{bot}/workspaces: %d, want 404", rr.Code)
	}
	after, _ := srv.store.GetUser(bot.ID)
	if after == nil || after.Role == "admin" || after.IsDisabled() {
		t.Errorf("an admin route changed the bot: %+v", after)
	}
}

func TestTask3392_MembershipOfABotBelongsToItsInstall(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "owner-3392m@example.com", "Owner")
	slug := createWSForTestWithCookie(t, srv, token)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatal(err)
	}
	if _, err := srv.store.DB().Exec(`INSERT INTO app_installs (id, workspace_id, origin, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"inst-member", ws.ID, "https://portal.example", time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	bot := task3392Bot(t, srv, "inst-member")
	if _, err := srv.store.DB().Exec(`INSERT INTO workspace_members (workspace_id, user_id, role, created_at) VALUES (?, ?, 'editor', ?)`,
		ws.ID, bot.ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	for _, rt := range []struct {
		method string
		body   any
	}{{"PATCH", map[string]string{"role": "viewer"}}, {"PATCH", map[string]string{"role": "owner"}}, {"DELETE", nil}} {
		rr := doRequestWithCookie(srv, rt.method, "/api/v1/workspaces/"+slug+"/members/"+bot.ID, rt.body, token)
		if rr.Code != http.StatusConflict || errorCode(t, rr) != "app_principal" {
			t.Errorf("%s member bot: %d %s, want 409 app_principal", rt.method, rr.Code, rr.Body.String())
		}
	}
	rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+slug+"/members", nil, token)
	var resp struct {
		Members []models.WorkspaceMember      `json:"members"`
		Apps    []store.WorkspaceAppPrincipal `json:"apps"`
	}
	parseJSON(t, rr, &resp)
	for _, m := range resp.Members {
		if m.UserID == bot.ID {
			t.Error("the bot is listed in members")
		}
	}
	if len(resp.Apps) != 1 || resp.Apps[0].UserID != bot.ID || resp.Apps[0].AppName != "https://portal.example" || resp.Apps[0].DisplayName != "Support Portal" {
		t.Errorf("apps = %+v (body %s)", resp.Apps, rr.Body.String())
	}
}

func createWSForTestWithCookie(t *testing.T, srv *Server, token string) string {
	t.Helper()
	rr := createWorkspaceAs(t, srv, token, "Bots 3392")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws struct {
		Slug string `json:"slug"`
	}
	parseJSON(t, rr, &ws)
	return ws.Slug
}

// The doors' own checks, behind the session backstop: each handler is called
// with the bot ALREADY resolved as the current user, which no real request
// can produce (ValidateSession refuses a bot first). This pins the second
// layer the lead ruled for, so removing either layer alone is caught.
func TestTask3392_SessionDoorsOwnChecksRefuseABot(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	sess := newOAuthSession(t, srv)
	user, _ := srv.store.GetUserByEmail("oauth-test@example.com")
	task3392SetKind(t, srv, user.ID, models.UserKindApp)
	bot, _ := srv.store.GetUser(user.ID)
	if !bot.IsApp() {
		t.Fatal("fixture: the row is not a bot")
	}
	asBot := func(r *http.Request, params map[string]string) *http.Request {
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
		return r.WithContext(WithCurrentUser(ctx, bot))
	}

	// CLI approve.
	cli := doRequest(srv, "POST", "/api/v1/auth/cli/sessions", nil)
	var cliResp struct {
		SessionCode string `json:"session_code"`
	}
	parseJSON(t, cli, &cliResp)
	rr := httptest.NewRecorder()
	srv.handleApproveCLIAuthSession(rr, asBot(httptest.NewRequest("POST", "/", nil), map[string]string{"code": cliResp.SessionCode}))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("CLI approve with a bot as the current user: %d %s, want 401", rr.Code, rr.Body.String())
	}

	// Invitation accept, both doors. The invitation is to the bot row's own
	// address, so only the kind can refuse it.
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Inv 3392"})
	if err != nil {
		t.Fatal(err)
	}
	inviter, err := srv.store.CreateUser(models.UserCreate{Email: "inviter-3392@example.com", Name: "I", Password: task3392Password})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := srv.store.CreateInvitation(ws.ID, bot.Email, "editor", inviter.ID)
	if err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	srv.handleAcceptInvitation(rr, asBot(httptest.NewRequest("POST", "/", nil), map[string]string{"code": inv.Code}))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("invitation accept by code as a bot: %d %s, want 401", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	srv.handleAcceptMyInvitation(rr, asBot(httptest.NewRequest("POST", "/", nil), map[string]string{"id": inv.ID}))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("invitation accept by id as a bot: %d %s, want 401", rr.Code, rr.Body.String())
	}
	if n := countRows(t, srv, `SELECT COUNT(*) FROM workspace_members WHERE user_id = ?`, bot.ID); n != 0 {
		t.Errorf("the bot joined %d workspaces", n)
	}

	// OAuth authorize: a bot is signed out, so the answer is the sign-in
	// redirect.
	q := url.Values{
		"client_id": {sess.clientID}, "response_type": {"code"}, "redirect_uri": {"https://app.test/cb"},
		"scope": {"pad:read"}, "code_challenge": {s256Challenge("verifier-3392-abcdefghijklmnopqrstuvwxyz0123456789")},
		"code_challenge_method": {"S256"}, "state": {"state-3392-02"}, "resource": {testCanonicalAudience},
	}
	rr = httptest.NewRecorder()
	srv.handleOAuthAuthorize(rr, asBot(httptest.NewRequest("GET", "/oauth/authorize?"+q.Encode(), nil), nil))
	if rr.Code != http.StatusFound || !strings.HasPrefix(rr.Header().Get("Location"), "/login") {
		t.Errorf("authorize with a bot as the current user: %d → %q, want a redirect to sign in", rr.Code, rr.Header().Get("Location"))
	}
}

// codex r1: an OAuth access token whose subject is a bot authenticates
// nothing at /mcp, and a stored reset token for a bot changes no password.
func TestTask3392_StoredCredentialsAreNoUseToABotOverHTTP(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	sess := newOAuthSession(t, srv)
	tok, _ := mintWithResource(t, srv, sess, testCanonicalAudience)
	user, _ := srv.store.GetUserByEmail("oauth-test@example.com")
	if rr := postMCP(srv, "/mcp", tok); rr.Code == http.StatusUnauthorized {
		t.Fatalf("fixture: the person's grant does not work at /mcp (%d)", rr.Code)
	}
	task3392SetKind(t, srv, user.ID, models.UserKindApp)
	if rr := postMCP(srv, "/mcp", tok); rr.Code != http.StatusUnauthorized {
		t.Errorf("a bot subject's OAuth grant at /mcp: %d, want 401", rr.Code)
	}

	bot := task3392Bot(t, srv, "inst-reset-consume")
	before, _ := srv.store.GetUser(bot.ID)
	sum := sha256.Sum256([]byte("padres_bot_http"))
	if _, err := srv.store.DB().Exec(`INSERT INTO password_reset_tokens (id, user_id, token_hash, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		"rst-3392", bot.ID, hex.EncodeToString(sum[:]), time.Now().UTC().Add(time.Hour).Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	task3392SameAnswer(t, "reset-password",
		doRequestFromRemoteAddr(srv, "POST", "/api/v1/auth/reset-password", map[string]string{"token": "padres_bot_http", "password": "a-new-password-3392"}, "192.0.2.50:1"),
		doRequestFromRemoteAddr(srv, "POST", "/api/v1/auth/reset-password", map[string]string{"token": "padres_nonexistent", "password": "a-new-password-3392"}, "192.0.2.51:1"))
	after, _ := srv.store.GetUser(bot.ID)
	if after.PasswordHash != before.PasswordHash || after.PasswordSet != before.PasswordSet {
		t.Error("a stored reset token changed a bot's password")
	}
}
