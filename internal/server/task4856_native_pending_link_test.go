package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type pl4856LogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *pl4856LogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *pl4856LogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (e pl4745Env) rawPost(path string, rawBody []byte, ip string, cookieTok string, extra ...*http.Cookie) *httptest.ResponseRecorder {
	var req *http.Request
	if rawBody != nil {
		req = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(rawBody))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(http.MethodPost, path, nil)
	}
	if ip == "" {
		ip = "192.0.2.1:1234"
	}
	req.RemoteAddr = ip
	if cookieTok != "" {
		req.AddCookie(&http.Cookie{Name: pendingLinkCookieName, Value: cookieTok})
	}
	for _, c := range extra {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	e.srv.ServeHTTP(rr, req)
	return rr
}

func (e pl4745Env) loginBody(t *testing.T, addr, bodyTok string, linkPending *bool, ip, cookieTok string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]interface{}{
		"email":    addr,
		"password": pl4745Password,
	}
	if bodyTok != "" {
		body["pending_link_token"] = bodyTok
	}
	if linkPending != nil {
		body["link_pending"] = *linkPending
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return e.rawPost("/api/v1/auth/login", data, ip, cookieTok)
}

func TestTASK4856_LoginBodyTokenLinks(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "bodylink@example.com")
	tok, err := e.srv.store.CreateOAuthPendingLink(u.ID, "google", "g-body-link", u.Email)
	if err != nil {
		t.Fatal(err)
	}

	rr := e.loginBody(t, "bodylink@example.com", tok, pl4745Yes, "192.0.2.10:1234", "")
	out := pl4745Read(t, rr)
	pl4745WantLinked(t, out.PendingLink, "google")
	if !e.linked(t, u.ID, "google") || oauthIdentityRows(t, e.srv, "google", "g-body-link") != 1 {
		t.Fatal("google not linked and bound")
	}
	if pl4745CookieCleared(rr) {
		t.Error("response cleared pending-link cookie even though request carried none")
	}
	peeked, err := e.srv.store.PeekOAuthPendingLink(tok)
	if err != nil || peeked != nil {
		t.Fatalf("token after link: peek=%v err=%v, want nil", peeked, err)
	}
}

func TestTASK4856_BodyTokenNoConsentSpends(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "noconsent-body@example.com")
	tok, err := e.srv.store.CreateOAuthPendingLink(u.ID, "google", "g-body-nc", u.Email)
	if err != nil {
		t.Fatal(err)
	}

	rr := e.loginBody(t, "noconsent-body@example.com", tok, nil, "192.0.2.11:1234", "")
	out := pl4745Read(t, rr)
	pl4745WantNotLinked(t, out.PendingLink, pendingLinkCodeNotRequested)
	if e.linked(t, u.ID, "google") {
		t.Fatal("google linked without consent")
	}
	peeked, err := e.srv.store.PeekOAuthPendingLink(tok)
	if err != nil || peeked != nil {
		t.Fatalf("token after no-consent login: peek=%v err=%v, want nil", peeked, err)
	}
}

func TestTASK4856_BodyToken2FAWaitsForVerify(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "2fa-body@example.com")
	totpSecret := enableTOTPForDeleteUser(t, e.srv, u.ID)
	tok, err := e.srv.store.CreateOAuthPendingLink(u.ID, "google", "g-body-2fa", u.Email)
	if err != nil {
		t.Fatal(err)
	}

	rr := e.loginBody(t, "2fa-body@example.com", tok, pl4745Yes, "192.0.2.12:1234", "")
	var first struct {
		Requires2FA    bool                   `json:"requires_2fa"`
		ChallengeToken string                 `json:"challenge_token"`
		PendingLink    map[string]interface{} `json:"pending_link"`
	}
	parseJSON(t, rr, &first)
	if rr.Code != http.StatusOK || !first.Requires2FA || first.ChallengeToken == "" || first.PendingLink != nil {
		t.Fatalf("password step: %d %s", rr.Code, rr.Body.String())
	}
	peeked, err := e.srv.store.PeekOAuthPendingLink(tok)
	if err != nil || peeked == nil {
		t.Fatalf("token spent on password step before 2FA: peek=%v err=%v", peeked, err)
	}
	if e.linked(t, u.ID, "google") {
		t.Fatal("linked before 2FA verification")
	}

	verifyBody, err := json.Marshal(map[string]interface{}{
		"challenge_token":    first.ChallengeToken,
		"code":               validTOTPCode(t, totpSecret),
		"link_pending":       true,
		"pending_link_token": tok,
	})
	if err != nil {
		t.Fatal(err)
	}
	rr = e.rawPost("/api/v1/auth/2fa/login-verify", verifyBody, "192.0.2.12:1234", "")
	out := pl4745Read(t, rr)
	pl4745WantLinked(t, out.PendingLink, "google")
	if !e.linked(t, u.ID, "google") || oauthIdentityRows(t, e.srv, "google", "g-body-2fa") != 1 {
		t.Fatal("google not linked after 2FA verify")
	}
}

