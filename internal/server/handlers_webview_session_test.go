package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

const webViewMintPath = "/api/v1/auth/webview-session"

type webViewMintResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	MaxAge    int    `json:"max_age"`
}

// mintWebView asks for a child of the web session parentTok, sent as a bearer.
func mintWebView(srv *Server, parentTok string, body any) *httptest.ResponseRecorder {
	return doRequestWithBearer(srv, http.MethodPost, webViewMintPath, parentTok, body)
}

func mustMintWebView(t *testing.T, srv *Server, parentTok string, replacePrevious bool) string {
	t.Helper()
	var body any
	if replacePrevious {
		body = map[string]bool{"replace_previous": true}
	}
	rr := mintWebView(srv, parentTok, body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s, want 201", rr.Code, rr.Body.String())
	}
	var out webViewMintResponse
	parseJSON(t, rr, &out)
	if !strings.HasPrefix(out.Token, "padsess_") {
		t.Fatalf("mint token %q is not a padsess_ token", out.Token)
	}
	return out.Token
}

func sessionGone(t *testing.T, srv *Server, tok string) bool {
	t.Helper()
	info, err := srv.store.ValidateSession(tok)
	if err != nil {
		t.Fatalf("validate session: %v", err)
	}
	return info == nil
}

func TestWebViewSessionMint(t *testing.T) {
	srv := testServer(t)
	user, parent := loginTestUser(t, srv)
	parentInfo, _ := srv.store.ValidateSession(parent)

	rr := mintWebView(srv, parent, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s, want 201", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if len(rr.Result().Cookies()) != 0 {
		t.Errorf("mint set cookies %v, want none", rr.Result().Cookies())
	}
	var out webViewMintResponse
	parseJSON(t, rr, &out)
	if out.MaxAge < 895 || out.MaxAge > 900 {
		t.Errorf("max_age = %d, want 900", out.MaxAge)
	}
	exp, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q: %v", out.ExpiresAt, err)
	}
	if d := time.Until(exp); d < 14*time.Minute || d > 15*time.Minute+time.Second {
		t.Errorf("expires_at is %v away, want 15 minutes", d)
	}

	info, err := srv.store.ValidateSession(out.Token)
	if err != nil || info == nil {
		t.Fatalf("child does not validate: %v %v", info, err)
	}
	if info.Kind != store.SessionKindWebView || info.ParentID != parentInfo.ID || info.User.ID != user.ID {
		t.Fatalf("child row: kind=%q parent=%q user=%q", info.Kind, info.ParentID, info.User.ID)
	}
	if info.RenewTTL != 0 || info.SignInStamp != nil || info.StepUpUntil != nil {
		t.Fatalf("child row slides or reads as signed in: renew=%v stamp=%v stepup=%v", info.RenewTTL, info.SignInStamp, info.StepUpUntil)
	}

	// Minted by the native client for the WebView, so it carries no
	// User-Agent binding: strict enforcement must not revoke it on the
	// WebView's own User-Agent.
	if info.UAHash != "" {
		t.Fatalf("child is bound to the minting client's User-Agent (%q)", info.UAHash)
	}
	srv.ipChangeEnforceStrict = true
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 15; wv) WebView/1")
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: out.Token})
	strict := httptest.NewRecorder()
	srv.ServeHTTP(strict, req)
	if strict.Code != http.StatusOK || sessionGone(t, srv, out.Token) {
		t.Fatalf("the child under strict binding with the WebView User-Agent: %d %s", strict.Code, strict.Body.String())
	}
	srv.ipChangeEnforceStrict = false

	// The child is a cookie for the UI.
	me := doRequestWithCookie(srv, http.MethodGet, "/api/v1/auth/me", nil, out.Token)
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), user.ID) {
		t.Fatalf("GET /auth/me with the child cookie: %d %s", me.Code, me.Body.String())
	}
	ws := doRequestWithCookie(srv, http.MethodPost, "/api/v1/workspaces", map[string]any{"name": "WebView WS"}, out.Token)
	if ws.Code != http.StatusCreated {
		t.Fatalf("an ordinary write with the child cookie: %d %s, want 201", ws.Code, ws.Body.String())
	}
	// The parent is untouched.
	if sessionGone(t, srv, parent) {
		t.Fatal("minting ended the parent")
	}
}

