package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/email"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4745: an OAuth sign-in refused as not linked keeps the provider
// identity behind a single-use token (the browser's __Host-pad_pending_link
// cookie, set by pad-cloud). The account's next full sign-in in that browser
// links the provider, with consent, and the owner is emailed.

const pl4745Password = "a-Strong-Passphrase-For-4745!"

var pl4745TokenRe = regexp.MustCompile(`^padpl_[0-9a-f]{64}$`)

type pl4745Env struct {
	srv   *Server
	mails chan capturedMail
}

func newPL4745(t *testing.T) pl4745Env {
	t.Helper()
	srv := testServer(t)
	sender, mails := newMailSink(t)
	srv.baseURL = "https://app.getpad.dev"
	srv.SetEmailSender(sender)
	srv.SetCloudMode(oauthProviderTestSecret)
	return pl4745Env{srv, mails}
}

// account creates a verified password account with the given providers
// linked (each bound to "<provider>-<email>").
func (e pl4745Env) account(t *testing.T, addr string, providers ...string) *models.User {
	t.Helper()
	u, err := e.srv.store.CreateUser(models.UserCreate{Email: addr, Name: "Owner", Password: pl4745Password})
	if err != nil {
		t.Fatal(err)
	}
	if !u.IsEmailVerified() {
		if err := e.srv.store.SetUserEmailVerified(u.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range providers {
		if err := e.srv.store.LinkOAuthProvider(u.ID, p, p+"-"+addr); err != nil {
			t.Fatal(err)
		}
	}
	return u
}

func pl4745OAuthBody(provider, addr, subject string, pending bool) map[string]interface{} {
	b := map[string]interface{}{
		"provider": provider, "email": addr, "name": "Owner", "email_verified": true, "subject": subject,
	}
	if provider == "github" {
		b["email_primary"] = true
	}
	if pending {
		b["pending_link"] = true
	}
	return b
}

type pl4745Refusal struct {
	Error struct {
		Code    string                 `json:"code"`
		Message string                 `json:"message"`
		Details map[string]interface{} `json:"details"`
	} `json:"error"`
	Token string `json:"token"`
}

// refuse makes the not-linked refusal the sidecar sees and returns the
// pending-link token it carries ("" when none).
func (e pl4745Env) refuse(t *testing.T, provider, addr, subject string, pending bool) string {
	t.Helper()
	rr := postOAuthLogin(t, e.srv, pl4745OAuthBody(provider, addr, subject, pending))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("not-linked oauth-login: %d %s", rr.Code, rr.Body.String())
	}
	var ref pl4745Refusal
	parseJSON(t, rr, &ref)
	if ref.Error.Code != "oauth_provider_not_linked" || ref.Token != "" {
		t.Fatalf("refusal: %+v", ref)
	}
	tok, _ := ref.Error.Details["pending_link_token"].(string)
	return tok
}

// browser sends a request as the browser would, with the pending-link
// cookie when tok is set.
func (e pl4745Env) browser(method, path string, body interface{}, tok string, extra ...*http.Cookie) *httptest.ResponseRecorder {
	var buf *bytes.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		buf = bytes.NewReader(data)
	} else {
		buf = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = "192.0.2.1:1234"
	if tok != "" {
		req.AddCookie(&http.Cookie{Name: pendingLinkCookieName, Value: tok})
	}
	for _, c := range extra {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	e.srv.ServeHTTP(rr, req)
	return rr
}

func (e pl4745Env) login(t *testing.T, addr, tok string, linkPending *bool) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]interface{}{"email": addr, "password": pl4745Password}
	if linkPending != nil {
		body["link_pending"] = *linkPending
	}
	return e.browser("POST", "/api/v1/auth/login", body, tok)
}

var pl4745Yes, pl4745No = ptr(true), ptr(false)

type pl4745Reply struct {
	Token       string                 `json:"token"`
	PendingLink map[string]interface{} `json:"pending_link"`
}

func pl4745Read(t *testing.T, rr *httptest.ResponseRecorder) pl4745Reply {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("sign-in: %d %s", rr.Code, rr.Body.String())
	}
	var out pl4745Reply
	parseJSON(t, rr, &out)
	if out.Token == "" {
		t.Fatalf("sign-in minted no session: %s", rr.Body.String())
	}
	return out
}