func TestTASK4856_BodyTokenOtherAccountBurns(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	accountA := e.account(t, "account-a@example.com")
	accountB := e.account(t, "account-b@example.com")
	tok, err := e.srv.store.CreateOAuthPendingLink(accountA.ID, "google", "g-acct-a", accountA.Email)
	if err != nil {
		t.Fatal(err)
	}

	rr := e.loginBody(t, "account-b@example.com", tok, pl4745Yes, "192.0.2.13:1234", "")
	out := pl4745Read(t, rr)
	pl4745WantNotLinked(t, out.PendingLink, pendingLinkCodeInvalid)
	peeked, err := e.srv.store.PeekOAuthPendingLink(tok)
	if err != nil || peeked != nil {
		t.Fatalf("token survived other-account sign-in: peek=%v err=%v", peeked, err)
	}
	if e.linked(t, accountA.ID, "google") || e.linked(t, accountB.ID, "google") {
		t.Fatal("other-account sign-in linked provider")
	}
}

func TestTASK4856_BodyTokenWinsOverCookie(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	// CreateOAuthPendingLink invalidates older tokens per user_id, so minting
	// the cookie token on a second account keeps both tokens live before login.
	bodyOwner := e.account(t, "body-wins@example.com")
	cookieOwner := e.account(t, "cookie-other@example.com")
	cookieTok, err := e.srv.store.CreateOAuthPendingLink(cookieOwner.ID, "github", "gh-cookie-other", cookieOwner.Email)
	if err != nil {
		t.Fatal(err)
	}
	bodyTok, err := e.srv.store.CreateOAuthPendingLink(bodyOwner.ID, "google", "g-body-wins", bodyOwner.Email)
	if err != nil {
		t.Fatal(err)
	}

	if p, err := e.srv.store.PeekOAuthPendingLink(cookieTok); err != nil || p == nil {
		t.Fatalf("cookie token not live before login: peek=%v err=%v", p, err)
	}
	if p, err := e.srv.store.PeekOAuthPendingLink(bodyTok); err != nil || p == nil {
		t.Fatalf("body token not live before login: peek=%v err=%v", p, err)
	}

	rr := e.loginBody(t, "body-wins@example.com", bodyTok, pl4745Yes, "192.0.2.14:1234", cookieTok)
	out := pl4745Read(t, rr)
	pl4745WantLinked(t, out.PendingLink, "google")
	if !e.linked(t, bodyOwner.ID, "google") {
		t.Fatal("body token did not link google")
	}
	if e.linked(t, bodyOwner.ID, "github") || e.linked(t, cookieOwner.ID, "github") {
		t.Fatal("cookie token should not have linked")
	}
	if p, err := e.srv.store.PeekOAuthPendingLink(bodyTok); err != nil || p != nil {
		t.Fatalf("body token should be spent: peek=%v err=%v", p, err)
	}
	if p, err := e.srv.store.PeekOAuthPendingLink(cookieTok); err != nil || p == nil {
		t.Fatalf("cookie token must remain live (not consumed): peek=%v err=%v", p, err)
	}
	if !pl4745CookieCleared(rr) {
		t.Error("response did not clear the pending-link cookie")
	}
}