func TestWebViewSessionCappedAtParentExpiry(t *testing.T) {
	srv := testServer(t)
	user, _ := loginTestUser(t, srv)
	parent, err := srv.store.CreateSession(user.ID, "test", "192.0.2.1", testSessionUA, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parentInfo, _ := srv.store.ValidateSession(parent)
	rr := mintWebView(srv, parent, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rr.Code, rr.Body.String())
	}
	var out webViewMintResponse
	parseJSON(t, rr, &out)
	if out.MaxAge > 300 {
		t.Errorf("max_age = %d, want at most the parent's 300 s", out.MaxAge)
	}
	var childExp, parentExp string
	if err := srv.store.DB().QueryRow(`SELECT expires_at FROM sessions WHERE parent_session_id = ?`, parentInfo.ID).Scan(&childExp); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.DB().QueryRow(`SELECT expires_at FROM sessions WHERE id = ?`, parentInfo.ID).Scan(&parentExp); err != nil {
		t.Fatal(err)
	}
	if childExp != parentExp || out.ExpiresAt != parentExp {
		t.Fatalf("child expires %s (response %s), parent %s: want the parent's expiry", childExp, out.ExpiresAt, parentExp)
	}
}

func TestWebViewSessionMintRefusesOtherCredentials(t *testing.T) {
	srv := testServer(t)
	user, parent := loginTestUser(t, srv)
	cli, err := srv.store.CreateSession(user.ID, "cli", "192.0.2.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	pat, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{Name: "webview-pat"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	child := mustMintWebView(t, srv, parent, false)
	before := countUserSessions(t, srv, user.ID)

	cases := []struct {
		name   string
		rr     *httptest.ResponseRecorder
		status int
		code   string
	}{
		{"no credential", doRequestWithCookie(srv, http.MethodPost, webViewMintPath, nil, ""), http.StatusUnauthorized, "unauthorized"},
		{"web session as a cookie", doRequestWithCookie(srv, http.MethodPost, webViewMintPath, nil, parent), http.StatusForbidden, "session_bearer_required"},
		{"api token", doRequestWithBearer(srv, http.MethodPost, webViewMintPath, pat.Token, nil), http.StatusForbidden, "session_bearer_required"},
		{"cli session bearer", doRequestWithBearer(srv, http.MethodPost, webViewMintPath, cli, nil), http.StatusForbidden, "webview_session_forbidden"},
		{"webview child as a bearer", doRequestWithBearer(srv, http.MethodPost, webViewMintPath, child, nil), http.StatusUnauthorized, "unauthorized"},
		{"webview child as a cookie", doRequestWithCookie(srv, http.MethodPost, webViewMintPath, nil, child), http.StatusForbidden, "webview_session_forbidden"},
		{"unknown body member", mintWebView(srv, parent, map[string]any{"replace": true}), http.StatusBadRequest, "bad_request"},
	}
	for _, c := range cases {
		if c.rr.Code != c.status || errorCode(t, c.rr) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, c.rr.Code, c.rr.Body.String(), c.status, c.code)
		}
	}
	if after := countUserSessions(t, srv, user.ID); after != before {
		t.Fatalf("a refused mint changed the session count: %d -> %d", before, after)
	}
}

func countUserSessions(t *testing.T, srv *Server, userID string) int {
	t.Helper()
	var n int
	if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, userID).Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return n
}

func TestWebViewSessionNotABearer(t *testing.T) {
	srv := testServer(t)
	user, parent := loginTestUser(t, srv)
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Bearer WS", OwnerID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	child := mustMintWebView(t, srv, parent, false)
	for _, path := range []string{"/api/v1/workspaces", "/api/v1/workspaces/" + ws.Slug + "/collections", "/api/v1/auth/me"} {
		rr := doRequestWithBearer(srv, http.MethodGet, path, child, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with the child as a bearer: %d %s, want 401", path, rr.Code, rr.Body.String())
		}
	}
	// The control: the parent bearer reaches the same routes.
	if rr := doRequestWithBearer(srv, http.MethodGet, "/api/v1/workspaces", parent, nil); rr.Code != http.StatusOK {
		t.Fatalf("parent bearer control: %d %s", rr.Code, rr.Body.String())
	}
}