func (e pl4745Env) linked(t *testing.T, userID, provider string) bool {
	t.Helper()
	u, err := e.srv.store.GetUser(userID)
	if err != nil || u == nil {
		t.Fatalf("GetUser: %v", err)
	}
	return u.HasOAuthProvider(provider)
}

func pl4745WantLinked(t *testing.T, got map[string]interface{}, provider string) {
	t.Helper()
	if got == nil || got["linked"] != true || got["provider"] != provider || len(got) != 2 {
		t.Fatalf("pending_link = %v, want {provider: %s, linked: true}", got, provider)
	}
}

func pl4745WantNotLinked(t *testing.T, got map[string]interface{}, code string) {
	t.Helper()
	if got == nil || got["linked"] != false || got["code"] != code || len(got) != 2 {
		t.Fatalf("pending_link = %v, want {linked: false, code: %s}", got, code)
	}
}

func pl4745CookieCleared(rr *httptest.ResponseRecorder) bool {
	for _, c := range rr.Result().Cookies() {
		if c.Name == pendingLinkCookieName && c.MaxAge < 0 && c.Value == "" && c.Secure && c.HttpOnly && c.Path == "/" {
			return true
		}
	}
	return false
}

func (e pl4745Env) liveTokens(t *testing.T, userID string) int {
	t.Helper()
	return count(t, e.srv, `SELECT COUNT(*) FROM oauth_pending_links WHERE user_id = ? AND used_at IS NULL`, userID)
}

// noMail fails if an email arrives within a short window.
func noMail(t *testing.T, ch chan capturedMail) {
	t.Helper()
	select {
	case m := <-ch:
		t.Fatalf("unexpected email to %s: %s", m.to, m.subject)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestTASK4745_IssuesTokenOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "ask@example.com")

	// An older sidecar does not ask: the refusal is unchanged and nothing is held.
	rr := postOAuthLogin(t, e.srv, pl4745OAuthBody("google", "ask@example.com", "g-ask", false))
	var plain pl4745Refusal
	parseJSON(t, rr, &plain)
	if rr.Code != http.StatusForbidden || plain.Error.Code != "oauth_provider_not_linked" || plain.Error.Details != nil {
		t.Fatalf("unasked refusal: %d %s", rr.Code, rr.Body.String())
	}
	if n := e.liveTokens(t, u.ID); n != 0 {
		t.Fatalf("an unasked refusal held %d tokens", n)
	}

	tok := e.refuse(t, "google", "ask@example.com", "g-ask", true)
	if !pl4745TokenRe.MatchString(tok) {
		t.Fatalf("pending_link_token = %q", tok)
	}
	rr = postOAuthLogin(t, e.srv, pl4745OAuthBody("google", "ask@example.com", "g-ask", true))
	var asked pl4745Refusal
	parseJSON(t, rr, &asked)
	if asked.Error.Details["provider"] != "google" || len(asked.Error.Details) != 2 || asked.Error.Message != plain.Error.Message {
		t.Fatalf("asked refusal: %+v", asked.Error)
	}
	// The plaintext is not what the table holds.
	if n := count(t, e.srv, `SELECT COUNT(*) FROM oauth_pending_links WHERE token_hash = ?`, tok); n != 0 {
		t.Fatal("the plaintext token is stored")
	}
	if e.linked(t, u.ID, "google") {
		t.Fatal("the refusal linked the provider")
	}
}