func TestTASK4856_NoTokenReplyUnchanged(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	e.account(t, "notoken@example.com")

	rr := e.loginBody(t, "notoken@example.com", "", nil, "192.0.2.15:1234", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rr.Code, rr.Body.String())
	}
	var raw map[string]interface{}
	parseJSON(t, rr, &raw)
	if _, has := raw["pending_link"]; has {
		t.Fatalf("reply unexpectedly contains pending_link: %v", raw["pending_link"])
	}
	if tok, _ := raw["token"].(string); tok == "" {
		t.Fatalf("login minted no session: %s", rr.Body.String())
	}
}

func TestTASK4856_PostPeekEqualsGet(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "peekeq@example.com", "github")
	tok, err := e.srv.store.CreateOAuthPendingLink(u.ID, "google", "g-peekeq", u.Email)
	if err != nil {
		t.Fatal(err)
	}

	rrGet := e.browser(http.MethodGet, "/api/v1/auth/pending-link", nil, tok)
	postBody, err := json.Marshal(map[string]string{"pending_link_token": tok})
	if err != nil {
		t.Fatal(err)
	}
	rrPost := e.rawPost("/api/v1/auth/pending-link", postBody, "192.0.2.16:1234", "")

	if rrGet.Code != http.StatusOK {
		t.Fatalf("GET status = %d %s", rrGet.Code, rrGet.Body.String())
	}
	if rrPost.Code != rrGet.Code {
		t.Fatalf("POST status = %d, want %d (%s)", rrPost.Code, rrGet.Code, rrPost.Body.String())
	}
	if rrPost.Body.String() != rrGet.Body.String() {
		t.Fatalf("POST body = %q, want %q", rrPost.Body.String(), rrGet.Body.String())
	}
	if rrPost.Header().Get("Cache-Control") != "no-store" || rrPost.Header().Get("Cache-Control") != rrGet.Header().Get("Cache-Control") {
		t.Fatalf("Cache-Control POST=%q GET=%q", rrPost.Header().Get("Cache-Control"), rrGet.Header().Get("Cache-Control"))
	}
	var got map[string]interface{}
	parseJSON(t, rrPost, &got)
	if got["pending"] != true || got["provider"] != "google" || got["email"] != "peekeq@example.com" || got["password"] != true {
		t.Fatalf("unexpected POST peek reply: %v", got)
	}
}

func TestTASK4856_PostPeekNoneByteIdentical(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	const origin = "https://elsewhere.example"
	e.srv.SetCORSOrigins(origin)
	u := e.account(t, "peeknone@example.com")

	spent, err := e.srv.store.CreateOAuthPendingLink(u.ID, "google", "g-spent", u.Email)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.store.ConsumeOAuthPendingLink(spent); err != nil {
		t.Fatal(err)
	}
	expired, err := e.srv.store.CreateOAuthPendingLink(u.ID, "google", "g-exp", u.Email)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.store.DB().Exec(`UPDATE oauth_pending_links SET expires_at = ? WHERE used_at IS NULL`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		body []byte
	}{
		{"absent", nil},
		{"empty_object", []byte(`{}`)},
		{"garbage_token", []byte(`{"pending_link_token":"padpl_` + strings.Repeat("0", 64) + `"}`)},
		{"spent_token", []byte(fmt.Sprintf(`{"pending_link_token":%q}`, spent))},
		{"expired_token", []byte(fmt.Sprintf(`{"pending_link_token":%q}`, expired))},
		{"undecodable", []byte(`{not-json`)},
		{"oversized", []byte(`{"pending_link_token":"` + strings.Repeat("a", 9000) + `"}`)},
	}

	for i, tc := range cases {
		var req *http.Request
		if tc.body != nil {
			req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/pending-link", bytes.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
		} else {
			req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/pending-link", nil)
		}
		req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", 30+i)
		req.Header.Set("Origin", origin)
		rr := httptest.NewRecorder()
		e.srv.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (%s)", tc.name, rr.Code, rr.Body.String())
		}
		if rr.Body.String() != "{\"pending\":false}\n" {
			t.Fatalf("%s: body = %q, want %q", tc.name, rr.Body.String(), "{\"pending\":false}\n")
		}
		if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", tc.name, cc)
		}
		if acao := rr.Header().Get("Access-Control-Allow-Origin"); acao != "" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q, want empty", tc.name, acao)
		}
		if acac := rr.Header().Get("Access-Control-Allow-Credentials"); acac != "" {
			t.Errorf("%s: Access-Control-Allow-Credentials = %q, want empty", tc.name, acac)
		}
	}
}