func TestWebViewSessionNeverSlides(t *testing.T) {
	srv := testServer(t)
	_, parent := loginTestUser(t, srv)
	child := mustMintWebView(t, srv, parent, false)
	info, _ := srv.store.ValidateSession(child)
	// One minute left: any sliding session would renew here.
	soon := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(`UPDATE sessions SET expires_at = ? WHERE id = ?`, soon, info.ID); err != nil {
		t.Fatal(err)
	}
	rr := doRequestWithCookie(srv, http.MethodGet, "/api/v1/workspaces", nil, child)
	if rr.Code != http.StatusOK {
		t.Fatalf("request with the child: %d %s", rr.Code, rr.Body.String())
	}
	if sessionCookieSet(srv, rr) {
		t.Fatal("the child's cookie was re-issued")
	}
	if _, renewed, err := srv.store.RenewSessionIfStale(child); err != nil || renewed {
		t.Fatalf("RenewSessionIfStale renewed the child: %v %v", renewed, err)
	}
	var exp string
	if err := srv.store.DB().QueryRow(`SELECT expires_at FROM sessions WHERE id = ?`, info.ID).Scan(&exp); err != nil {
		t.Fatal(err)
	}
	if exp != soon {
		t.Fatalf("child expiry moved from %s to %s", soon, exp)
	}
}

// webViewServerBackends runs fn on a SQLite server and, when
// PAD_TEST_POSTGRES_URL is set, a Postgres one.
func webViewServerBackends(t *testing.T, fn func(t *testing.T, srv *Server)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) { fn(t, testServer(t)) })
	t.Run("postgres", func(t *testing.T) {
		if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
			t.Skip("PAD_TEST_POSTGRES_URL not set; Postgres-backed leg skipped")
		}
		srv := New(storetest.NewPostgres(t))
		t.Cleanup(srv.Stop)
		fn(t, srv)
	})
}

func TestWebViewSessionDiesWithParent(t *testing.T) {
	webViewServerBackends(t, func(t *testing.T, srv *Server) {
		_, parent := loginTestUser(t, srv)
		a := mustMintWebView(t, srv, parent, false)
		b := mustMintWebView(t, srv, parent, false)
		if rr := doRequestWithCookie(srv, http.MethodGet, "/api/v1/auth/me", nil, a); rr.Code != http.StatusOK {
			t.Fatalf("child before the parent ends: %d %s", rr.Code, rr.Body.String())
		}
		if err := srv.store.DeleteSession(parent); err != nil {
			t.Fatal(err)
		}
		for _, tok := range []string{a, b} {
			if !sessionGone(t, srv, tok) {
				t.Fatal("a child survived its parent")
			}
			if rr := doRequestWithCookie(srv, http.MethodGet, "/api/v1/auth/me", nil, tok); rr.Code != http.StatusUnauthorized {
				t.Fatalf("child after the parent ended: %d %s, want 401", rr.Code, rr.Body.String())
			}
		}
		// A mint from a parent that is gone is 401 and inserts nothing.
		if rr := mintWebView(srv, parent, nil); rr.Code != http.StatusUnauthorized {
			t.Fatalf("mint from an ended parent: %d %s, want 401", rr.Code, rr.Body.String())
		}
	})
}