func TestTASK4745_PasswordSignInLinks(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "pw@example.com")
	tok := e.refuse(t, "google", "pw@example.com", "g-pw", true)

	rr := e.login(t, "pw@example.com", tok, pl4745Yes)
	out := pl4745Read(t, rr)
	pl4745WantLinked(t, out.PendingLink, "google")
	if !pl4745CookieCleared(rr) {
		t.Error("the sign-in did not clear the pending-link cookie")
	}
	if !e.linked(t, u.ID, "google") || oauthIdentityRows(t, e.srv, "google", "g-pw") != 1 {
		t.Fatal("google not linked and bound")
	}
	// Audited as a link, via the pending link, and the token is nowhere in it.
	if n := count(t, e.srv, `SELECT COUNT(*) FROM activities WHERE action = ? AND user_id = ? AND metadata LIKE '%"via":"pending_link"%' AND metadata LIKE '%"action":"link_provider"%'`,
		models.ActionOAuthLogin, u.ID); n != 1 {
		t.Errorf("link audit rows = %d, want 1", n)
	}
	if n := count(t, e.srv, `SELECT COUNT(*) FROM activities WHERE metadata LIKE '%padpl_%'`); n != 0 {
		t.Errorf("%d audit rows carry the token", n)
	}
	// Next time the provider signs straight in.
	if rr := postOAuthLogin(t, e.srv, pl4745OAuthBody("google", "pw@example.com", "g-pw", true)); rr.Code != http.StatusOK {
		t.Fatalf("google sign-in after the link: %d %s", rr.Code, rr.Body.String())
	}
}

func TestTASK4745_SingleUse(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "once@example.com")
	tok := e.refuse(t, "google", "once@example.com", "g-once", true)
	pl4745WantLinked(t, pl4745Read(t, e.login(t, "once@example.com", tok, pl4745Yes)).PendingLink, "google")
	// Unlinked again, the same token links nothing.
	if err := e.srv.store.RemoveOAuthProvider(u.ID, "google"); err != nil {
		t.Fatal(err)
	}
	pl4745WantNotLinked(t, pl4745Read(t, e.login(t, "once@example.com", tok, pl4745Yes)).PendingLink, pendingLinkCodeInvalid)
	if e.linked(t, u.ID, "google") {
		t.Fatal("a spent token linked again")
	}
}