func TestTASK4856_PostPeekIgnoresCookie(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "ignorecookie@example.com")
	tok, err := e.srv.store.CreateOAuthPendingLink(u.ID, "google", "g-ignore", u.Email)
	if err != nil {
		t.Fatal(err)
	}

	absent := e.rawPost("/api/v1/auth/pending-link", nil, "192.0.2.40:1234", "")
	withCookieNoBody := e.rawPost("/api/v1/auth/pending-link", nil, "192.0.2.41:1234", tok)
	withCookieEmptyObj := e.rawPost("/api/v1/auth/pending-link", []byte(`{}`), "192.0.2.42:1234", tok)

	for name, rr := range map[string]*httptest.ResponseRecorder{
		"absent":          absent,
		"cookie_no_body":  withCookieNoBody,
		"cookie_emptyobj": withCookieEmptyObj,
	} {
		if rr.Code != http.StatusOK || rr.Body.String() != "{\"pending\":false}\n" {
			t.Fatalf("%s: %d %q", name, rr.Code, rr.Body.String())
		}
	}
	if withCookieNoBody.Body.String() != absent.Body.String() || withCookieEmptyObj.Body.String() != absent.Body.String() {
		t.Fatalf("cookie POST differed from absent: noBody=%q emptyObj=%q absent=%q",
			withCookieNoBody.Body.String(), withCookieEmptyObj.Body.String(), absent.Body.String())
	}
	if p, err := e.srv.store.PeekOAuthPendingLink(tok); err != nil || p == nil {
		t.Fatalf("cookie token was spent by POST peek: peek=%v err=%v", p, err)
	}
}

func TestTASK4856_PostPeekDoesNotSpend(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "nospend@example.com")
	tok, err := e.srv.store.CreateOAuthPendingLink(u.ID, "google", "g-nospend", u.Email)
	if err != nil {
		t.Fatal(err)
	}

	postBody, err := json.Marshal(map[string]string{"pending_link_token": tok})
	if err != nil {
		t.Fatal(err)
	}
	rr := e.rawPost("/api/v1/auth/pending-link", postBody, "192.0.2.43:1234", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST peek: %d %s", rr.Code, rr.Body.String())
	}
	var peek map[string]interface{}
	parseJSON(t, rr, &peek)
	if peek["pending"] != true {
		t.Fatalf("POST peek reply = %v, want pending=true", peek)
	}
	if p, err := e.srv.store.PeekOAuthPendingLink(tok); err != nil || p == nil {
		t.Fatalf("token spent by POST peek: peek=%v err=%v", p, err)
	}

	loginRR := e.loginBody(t, "nospend@example.com", tok, pl4745Yes, "192.0.2.44:1234", "")
	pl4745WantLinked(t, pl4745Read(t, loginRR).PendingLink, "google")
	if !e.linked(t, u.ID, "google") {
		t.Fatal("subsequent login did not link")
	}
}

func TestTASK4856_PostPeekSelfHost404(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	e := pl4745Env{srv: srv}
	u := e.account(t, "selfpost@example.com")
	tok, err := srv.store.CreateOAuthPendingLink(u.ID, "google", "g-selfpost", u.Email)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"pending_link_token": tok})
	if err != nil {
		t.Fatal(err)
	}
	rr := e.rawPost("/api/v1/auth/pending-link", body, "192.0.2.45:1234", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("self-host POST /pending-link: %d %s, want 404", rr.Code, rr.Body.String())
	}
}