func TestWebViewSessionReplacePrevious(t *testing.T) {
	srv := testServer(t)
	user, parent := loginTestUser(t, srv)
	other, err := srv.store.CreateSession(user.ID, "test", "192.0.2.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	a := mustMintWebView(t, srv, parent, false)
	b := mustMintWebView(t, srv, parent, false)
	otherChild := mustMintWebView(t, srv, other, false)
	if sessionGone(t, srv, a) || sessionGone(t, srv, b) {
		t.Fatal("a mint without replace_previous ended an earlier child")
	}
	c := mustMintWebView(t, srv, parent, true)
	if !sessionGone(t, srv, a) || !sessionGone(t, srv, b) {
		t.Fatal("replace_previous left an earlier child alive")
	}
	if sessionGone(t, srv, c) || sessionGone(t, srv, parent) {
		t.Fatal("replace_previous ended the new child or the parent")
	}
	if sessionGone(t, srv, otherChild) {
		t.Fatal("replace_previous ended a child of another parent")
	}
}

func TestWebViewLogoutEndsParent(t *testing.T) {
	srv := testServer(t)
	user, parent := loginTestUser(t, srv)
	other, err := srv.store.CreateSession(user.ID, "test", "192.0.2.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	a := mustMintWebView(t, srv, parent, false)
	b := mustMintWebView(t, srv, parent, false)
	rr := doRequestWithCookie(srv, http.MethodPost, "/api/v1/auth/logout", nil, a)
	if rr.Code != http.StatusOK {
		t.Fatalf("logout: %d %s", rr.Code, rr.Body.String())
	}
	for name, tok := range map[string]string{"the child": a, "its sibling": b, "the parent": parent} {
		if !sessionGone(t, srv, tok) {
			t.Errorf("logout with a child cookie left %s alive", name)
		}
	}
	if sessionGone(t, srv, other) {
		t.Fatal("logout with a child cookie ended an unrelated session")
	}

	// An expired child still names its parent: logout ends the parent.
	c := mustMintWebView(t, srv, other, false)
	cInfo, _ := srv.store.ValidateSession(c)
	if _, err := srv.store.DB().Exec(`UPDATE sessions SET expires_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), cInfo.ID); err != nil {
		t.Fatal(err)
	}
	if rr := doRequestWithCookie(srv, http.MethodPost, "/api/v1/auth/logout", nil, c); rr.Code != http.StatusOK {
		t.Fatalf("logout with an expired child: %d %s", rr.Code, rr.Body.String())
	}
	if !sessionGone(t, srv, other) {
		t.Fatal("logout with an expired child left its parent alive")
	}
}

// webViewDoorFixture is a signed-in admin with a web parent, a child, a
// workspace and a PAT, for the refused-door rows.
type webViewDoorFixture struct {
	srv    *Server
	user   *models.User
	parent string
	child  string
	slug   string
	pat    *models.APITokenWithSecret
}

func newWebViewDoorFixture(t *testing.T) *webViewDoorFixture {
	t.Helper()
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "1")
	srv := testServer(t)
	user, parent := loginTestUser(t, srv)
	if _, err := srv.store.DB().Exec(`UPDATE users SET role = 'admin' WHERE id = ?`, user.ID); err != nil {
		t.Fatal(err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Doors WS", OwnerID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	pat, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{Name: "door-pat"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &webViewDoorFixture{srv: srv, user: user, parent: parent, child: mustMintWebView(t, srv, parent, false), slug: ws.Slug, pat: pat}
}

func webViewTableCounts(t *testing.T, srv *Server) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"sessions", "users", "api_tokens", "webhooks", "calendar_feeds", "push_devices", "step_up_codes", "user_oauth_identities"} {
		var n int
		if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

func TestWebViewSessionRefusedAtCredentialDoors(t *testing.T) {
	f := newWebViewDoorFixture(t)
	victim, err := f.srv.store.CreateUser(models.UserCreate{Email: "victim@example.com", Name: "Victim", Password: "victim-pass-12345"})
	if err != nil {
		t.Fatal(err)
	}
	ws := "/api/v1/workspaces/" + f.slug
	doors := []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/v1/auth/tokens", map[string]any{"name": "minted"}},
		{"PATCH", "/api/v1/auth/tokens/" + f.pat.ID, map[string]any{"name": "renamed"}},
		{"POST", "/api/v1/auth/tokens/" + f.pat.ID + "/rotate", nil},
		{"POST", ws + "/tokens", map[string]any{"name": "minted"}},
		{"POST", "/api/v1/auth/webview-session", nil},
		{"POST", "/api/v1/auth/cli/sessions/ABCD-EFGH/approve", nil},
		{"POST", "/api/v1/me/calendar-feeds", map[string]any{"name": "feed"}},
		{"POST", ws + "/apps/install-x/install-code", nil},
		{"POST", ws + "/apps/install-x/rotate", nil},
		{"PUT", "/api/v1/me/push-devices/conn-1", map[string]any{"platform": "android", "token": "fcm-token"}},
		{"POST", "/api/v1/auth/2fa/setup", nil},
		{"POST", "/api/v1/auth/2fa/verify", map[string]any{"code": "123456"}},
		{"POST", "/api/v1/auth/2fa/disable", map[string]any{"password": "pw-test-12345", "code": "123456"}},
		{"POST", "/api/v1/auth/delete-account", map[string]any{"password": "pw-test-12345"}},
		{"POST", "/api/v1/auth/step-up/email", nil},
		{"POST", "/api/v1/auth/step-up/verify", map[string]any{"code": "123456"}},
		{"POST", "/api/v1/auth/oauth-link", map[string]any{"provider": "github", "provider_id": "1"}},
		{"POST", "/api/v1/auth/oauth-unlink", map[string]any{"provider": "github"}},
		{"POST", ws + "/webhooks", map[string]any{"url": "https://example.com/hook"}},
		{"POST", "/api/v1/auth/register", map[string]any{"email": "made@example.com", "name": "Made", "password": "made-pass-12345"}},
		{"POST", "/api/v1/admin/users/" + victim.ID + "/reset-password", nil},
		// An encoded slash routes on the raw path; the refusal reads it too.
		{"POST", "/api/v1/workspaces/a%2Fb/tokens", map[string]any{"name": "minted"}},
	}
	before := webViewTableCounts(t, f.srv)
	for _, d := range doors {
		rr := doRequestWithCookie(f.srv, d.method, d.path, d.body, f.child)
		if rr.Code != http.StatusForbidden || errorCode(t, rr) != "webview_session_forbidden" {
			t.Errorf("%s %s with the child cookie: %d %s, want 403 webview_session_forbidden", d.method, d.path, rr.Code, rr.Body.String())
		}
	}
	after := webViewTableCounts(t, f.srv)
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s rows %d -> %d: a refused door wrote", table, n, after[table])
		}
	}
	if tok, err := f.srv.store.ValidateToken(f.pat.Token); err != nil || tok == nil || tok.Name != "door-pat" {
		t.Errorf("the PAT was renamed or rotated: %+v %v", tok, err)
	}
	u, err := f.srv.store.GetUser(f.user.ID)
	if err != nil || u == nil || u.TOTPEnabled {
		t.Errorf("caller after the doors: %+v %v", u, err)
	}
	if ok, _ := f.srv.store.ValidatePassword(victim.Email, "victim-pass-12345"); ok == nil {
		t.Error("the admin reset changed another user's password")
	}

	// POST /oauth/authorize/decide is mounted behind the OAuth availability
	// gate, which answers 404 here; SessionAuth is checked on its own.
	reached := false
	h := f.srv.SessionAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	req := httptest.NewRequest(http.MethodPost, "/oauth/authorize/decide", strings.NewReader("x=1"))
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: f.child})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if reached || rr.Code != http.StatusForbidden || errorCode(t, rr) != "webview_session_forbidden" {
		t.Errorf("SessionAuth on POST /oauth/authorize/decide with the child: reached=%v %d %s", reached, rr.Code, rr.Body.String())
	}

	// The control: the same doors with the parent cookie are not refused as
	// a WebView session.
	for _, d := range doors {
		rr := doRequestWithCookie(f.srv, d.method, d.path, d.body, f.parent)
		if rr.Code == http.StatusForbidden && strings.Contains(rr.Body.String(), "webview_session_forbidden") {
			t.Errorf("%s %s with the parent cookie was refused as a WebView session", d.method, d.path)
		}
	}
}

func TestWebViewPasswordChangeRefused(t *testing.T) {
	f := newWebViewDoorFixture(t)
	rr := doRequestWithCookie(f.srv, http.MethodPatch, "/api/v1/auth/me",
		map[string]any{"current_password": "pw-test-12345", "new_password": "a-new-long-password-9876"}, f.child)
	if rr.Code != http.StatusForbidden || errorCode(t, rr) != "webview_session_forbidden" {
		t.Fatalf("password change with the child: %d %s, want 403 webview_session_forbidden", rr.Code, rr.Body.String())
	}
	if ok, _ := f.srv.store.ValidatePassword(f.user.Email, "pw-test-12345"); ok == nil {
		t.Fatal("the password changed")
	}
	if sessionGone(t, f.srv, f.parent) || sessionGone(t, f.srv, f.child) {
		t.Fatal("a refused password change rotated sessions")
	}
	// A name-only PATCH still works.
	rr = doRequestWithCookie(f.srv, http.MethodPatch, "/api/v1/auth/me", map[string]any{"name": "Renamed In App"}, f.child)
	if rr.Code != http.StatusOK {
		t.Fatalf("name-only PATCH with the child: %d %s, want 200", rr.Code, rr.Body.String())
	}
	if u, _ := f.srv.store.GetUser(f.user.ID); u == nil || u.Name != "Renamed In App" {
		t.Fatalf("name not saved: %+v", u)
	}
}

func TestWebViewDeleteAccountRefused(t *testing.T) {
	f := newWebViewDoorFixture(t)
	for _, body := range []map[string]any{{"password": "pw-test-12345"}, {"confirm": true}} {
		rr := doRequestWithCookie(f.srv, http.MethodPost, "/api/v1/auth/delete-account", body, f.child)
		if rr.Code != http.StatusForbidden || errorCode(t, rr) != "webview_session_forbidden" {
			t.Fatalf("delete-account %v with the child: %d %s, want 403 webview_session_forbidden", body, rr.Code, rr.Body.String())
		}
	}
	if u, err := f.srv.store.GetUser(f.user.ID); err != nil || u == nil {
		t.Fatalf("the account is gone: %v %v", u, err)
	}
}

// webViewClassifiedPattern names every route that can create, widen or prove
// a credential. A route matching it must be in webViewRefusedRoutes or
// webViewAllowedRoutes, so a door added later is classified by test.
var webViewClassifiedPattern = regexp.MustCompile(`token|oauth|cli/|calendar-feeds|step-up|2fa|password|delete-account|push-devices|install-code|rotate|logout|webview|webhook|admin/users|register|claim-code`)

// webViewRefusedRoutes are refused to a WebView session (webViewRouteRefused).
var webViewRefusedRoutes = map[string]string{
	"POST /api/v1/auth/tokens":                                     "mints an API key",
	"PATCH /api/v1/auth/tokens/{tokenID}":                          "widens an API key",
	"POST /api/v1/auth/tokens/{tokenID}/rotate":                    "mints a new API key secret",
	"POST /api/v1/workspaces/{slug}/tokens/":                       "mints a workspace API key",
	"POST /api/v1/auth/webview-session":                            "a child never mints a child",
	"POST /api/v1/auth/cli/sessions/{code}/approve":                "mints a 30-day CLI session",
	"POST /oauth/authorize/decide":                                 "grants an OAuth connection",
	"POST /api/v1/me/calendar-feeds":                               "mints a calendar feed URL",
	"POST /api/v1/workspaces/{slug}/apps/{installID}/install-code": "issues an app install code",
	"POST /api/v1/workspaces/{slug}/apps/{installID}/rotate":       "rotates an app secret",
	"PUT /api/v1/me/push-devices/{connection_id}":                  "binds a push device to the session",
	"POST /api/v1/auth/2fa/setup":                                  "changes a sign-in factor",
	"POST /api/v1/auth/2fa/verify":                                 "enables a sign-in factor",
	"POST /api/v1/auth/2fa/disable":                                "removes a sign-in factor and rotates sessions",
	"POST /api/v1/auth/delete-account":                             "irreversible; its freshness check reads created_at",
	"POST /api/v1/auth/step-up/email":                              "starts a step-up grant",
	"POST /api/v1/auth/step-up/verify":                             "grants step-up on the session",
	"POST /api/v1/auth/oauth-link":                                 "links a sign-in provider",
	"POST /api/v1/auth/oauth-unlink":                               "unlinks a sign-in provider and rotates sessions",
	"POST /api/v1/workspaces/{slug}/webhooks/":                     "creates a webhook, a standing outbound feed",
	"POST /api/v1/auth/register":                                   "with an admin caller, creates an account",
	"POST /api/v1/admin/users/{userID}/reset-password":             "sets another account's password",
}

// webViewAllowedRoutes match the pattern but stay open to a WebView
// session, each for the reason given.
var webViewAllowedRoutes = map[string]string{
	"GET /api/v1/auth/tokens":                                  "lists keys; no secret",
	"DELETE /api/v1/auth/tokens/{tokenID}":                     "revocation only narrows access",
	"GET /api/v1/workspaces/{slug}/tokens/":                    "lists keys; no secret",
	"DELETE /api/v1/workspaces/{slug}/tokens/{tokenID}":        "revocation only narrows access",
	"GET /api/v1/me/calendar-feeds":                            "lists feeds; no secret URL",
	"DELETE /api/v1/me/calendar-feeds/{id}":                    "revocation only narrows access",
	"DELETE /api/v1/me/push-devices/{connection_id}":           "removes a device only",
	"GET /api/v1/auth/step-up":                                 "reads step-up status",
	"GET /api/v1/auth/cli/sessions/{code}":                     "the CLI's poll; the code is the credential",
	"POST /api/v1/auth/cli/sessions":                           "opens an unapproved request; nothing is granted",
	"POST /api/v1/auth/cli/sessions/{code}/deny":               "denies a request; nothing is granted",
	"POST /api/v1/auth/logout":                                 "ends the child and its parent",
	"POST /api/v1/auth/2fa/login-verify":                       "a sign-in step; mints from the challenge, not the cookie",
	"POST /api/v1/auth/forgot-password":                        "sends a reset mail to the account owner",
	"POST /api/v1/auth/reset-password":                         "the emailed token is the credential",
	"POST /api/v1/auth/oauth-login":                            "a sign-in by the cloud sidecar; the cookie is not the credential",
	"GET /api/v1/feeds/ics/{token}":                            "the feed URL is the credential; no cookie is read",
	"GET /api/v1/s/{token}":                                    "a share link is the credential",
	"GET /api/v1/s/{token}/attachments/{attachmentID}":         "a share link is the credential",
	"GET /api/v1/oauth/clients/{id}/public-info":               "reads public client metadata",
	"POST /api/v1/oauth/claim":                                 "redeemed by an OAuth grant, not a cookie session",
	"GET /api/v1/workspaces/{slug}/claim-code":                 "the code verifies only for the same user's grant (handleOAuthClaim)",
	"GET /api/v1/workspaces/{slug}/webhooks/":                  "lists webhooks",
	"DELETE /api/v1/workspaces/{slug}/webhooks/{webhookID}/":   "removal only narrows access",
	"POST /api/v1/workspaces/{slug}/webhooks/{webhookID}/test": "sends a test delivery to an existing webhook",
	"GET /api/v1/admin/users":                                  "admin read",
	"GET /api/v1/admin/users/{userID}/":                        "admin read",
	"GET /api/v1/admin/users/{userID}/activity":                "admin read",
	"GET /api/v1/admin/users/{userID}/detail":                  "admin read",
	"GET /api/v1/admin/users/{userID}/metrics":                 "admin read",
	"GET /api/v1/admin/users/{userID}/workspaces":              "admin read",
	"PATCH /api/v1/admin/users/{userID}/":                      "accepted residual: admin role edit, UI authority",
	"POST /api/v1/admin/users/{userID}/disable":                "only removes access",
	"POST /api/v1/admin/users/{userID}/enable":                 "accepted residual: restores an account, mints no credential",
	"POST /api/v1/admin/users/{userID}/verify-email":           "accepted residual: marks an address verified, mints no credential",
	"GET /oauth/authorize":                                     "renders consent; nothing is granted until decide",
	"POST /oauth/token":                                        "PKCE or client bound; the cookie is not the credential",
	"POST /oauth/revoke":                                       "revocation only narrows access",
	"POST /oauth/introspect":                                   "reads a token the caller holds",
	"POST /oauth/register":                                     "registers a public client; grants nothing",
	"GET /.well-known/oauth-authorization-server":              "public metadata",
	"GET /.well-known/oauth-protected-resource":                "public metadata",
	"GET /.well-known/oauth-protected-resource/mcp":            "public metadata",
	"GET /.well-known/oauth-protected-resource/mcp/":           "public metadata",
	"GET /.well-known/oauth-protected-resource/mcp/chatgpt":    "public metadata",
}

func TestEveryCredentialRouteClassifiedForWebView(t *testing.T) {
	srv := testServer(t)
	srv.ensureRouter()
	param := regexp.MustCompile(`\{[^}]*\}`)
	seen := map[string]bool{}
	matched := 0
	err := chi.Walk(srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := method + " " + route
		path := strings.ReplaceAll(param.ReplaceAllString(route, "x"), "*", "x")
		forms := []string{path}
		if trimmed := strings.TrimSuffix(path, "/"); trimmed != path && trimmed != "" {
			forms = append(forms, trimmed)
		}
		refusedAny := false
		for _, p := range forms {
			if webViewRouteRefused(httptest.NewRequest(method, p, nil)) {
				refusedAny = true
			}
		}
		if !webViewClassifiedPattern.MatchString(route) {
			if refusedAny {
				t.Errorf("%s is refused to a WebView session but outside the classified pattern", key)
			}
			return nil
		}
		matched++
		seen[key] = true
		_, refused := webViewRefusedRoutes[key]
		reason, allowed := webViewAllowedRoutes[key]
		switch {
		case refused && allowed:
			t.Errorf("%s is in both the refused and the allowed table", key)
		case refused:
			for _, p := range forms {
				if !webViewRouteRefused(httptest.NewRequest(method, p, nil)) {
					t.Errorf("%s (%s) is in the refused table but webViewRouteRefused allows it", key, p)
				}
			}
		case allowed:
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%s is allowed with no reason", key)
			}
			if refusedAny {
				t.Errorf("%s is in the allowed table but webViewRouteRefused refuses it", key)
			}
		default:
			t.Errorf("%s matches the credential pattern but is neither refused nor allowed for a WebView session", key)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []map[string]string{webViewRefusedRoutes, webViewAllowedRoutes} {
		for key := range table {
			if !seen[key] {
				t.Errorf("%s is classified but no longer registered", key)
			}
		}
	}
	if matched < 40 {
		t.Fatalf("only %d routes matched; the walk is not seeing the router", matched)
	}
	// A webhook update route does not exist yet; one added later is refused.
	for _, m := range []string{http.MethodPatch, http.MethodPut} {
		if !webViewRouteRefused(httptest.NewRequest(m, "/api/v1/workspaces/x/webhooks/x", nil)) {
			t.Errorf("%s on a webhook is not refused to a WebView session", m)
		}
	}
}

func TestWebViewStreamEndsAtExpiry(t *testing.T) {
	prev := sseMembershipRevalInterval
	sseMembershipRevalInterval = 25 * time.Millisecond
	t.Cleanup(func() { sseMembershipRevalInterval = prev })

	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	user, parent := loginTestUser(t, srv)
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Stream WS", OwnerID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	child := mustMintWebView(t, srv, parent, false)
	childInfo, _ := srv.store.ValidateSession(child)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events?workspace="+ws.Slug, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: child})
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}
	if _, err := srv.store.DB().Exec(`UPDATE sessions SET expires_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Second).Format(time.RFC3339), childInfo.ID); err != nil {
		t.Fatal(err)
	}
	if !streamClosedWithin(t, bufio.NewScanner(resp.Body), 3*time.Second) {
		t.Fatal("stream opened with the child kept running after the child expired")
	}
	if sessionGone(t, srv, parent) {
		t.Fatal("the parent ended with the child")
	}
}

func TestWebViewSessionMintRateLimited(t *testing.T) {
	srv := testServer(t)
	user, parent := loginTestUser(t, srv)
	other, err := srv.store.CreateSession(user.ID, "test", "192.0.2.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if rr := mintWebView(srv, parent, nil); rr.Code != http.StatusCreated {
			t.Fatalf("mint %d: %d %s, want 201 within the burst", i+1, rr.Code, rr.Body.String())
		}
	}
	before := countUserSessions(t, srv, user.ID)
	rr := mintWebView(srv, parent, nil)
	if rr.Code != http.StatusTooManyRequests || errorCode(t, rr) != "rate_limited" {
		t.Fatalf("mint 31: %d %s, want 429 rate_limited", rr.Code, rr.Body.String())
	}
	if after := countUserSessions(t, srv, user.ID); after != before {
		t.Fatalf("a rate-limited mint inserted a session: %d -> %d", before, after)
	}
	// The bucket is the parent's: another session of the same user mints.
	if rr := mintWebView(srv, other, nil); rr.Code != http.StatusCreated {
		t.Fatalf("mint from another parent: %d %s, want 201", rr.Code, rr.Body.String())
	}
}