func TestTASK4745_Expired(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "late@example.com")
	tok := e.refuse(t, "google", "late@example.com", "g-late", true)
	if _, err := e.srv.store.DB().Exec(`UPDATE oauth_pending_links SET expires_at = ?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	rr := e.login(t, "late@example.com", tok, pl4745Yes)
	pl4745WantNotLinked(t, pl4745Read(t, rr).PendingLink, pendingLinkCodeInvalid)
	if !pl4745CookieCleared(rr) || e.linked(t, u.ID, "google") {
		t.Fatal("an expired token linked, or its cookie survived")
	}
}

func TestTASK4745_DifferentAccountInert(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	victim := e.account(t, "victim@example.com")
	other := e.account(t, "other@example.com")
	tok := e.refuse(t, "google", "victim@example.com", "g-victim", true)

	rr := e.login(t, "other@example.com", tok, pl4745Yes)
	pl4745WantNotLinked(t, pl4745Read(t, rr).PendingLink, pendingLinkCodeInvalid)
	if !pl4745CookieCleared(rr) {
		t.Error("the cookie survived another account's sign-in")
	}
	if e.linked(t, other.ID, "google") || e.linked(t, victim.ID, "google") {
		t.Fatal("another account's sign-in linked the provider")
	}
	// Burned: the right account cannot use it afterwards.
	pl4745WantNotLinked(t, pl4745Read(t, e.login(t, "victim@example.com", tok, pl4745Yes)).PendingLink, pendingLinkCodeInvalid)
	if e.linked(t, victim.ID, "google") {
		t.Fatal("a burned token linked")
	}
}

func TestTASK4745_NoConsentNoLink(t *testing.T) {
	t.Parallel()
	for name, consent := range map[string]*bool{"absent": nil, "false": pl4745No} {
		t.Run(name, func(t *testing.T) {
			e := newPL4745(t)
			u := e.account(t, "noconsent@example.com")
			tok := e.refuse(t, "google", "noconsent@example.com", "g-nc", true)
			rr := e.login(t, "noconsent@example.com", tok, consent)
			pl4745WantNotLinked(t, pl4745Read(t, rr).PendingLink, pendingLinkCodeNotRequested)
			if e.linked(t, u.ID, "google") || e.liveTokens(t, u.ID) != 0 || !pl4745CookieCleared(rr) {
				t.Fatal("a sign-in without consent linked, or left the token live")
			}
		})
	}
}

func TestTASK4745_TwoFactorLinksOnlyAfterCode(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "2fa@example.com")
	totpSecret := enableTOTPForDeleteUser(t, e.srv, u.ID)
	tok := e.refuse(t, "google", "2fa@example.com", "g-2fa", true)

	rr := e.login(t, "2fa@example.com", tok, pl4745Yes)
	var first struct {
		Requires2FA    bool                   `json:"requires_2fa"`
		ChallengeToken string                 `json:"challenge_token"`
		PendingLink    map[string]interface{} `json:"pending_link"`
	}
	parseJSON(t, rr, &first)
	if rr.Code != http.StatusOK || !first.Requires2FA || first.ChallengeToken == "" || first.PendingLink != nil {
		t.Fatalf("password step: %d %s", rr.Code, rr.Body.String())
	}
	if pl4745CookieCleared(rr) || e.liveTokens(t, u.ID) != 1 || e.linked(t, u.ID, "google") {
		t.Fatal("the first factor alone spent or used the token")
	}
	// A wrong code links nothing either.
	rr = e.browser("POST", "/api/v1/auth/2fa/login-verify", map[string]interface{}{
		"challenge_token": first.ChallengeToken, "code": "000000", "link_pending": true,
	}, tok)
	if rr.Code == http.StatusOK || e.liveTokens(t, u.ID) != 1 || e.linked(t, u.ID, "google") {
		t.Fatalf("a wrong code: %d, live=%d", rr.Code, e.liveTokens(t, u.ID))
	}
	rr = e.browser("POST", "/api/v1/auth/2fa/login-verify", map[string]interface{}{
		"challenge_token": first.ChallengeToken, "code": validTOTPCode(t, totpSecret), "link_pending": true,
	}, tok)
	pl4745WantLinked(t, pl4745Read(t, rr).PendingLink, "google")
	if !pl4745CookieCleared(rr) || !e.linked(t, u.ID, "google") {
		t.Fatal("the code step did not link and clear")
	}
}

func TestTASK4745_UnverifiedNoToken(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u, err := e.srv.store.CreateUser(models.UserCreate{Email: "unv@example.com", Name: "Unv", Password: pl4745Password, Unverified: true})
	if err != nil {
		t.Fatal(err)
	}
	// GitHub not vouching for the primary address: no claim, and no token.
	body := pl4745OAuthBody("github", "unv@example.com", "gh-unv", true)
	body["email_primary"] = false
	rr := postOAuthLogin(t, e.srv, body)
	var ref pl4745Refusal
	parseJSON(t, rr, &ref)
	if rr.Code != http.StatusForbidden || ref.Error.Code != "oauth_provider_not_linked" || ref.Error.Details != nil {
		t.Fatalf("unverified account: %d %s", rr.Code, rr.Body.String())
	}
	// Verified, but GitHub says the address is not the primary one: no token.
	v := e.account(t, "secondary@example.com")
	body = pl4745OAuthBody("github", "secondary@example.com", "gh-sec", true)
	body["email_primary"] = false
	if tok := func() string {
		rr := postOAuthLogin(t, e.srv, body)
		var ref pl4745Refusal
		parseJSON(t, rr, &ref)
		s, _ := ref.Error.Details["pending_link_token"].(string)
		return s
	}(); tok != "" {
		t.Fatal("a non-primary GitHub address got a token")
	}
	if n := count(t, e.srv, `SELECT COUNT(*) FROM oauth_pending_links WHERE user_id IN (?, ?)`, u.ID, v.ID); n != 0 {
		t.Fatalf("%d tokens held", n)
	}
	// The claim path is unchanged: Google vouching claims the unverified account.
	rr = postOAuthLogin(t, e.srv, pl4745OAuthBody("google", "unv@example.com", "g-unv", true))
	var claimed struct {
		Claimed bool `json:"claimed"`
	}
	parseJSON(t, rr, &claimed)
	if rr.Code != http.StatusOK || !claimed.Claimed {
		t.Fatalf("claim: %d %s", rr.Code, rr.Body.String())
	}
}

func TestTASK4745_SubjectBoundElsewhereAtConsume(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "bound@example.com")
	tok := e.refuse(t, "google", "bound@example.com", "g-bound-subject", true)
	// Meanwhile the same Google account is bound to another Pad account.
	other := e.account(t, "elsewhere@example.com")
	if err := e.srv.store.LinkOAuthProvider(other.ID, "google", "g-bound-subject"); err != nil {
		t.Fatal(err)
	}
	rr := e.login(t, "bound@example.com", tok, pl4745Yes)
	pl4745WantNotLinked(t, pl4745Read(t, rr).PendingLink, pendingLinkCodeSubjectBound)
	if strings.Contains(rr.Body.String(), "g-bound-subject") {
		t.Fatal("the reply carries the subject")
	}
	if e.linked(t, u.ID, "google") {
		t.Fatal("linked a provider account bound elsewhere")
	}
	if n := count(t, e.srv, `SELECT COUNT(*) FROM activities WHERE action = ? AND user_id = ? AND metadata LIKE '%subject_mismatch%'`,
		models.ActionOAuthLoginFailed, u.ID); n != 1 {
		t.Errorf("mismatch audit rows = %d", n)
	}
}

func TestTASK4745_EmailChangedNoLink(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "before@example.com")
	tok := e.refuse(t, "google", "before@example.com", "g-moved", true)
	if _, err := e.srv.store.DB().Exec(`UPDATE users SET email = ? WHERE id = ?`, "after@example.com", u.ID); err != nil {
		t.Fatal(err)
	}
	// Still pending for the page? No: the address it was asserted for is gone.
	if body := e.browser("GET", "/api/v1/auth/pending-link", nil, tok).Body.String(); body != "{\"pending\":false}\n" {
		t.Fatalf("peek after the address moved: %s", body)
	}
	pl4745WantNotLinked(t, pl4745Read(t, e.login(t, "after@example.com", tok, pl4745Yes)).PendingLink, pendingLinkCodeIneligible)
	if e.linked(t, u.ID, "google") {
		t.Fatal("linked after the address changed")
	}
}

func TestTASK4745_PeekIdenticalWhenAbsent(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "peek@example.com")
	spent := e.refuse(t, "google", "peek@example.com", "g-peek", true)
	pl4745Read(t, e.login(t, "peek@example.com", spent, pl4745No))
	expired := e.refuse(t, "google", "peek@example.com", "g-peek", true)
	if _, err := e.srv.store.DB().Exec(`UPDATE oauth_pending_links SET expires_at = ? WHERE used_at IS NULL`, "2000-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	disabledFor := e.account(t, "gone@example.com")
	disabledTok := e.refuse(t, "google", "gone@example.com", "g-gone", true)
	if err := e.srv.store.DisableUserAndRevokeAccess(disabledFor.ID); err != nil {
		t.Fatal(err)
	}
	_ = u

	var want *httptest.ResponseRecorder
	for name, tok := range map[string]string{
		"no cookie": "", "garbage": "padpl_" + strings.Repeat("0", 64), "spent": spent,
		"expired": expired, "disabled account": disabledTok,
	} {
		rr := e.browser("GET", "/api/v1/auth/pending-link", nil, tok)
		if rr.Code != http.StatusOK || rr.Body.String() != "{\"pending\":false}\n" {
			t.Fatalf("%s: %d %q", name, rr.Code, rr.Body.String())
		}
		if want == nil {
			want = rr
			continue
		}
		for _, h := range []string{"Cache-Control", "Content-Type", "Set-Cookie", "Content-Length"} {
			if rr.Header().Get(h) != want.Header().Get(h) {
				t.Errorf("%s: header %s = %q, want %q", name, h, rr.Header().Get(h), want.Header().Get(h))
			}
		}
	}
}

func TestTASK4745_OtherProviderOAuthLoginConsumes(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "multi@example.com", "github")
	tok := e.refuse(t, "google", "multi@example.com", "g-multi", true)

	// The page offers GitHub; pad-cloud forwards the cookie's token.
	body := pl4745OAuthBody("github", "multi@example.com", "github-multi@example.com", false)
	body["pending_link_token"] = tok
	rr := postOAuthLogin(t, e.srv, body)
	pl4745WantLinked(t, pl4745Read(t, rr).PendingLink, "google")
	if !e.linked(t, u.ID, "google") || oauthIdentityRows(t, e.srv, "google", "g-multi") != 1 {
		t.Fatal("the other provider's sign-in did not link")
	}
	waitMail(t, e.mails)

	// A 2FA account keeps the token through the provider step.
	v := e.account(t, "multi2fa@example.com", "github")
	enableTOTPForDeleteUser(t, e.srv, v.ID)
	tok2 := e.refuse(t, "google", "multi2fa@example.com", "g-multi2", true)
	body = pl4745OAuthBody("github", "multi2fa@example.com", "github-multi2fa@example.com", false)
	body["pending_link_token"] = tok2
	if rr := postOAuthLogin(t, e.srv, body); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "two_factor_required") {
		t.Fatalf("2FA provider step: %d %s", rr.Code, rr.Body.String())
	}
	if e.liveTokens(t, v.ID) != 1 {
		t.Fatal("the provider step spent the token before the code")
	}
}

func TestTASK4745_SelfHostInert(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	e := pl4745Env{srv: srv}
	u := e.account(t, "self@example.com")
	tok, err := srv.store.CreateOAuthPendingLink(u.ID, "google", "g-self", "self@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if rr := e.browser("GET", "/api/v1/auth/pending-link", nil, tok); rr.Code != http.StatusNotFound {
		t.Fatalf("self-host pending-link: %d", rr.Code)
	}
	rr := e.login(t, "self@example.com", tok, pl4745Yes)
	if out := pl4745Read(t, rr); out.PendingLink != nil {
		t.Fatalf("self-host reply carries pending_link: %v", out.PendingLink)
	}
	if pl4745CookieCleared(rr) || e.linked(t, u.ID, "google") || e.liveTokens(t, u.ID) != 1 {
		t.Fatal("self-host acted on a pending link")
	}
}

func TestTASK4745_NewTokenInvalidatesOld(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "twice@example.com")
	old := e.refuse(t, "google", "twice@example.com", "g-twice", true)
	newer := e.refuse(t, "google", "twice@example.com", "g-twice", true)
	if old == newer || e.liveTokens(t, u.ID) != 1 {
		t.Fatalf("live tokens = %d", e.liveTokens(t, u.ID))
	}
	pl4745WantNotLinked(t, pl4745Read(t, e.login(t, "twice@example.com", old, pl4745Yes)).PendingLink, pendingLinkCodeInvalid)
	pl4745WantLinked(t, pl4745Read(t, e.login(t, "twice@example.com", newer, pl4745Yes)).PendingLink, "google")
}

func TestTASK4745_ProviderLinkedEmailSent(t *testing.T) {
	t.Parallel()
	wantMail := func(t *testing.T, m capturedMail, to, label string) {
		t.Helper()
		if m.to != to || !strings.Contains(m.subject, label+" was linked") ||
			!strings.Contains(m.body, "unlink "+label+" from your account settings") || !strings.Contains(m.body, "change your password") ||
			!strings.Contains(m.body, "UTC") {
			t.Fatalf("provider-linked mail: to=%s subject=%q body=%q", m.to, m.subject, m.body)
		}
		if strings.Contains(m.body, "padpl_") {
			t.Fatal("the mail carries the token")
		}
	}
	t.Run("pending link", func(t *testing.T) {
		e := newPL4745(t)
		e.account(t, "mailpl@example.com")
		tok := e.refuse(t, "google", "mailpl@example.com", "g-mailpl", true)
		noMail(t, e.mails)
		pl4745WantLinked(t, pl4745Read(t, e.login(t, "mailpl@example.com", tok, pl4745Yes)).PendingLink, "google")
		wantMail(t, waitMail(t, e.mails), "mailpl@example.com", "Google")
	})
	t.Run("oauth-link", func(t *testing.T) {
		e := newPL4745(t)
		e.account(t, "maillink@example.com")
		req := cloudAdminReq(t, "POST", "/api/v1/auth/oauth-link", map[string]interface{}{
			"provider": "github", "email": "maillink@example.com", "email_verified": true,
			"subject": "gh-maillink", "cloud_secret": oauthProviderTestSecret,
		}, map[string]string{"X-Cloud-Secret": oauthProviderTestSecret})
		rr := httptest.NewRecorder()
		e.srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("oauth-link: %d %s", rr.Code, rr.Body.String())
		}
		wantMail(t, waitMail(t, e.mails), "maillink@example.com", "GitHub")
		// Already linked: no second mail.
		e.srv.ServeHTTP(httptest.NewRecorder(), cloudAdminReq(t, "POST", "/api/v1/auth/oauth-link", map[string]interface{}{
			"provider": "github", "email": "maillink@example.com", "email_verified": true,
			"subject": "gh-maillink", "cloud_secret": oauthProviderTestSecret,
		}, map[string]string{"X-Cloud-Secret": oauthProviderTestSecret}))
		noMail(t, e.mails)
	})
}

func TestTASK4745_LinkSucceedsWhenEmailSendErrors(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	failed := make(chan struct{}, 4)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failed <- struct{}{}
		http.Error(w, `{"success":false}`, http.StatusInternalServerError)
	}))
	t.Cleanup(bad.Close)
	sender := email.NewSender("test-api-key", "noreply@pad.test", "Pad", "http://pad.test")
	sender.SetEndpoint(bad.URL)
	e.srv.SetEmailSender(sender)

	u := e.account(t, "mailfail@example.com")
	tok := e.refuse(t, "google", "mailfail@example.com", "g-mailfail", true)
	pl4745WantLinked(t, pl4745Read(t, e.login(t, "mailfail@example.com", tok, pl4745Yes)).PendingLink, "google")
	if !e.linked(t, u.ID, "google") {
		t.Fatal("not linked")
	}
	select {
	case <-failed:
	case <-time.After(3 * time.Second):
		t.Fatal("the notice was never attempted")
	}
}

func TestTASK4745_PendingLinkPeekNoStoreNoCORS(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	const origin = "https://elsewhere.example"
	e.srv.SetCORSOrigins(origin)
	e.account(t, "cors@example.com")
	tok := e.refuse(t, "google", "cors@example.com", "g-cors", true)

	get := func(path, tok string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "192.0.2.1:1234"
		req.Header.Set("Origin", origin)
		if tok != "" {
			req.AddCookie(&http.Cookie{Name: pendingLinkCookieName, Value: tok})
		}
		rr := httptest.NewRecorder()
		e.srv.ServeHTTP(rr, req)
		return rr
	}
	// Control: the configured origin gets credentialed CORS on a sibling route.
	if ctl := get("/api/v1/auth/session", ""); ctl.Header().Get("Access-Control-Allow-Origin") != origin {
		t.Fatalf("control: no CORS on /auth/session: %v", ctl.Header())
	}
	for name, tk := range map[string]string{"pending": tok, "absent": ""} {
		rr := get("/api/v1/auth/pending-link", tk)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d", name, rr.Code)
		}
		if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control = %q", name, cc)
		}
		if a := rr.Header().Get("Access-Control-Allow-Origin"); a != "" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q", name, a)
		}
		if a := rr.Header().Get("Access-Control-Allow-Credentials"); a != "" {
			t.Errorf("%s: Access-Control-Allow-Credentials = %q", name, a)
		}
	}
}

func TestTASK4745_PendingLinkReplyKeySet(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "keys@example.com", "github")
	enableTOTPForDeleteUser(t, e.srv, u.ID)
	tok := e.refuse(t, "google", "keys@example.com", "g-keys", true)
	rr := e.browser("GET", "/api/v1/auth/pending-link", nil, tok)
	var got map[string]interface{}
	parseJSON(t, rr, &got)
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "email,password,pending,provider,providers" {
		t.Fatalf("reply keys = %v", keys)
	}
	providers, _ := got["providers"].([]interface{})
	if got["pending"] != true || got["provider"] != "google" || got["email"] != "keys@example.com" || got["password"] != true ||
		len(providers) != 1 || providers[0] != "github" {
		t.Fatalf("reply = %v", got)
	}
	// Peeking does not spend it.
	if e.liveTokens(t, u.ID) != 1 {
		t.Fatal("the peek spent the token")
	}
}