func TestTASK4856_PostPeekCSRFExemptWithAmbientCookie(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	u := e.account(t, "ambient-csrf@example.com")
	loginRR := e.loginBody(t, "ambient-csrf@example.com", "", nil, "192.0.2.46:1234", "")
	if loginRR.Code != http.StatusOK {
		t.Fatalf("login: %d %s", loginRR.Code, loginRR.Body.String())
	}
	var sessionCookie *http.Cookie
	for _, c := range loginRR.Result().Cookies() {
		if c.Name == "pad_session" || c.Name == "__Host-pad_session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("login did not set a session cookie")
	}

	tok, err := e.srv.store.CreateOAuthPendingLink(u.ID, "google", "g-csrf", u.Email)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"pending_link_token": tok})
	if err != nil {
		t.Fatal(err)
	}
	// Send valid session cookie and deliberately no X-CSRF-Token header or CSRF cookie.
	rr := e.rawPost("/api/v1/auth/pending-link", body, "192.0.2.47:1234", "", sessionCookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /pending-link with ambient session cookie: %d %s, want 200", rr.Code, rr.Body.String())
	}
	var got map[string]interface{}
	parseJSON(t, rr, &got)
	if got["pending"] != true || got["provider"] != "google" {
		t.Fatalf("POST /pending-link reply = %v", got)
	}
}

func TestTASK4856_AppleRelayNoToken(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	relayLower := e.account(t, "x@privaterelay.appleid.com")
	relayUpper := e.account(t, "y@privaterelay.appleid.com")
	normal := e.account(t, "normal@example.com")

	for name, addr := range map[string]string{
		"lower": "x@privaterelay.appleid.com",
		"upper": "y@PRIVATERELAY.APPLEID.COM",
	} {
		rr := postOAuthLogin(t, e.srv, pl4745OAuthBody("apple", addr, "apple-sub-"+name, true))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d %s, want 403", name, rr.Code, rr.Body.String())
		}
		var ref pl4745Refusal
		parseJSON(t, rr, &ref)
		if ref.Error.Code != "oauth_provider_not_linked" {
			t.Fatalf("%s: error code = %q, want oauth_provider_not_linked", name, ref.Error.Code)
		}
		if tok, _ := ref.Error.Details["pending_link_token"].(string); tok != "" {
			t.Fatalf("%s: apple relay unexpectedly received pending_link_token", name)
		}
	}
	if n := count(t, e.srv, `SELECT COUNT(*) FROM oauth_pending_links WHERE user_id IN (?, ?)`, relayLower.ID, relayUpper.ID); n != 0 {
		t.Fatalf("apple relay accounts hold %d pending link tokens, want 0", n)
	}
	// Also verify uppercase stored email directly on pendingLinkEligible.
	upperUser := *relayUpper
	upperUser.Email = "Y@PRIVATERELAY.APPLEID.COM"
	if pendingLinkEligible(&upperUser, "apple", nil) {
		t.Fatal("pendingLinkEligible returned true for uppercase apple relay email")
	}

	// Control: normal address with google still gets a token.
	tok := e.refuse(t, "google", normal.Email, "g-normal-ctrl", true)
	if !pl4745TokenRe.MatchString(tok) {
		t.Fatalf("google control token = %q, want valid padpl_ token", tok)
	}
}

func TestTASK4856_TokenNeverLogged(t *testing.T) {
	// Modifies slog.Default; must not run in parallel.
	var logBuf pl4856LogBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := newPL4745(t)
	u1 := e.account(t, "log-linked@example.com")
	u2 := e.account(t, "log-notlinked@example.com")
	u3 := e.account(t, "log-2fa@example.com")
	totpSecret := enableTOTPForDeleteUser(t, e.srv, u3.ID)

	tok1, err := e.srv.store.CreateOAuthPendingLink(u1.ID, "google", "g-log-1", u1.Email)
	if err != nil {
		t.Fatal(err)
	}
	tok2, err := e.srv.store.CreateOAuthPendingLink(u2.ID, "google", "g-log-2", u2.Email)
	if err != nil {
		t.Fatal(err)
	}
	tok3, err := e.srv.store.CreateOAuthPendingLink(u3.ID, "google", "g-log-3", u3.Email)
	if err != nil {
		t.Fatal(err)
	}

	// 1. POST peek with live body token.
	peekBody, err := json.Marshal(map[string]string{"pending_link_token": tok1})
	if err != nil {
		t.Fatal(err)
	}
	if rr := e.rawPost("/api/v1/auth/pending-link", peekBody, "192.0.2.50:1234", ""); rr.Code != http.StatusOK {
		t.Fatalf("POST peek: %d %s", rr.Code, rr.Body.String())
	}

	// 2. Login with body token (linked outcome).
	pl4745WantLinked(t, pl4745Read(t, e.loginBody(t, u1.Email, tok1, pl4745Yes, "192.0.2.51:1234", "")).PendingLink, "google")

	// 3. Login with body token (not-linked outcome).
	pl4745WantNotLinked(t, pl4745Read(t, e.loginBody(t, u2.Email, tok2, pl4745No, "192.0.2.52:1234", "")).PendingLink, pendingLinkCodeNotRequested)

	// 4. 2FA login + login-verify with body token.
	rr2fa := e.loginBody(t, u3.Email, tok3, pl4745Yes, "192.0.2.53:1234", "")
	var challenge struct {
		Requires2FA    bool   `json:"requires_2fa"`
		ChallengeToken string `json:"challenge_token"`
	}
	parseJSON(t, rr2fa, &challenge)
	if rr2fa.Code != http.StatusOK || !challenge.Requires2FA || challenge.ChallengeToken == "" {
		t.Fatalf("2fa login: %d %s", rr2fa.Code, rr2fa.Body.String())
	}
	verifyBody, err := json.Marshal(map[string]interface{}{
		"challenge_token":    challenge.ChallengeToken,
		"code":               validTOTPCode(t, totpSecret),
		"link_pending":       true,
		"pending_link_token": tok3,
	})
	if err != nil {
		t.Fatal(err)
	}
	pl4745WantLinked(t, pl4745Read(t, e.rawPost("/api/v1/auth/2fa/login-verify", verifyBody, "192.0.2.53:1234", "")).PendingLink, "google")

	// Wait for the async provider-linked emails so any async log lines settle.
	waitMail(t, e.mails)
	waitMail(t, e.mails)

	logs := logBuf.String()
	for _, tok := range []string{tok1, tok2, tok3, "padpl_"} {
		if strings.Contains(logs, tok) {
			t.Fatalf("captured slog output contains token material (%q)", tok)
		}
	}

	// Audit rows must carry no token either.
	if n := count(t, e.srv, `SELECT COUNT(*) FROM activities WHERE metadata LIKE '%padpl_%'`); n != 0 {
		t.Fatalf("%d audit rows contain padpl_ token material", n)
	}
	for _, tok := range []string{tok1, tok2, tok3} {
		if n := count(t, e.srv, `SELECT COUNT(*) FROM activities WHERE metadata LIKE ?`, "%"+tok+"%"); n != 0 {
			t.Fatalf("%d audit rows contain raw token", n)
		}
	}
}

func TestTASK4856_PostPeekUsesAuthRateLimitNotGet(t *testing.T) {
	t.Parallel()
	e := newPL4745(t)
	e.account(t, "ratelimit@example.com")

	const ip = "192.0.2.99:1234"
	// Six GETs must not burn the Auth bucket (burst 5).
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/pending-link", nil)
		req.RemoteAddr = ip
		rr := httptest.NewRecorder()
		e.srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %d: status = %d, want 200", i+1, rr.Code)
		}
	}

	// Five POSTs consume the Auth bucket; the 6th is rate-limited.
	for i := 0; i < 5; i++ {
		rr := e.rawPost("/api/v1/auth/pending-link", []byte(`{}`), ip, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("POST %d: status = %d, want 200", i+1, rr.Code)
		}
	}
	rr := e.rawPost("/api/v1/auth/pending-link", []byte(`{}`), ip, "")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("6th POST: status = %d, want 429", rr.Code)
	}
}
