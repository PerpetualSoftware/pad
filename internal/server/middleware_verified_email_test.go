package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-1933 DR-4 — RequireVerifiedEmail enforcement across every
// authenticated mutation perimeter. These tests are the DR-4 "systematic
// perimeter audit": one blocked-unverified assertion per perimeter, plus
// the three cross-cutting invariants (verified passes, self-host is a
// no-op, invitation-accept carve-out works unverified).
//
// Unverified users are created directly via the store's explicit
// UserCreate.Unverified control (Wave 1) — nothing in production mints an
// unverified user until Wave 3b, so the store is the only way to reach
// the state this middleware guards.

// veUser bundles the two credential shapes a member can present.
type veUser struct {
	id      string
	email   string
	session string // raw session token (cookie value)
	bearer  string // raw PAT (Authorization: Bearer)
}

// verifiedEmailFixture is a workspace with one unverified + one verified
// editor member, each holding a session cookie AND a workspace-scoped
// PAT. cloud toggles SetCloudMode so the same fixture drives both the
// enforcement tests and the self-host no-op test.
type verifiedEmailFixture struct {
	srv      *Server
	wsSlug   string
	collSlug string
	unv      veUser
	ver      veUser
}

func newVerifiedEmailFixture(t *testing.T, cloud bool) *verifiedEmailFixture {
	t.Helper()
	srv := testServer(t)
	if cloud {
		srv.SetCloudMode("ve-secret")
	}
	// Bootstrap the first admin so UserCount>0 → RequireAuth is active
	// (the middleware chain we're exercising only kicks in once users
	// exist). The admin is verified via the DR-3 safe default.
	bootstrapFirstUser(t, srv, "admin@ve.test", "Admin")
	admin, err := srv.store.GetUserByEmail("admin@ve.test")
	if err != nil || admin == nil {
		t.Fatalf("GetUserByEmail admin: %v", err)
	}
	// The workspace needs an owner so the cloud-mode plan-limit check on
	// item-create can resolve the owner's plan (else it 500s on "owner
	// not found"). A verified item-create must reach 201.
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Verified Email WS", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	mkMember := func(email string, unverified bool) veUser {
		u, err := srv.store.CreateUser(models.UserCreate{
			Email:      email,
			Name:       email,
			Password:   "correct-horse-battery-staple",
			Role:       "member",
			Unverified: unverified,
		})
		if err != nil {
			t.Fatalf("CreateUser %s: %v", email, err)
		}
		if u.IsEmailVerified() == unverified {
			t.Fatalf("CreateUser %s: IsEmailVerified()=%v, want %v", email, u.IsEmailVerified(), !unverified)
		}
		if err := srv.store.AddWorkspaceMember(ws.ID, u.ID, "editor"); err != nil {
			t.Fatalf("AddWorkspaceMember %s: %v", email, err)
		}
		// UA="" → no session UA binding; ip matches doRequestWithCookie's
		// 192.0.2.1 so the IP-change path stays quiet.
		sess, err := srv.store.CreateSession(u.ID, "ve", "192.0.2.1", "", 24*time.Hour)
		if err != nil {
			t.Fatalf("CreateSession %s: %v", email, err)
		}
		pat, err := srv.store.CreateAPIToken(u.ID, models.APITokenCreate{
			Name: "ve-pat", WorkspaceID: ws.ID,
		}, 0, 0)
		if err != nil {
			t.Fatalf("CreateAPIToken %s: %v", email, err)
		}
		return veUser{id: u.ID, email: email, session: sess, bearer: pat.Token}
	}

	return &verifiedEmailFixture{
		srv:      srv,
		wsSlug:   ws.Slug,
		collSlug: coll.Slug,
		unv:      mkMember("unverified@ve.test", true),
		ver:      mkMember("verified@ve.test", false),
	}
}

func (f *verifiedEmailFixture) itemsPath() string {
	return "/api/v1/workspaces/" + f.wsSlug + "/collections/" + f.collSlug + "/items"
}

// veDoBearer issues a request authenticated with a Bearer PAT (which
// skips CSRF). Mirrors the CLI / connector path through TokenAuth.
func veDoBearer(srv *Server, method, path string, body any, bearer string) *httptest.ResponseRecorder {
	var r *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = strings.NewReader(string(b))
	}
	var req *http.Request
	if r != nil {
		req = httptest.NewRequest(method, path, r)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// veErrorCode extracts the JSON error.code from a response, or "" if the
// body isn't an error envelope.
func veErrorCode(rr *httptest.ResponseRecorder) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	return env.Error.Code
}

// assertBlocked asserts the response is a 403 email_not_verified.
func assertBlocked(t *testing.T, rr *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rr.Code != http.StatusForbidden {
		t.Fatalf("%s: expected 403, got %d (body: %s)", what, rr.Code, rr.Body.String())
	}
	if code := veErrorCode(rr); code != "email_not_verified" {
		t.Fatalf("%s: expected error code email_not_verified, got %q (body: %s)", what, code, rr.Body.String())
	}
}

// assertNotEmailBlocked asserts the response is NOT a verified-email 403
// (it may legitimately be a 2xx, or a 4xx for an unrelated reason).
func assertNotEmailBlocked(t *testing.T, rr *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rr.Code == http.StatusForbidden && veErrorCode(rr) == "email_not_verified" {
		t.Fatalf("%s: unexpectedly blocked with email_not_verified (body: %s)", what, rr.Body.String())
	}
}

// ---------------------------------------------------------------------
// Perimeter 1: /api/v1 core writes — session AND PAT.
// ---------------------------------------------------------------------

func TestVerifiedEmail_CoreWrite_Session_BlocksUnverified(t *testing.T) {
	f := newVerifiedEmailFixture(t, true)
	rr := doRequestWithCookie(f.srv, "POST", f.itemsPath(), map[string]any{"title": "blocked"}, f.unv.session)
	assertBlocked(t, rr, "unverified session item-create")
}

func TestVerifiedEmail_CoreWrite_PAT_BlocksUnverified(t *testing.T) {
	f := newVerifiedEmailFixture(t, true)
	rr := veDoBearer(f.srv, "POST", f.itemsPath(), map[string]any{"title": "blocked"}, f.unv.bearer)
	assertBlocked(t, rr, "unverified PAT item-create")
}

// ---------------------------------------------------------------------
// Cross-cutting invariant: a VERIFIED cloud user passes every perimeter.
// ---------------------------------------------------------------------

func TestVerifiedEmail_VerifiedUser_PassesAllPerimeters(t *testing.T) {
	f := newVerifiedEmailFixture(t, true)

	// Core write via session → 201.
	if rr := doRequestWithCookie(f.srv, "POST", f.itemsPath(), map[string]any{"title": "ok-session"}, f.ver.session); rr.Code != http.StatusCreated {
		t.Fatalf("verified session item-create: expected 201, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// Core write via PAT → 201.
	if rr := veDoBearer(f.srv, "POST", f.itemsPath(), map[string]any{"title": "ok-pat"}, f.ver.bearer); rr.Code != http.StatusCreated {
		t.Fatalf("verified PAT item-create: expected 201, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// Token create → not email-blocked (should succeed).
	if rr := doRequestWithCookie(f.srv, "POST", "/api/v1/auth/tokens", map[string]any{"name": "t"}, f.ver.session); rr.Code == http.StatusForbidden && veErrorCode(rr) == "email_not_verified" {
		t.Fatalf("verified token-create unexpectedly email-blocked (body: %s)", rr.Body.String())
	}
	// PATCH /me → not email-blocked.
	rr := doRequestWithCookie(f.srv, "PATCH", "/api/v1/auth/me", map[string]any{"name": "Verified Renamed"}, f.ver.session)
	assertNotEmailBlocked(t, rr, "verified PATCH /me")
	// import/url → not email-blocked (may 400/upstream, but never our gate).
	rr = doRequestWithCookie(f.srv, "POST", "/api/v1/import/url", map[string]any{"url": "https://example.com"}, f.ver.session)
	assertNotEmailBlocked(t, rr, "verified import/url")
}

// ---------------------------------------------------------------------
// Cross-cutting invariant: self-host (!cloudMode) is entirely unaffected.
// ---------------------------------------------------------------------

func TestVerifiedEmail_SelfHost_NoOp(t *testing.T) {
	f := newVerifiedEmailFixture(t, false) // NOT cloud mode

	// An unverified user on a self-hosted instance writes freely — the
	// middleware collapses to pass-through.
	if rr := doRequestWithCookie(f.srv, "POST", f.itemsPath(), map[string]any{"title": "selfhost-ok"}, f.unv.session); rr.Code != http.StatusCreated {
		t.Fatalf("self-host unverified item-create: expected 201, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if rr := veDoBearer(f.srv, "POST", f.itemsPath(), map[string]any{"title": "selfhost-ok-pat"}, f.unv.bearer); rr.Code != http.StatusCreated {
		t.Fatalf("self-host unverified PAT item-create: expected 201, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// Token-minting paths also stay open on self-host.
	if rr := doRequestWithCookie(f.srv, "POST", "/api/v1/auth/tokens", map[string]any{"name": "t"}, f.unv.session); rr.Code == http.StatusForbidden && veErrorCode(rr) == "email_not_verified" {
		t.Fatalf("self-host unverified token-create unexpectedly blocked (body: %s)", rr.Body.String())
	}
}

// ---------------------------------------------------------------------
// Perimeter 2: authenticated /auth/* token-minting + profile mutations.
// ---------------------------------------------------------------------

func TestVerifiedEmail_TokenCreate_BlocksUnverified(t *testing.T) {
	f := newVerifiedEmailFixture(t, true)
	rr := doRequestWithCookie(f.srv, "POST", "/api/v1/auth/tokens", map[string]any{"name": "sneaky"}, f.unv.session)
	assertBlocked(t, rr, "unverified token-create")
}

func TestVerifiedEmail_PatchMe_BlocksUnverified(t *testing.T) {
	f := newVerifiedEmailFixture(t, true)
	rr := doRequestWithCookie(f.srv, "PATCH", "/api/v1/auth/me", map[string]any{"name": "Renamed"}, f.unv.session)
	assertBlocked(t, rr, "unverified PATCH /me")
}

func TestVerifiedEmail_CLISessionApprove_BlocksUnverified(t *testing.T) {
	f := newVerifiedEmailFixture(t, true)
	// A pending CLI session so the block can't be attributed to a 404.
	sess, err := f.srv.store.CreateCLIAuthSession()
	if err != nil {
		t.Fatalf("CreateCLIAuthSession: %v", err)
	}
	rr := doRequestWithCookie(f.srv, "POST", "/api/v1/auth/cli/sessions/"+sess.Code+"/approve", nil, f.unv.session)
	assertBlocked(t, rr, "unverified cli-session approve")

	// Verified user reaches the handler (session gets minted).
	rr = doRequestWithCookie(f.srv, "POST", "/api/v1/auth/cli/sessions/"+sess.Code+"/approve", nil, f.ver.session)
	assertNotEmailBlocked(t, rr, "verified cli-session approve")
}

// ---------------------------------------------------------------------
// Perimeter 3: POST /api/v1/import/url (SSRF/abuse surface).
// ---------------------------------------------------------------------

func TestVerifiedEmail_ImportURL_BlocksUnverified(t *testing.T) {
	f := newVerifiedEmailFixture(t, true)
	rr := doRequestWithCookie(f.srv, "POST", "/api/v1/import/url", map[string]any{"url": "https://example.com"}, f.unv.session)
	assertBlocked(t, rr, "unverified import/url")
}

// ---------------------------------------------------------------------
// Reads + the account-lifecycle carve-outs stay open for unverified.
// ---------------------------------------------------------------------

func TestVerifiedEmail_Reads_AllowedForUnverified(t *testing.T) {
	f := newVerifiedEmailFixture(t, true)
	// GET /me (session identity) works.
	if rr := doRequestWithCookie(f.srv, "GET", "/api/v1/auth/me", nil, f.unv.session); rr.Code != http.StatusOK {
		t.Fatalf("unverified GET /me: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// GET item list works.
	if rr := doRequestWithCookie(f.srv, "GET", f.itemsPath(), nil, f.unv.session); rr.Code != http.StatusOK {
		t.Fatalf("unverified GET items: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestVerifiedEmail_Logout_AllowedForUnverified(t *testing.T) {
	f := newVerifiedEmailFixture(t, true)
	rr := doRequestWithCookie(f.srv, "POST", "/api/v1/auth/logout", nil, f.unv.session)
	assertNotEmailBlocked(t, rr, "unverified logout")
	if rr.Code != http.StatusOK {
		t.Fatalf("unverified logout: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestVerifiedEmail_DeleteAccount_AllowedForUnverified(t *testing.T) {
	f := newVerifiedEmailFixture(t, true)
	// delete-account requires the account password in the body.
	rr := doRequestWithCookie(f.srv, "POST", "/api/v1/auth/delete-account",
		map[string]any{"password": "correct-horse-battery-staple"}, f.unv.session)
	assertNotEmailBlocked(t, rr, "unverified delete-account")
}

// ---------------------------------------------------------------------
// Carve-out: POST /api/v1/invitations/{code}/accept works unverified.
// ---------------------------------------------------------------------

func TestVerifiedEmail_InvitationAccept_CarveOut(t *testing.T) {
	f := newVerifiedEmailFixture(t, true)

	// A second workspace the unverified user is NOT yet a member of,
	// with an invitation bound to their email.
	otherWS, err := f.srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Invite Target"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	inv, err := f.srv.store.CreateInvitation(otherWS.ID, f.unv.email, "editor", f.unv.id)
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}

	rr := doRequestWithCookie(f.srv, "POST", "/api/v1/invitations/"+inv.Code+"/accept", nil, f.unv.session)
	assertNotEmailBlocked(t, rr, "unverified invitation-accept")
	if rr.Code != http.StatusOK {
		t.Fatalf("unverified invitation-accept: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------
// Perimeter 4: collab WebSocket GET-upgrade (persists Yjs edits).
// ---------------------------------------------------------------------

// veCollabMember seeds ws+coll+item and returns the itemID plus a
// session token for a member with the given verification state.
func veCollabMember(t *testing.T, srv *Server, wsName, email string, unverified bool) (itemID, sessionToken string) {
	t.Helper()
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: wsName})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Doc", Fields: `{}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	u, err := srv.store.CreateUser(models.UserCreate{
		Email: email, Name: email, Password: "correct-horse-battery-staple", Role: "member", Unverified: unverified,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	tok, err := srv.store.CreateSession(u.ID, "ve", "127.0.0.1", "ve-ua", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return item.ID, tok
}

func TestVerifiedEmail_CollabUpgrade_BlocksUnverified(t *testing.T) {
	srv := testServerWithCollab(t)
	srv.SetCloudMode("ve-secret")
	bootstrapFirstUser(t, srv, "admin@ve.test", "Admin")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	itemID, sessionToken := veCollabMember(t, srv, "Collab Unverified", "collab-unv@ve.test", true)

	cookies := []*http.Cookie{{Name: "pad_session", Value: sessionToken}}
	_, resp, err := dialCollab(t, ts.URL, itemID, cookies, "ve-ua")
	if err == nil {
		t.Fatal("expected collab dial to fail for unverified member")
	}
	if resp == nil {
		t.Fatalf("dial returned no response: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Error.Code != "email_not_verified" {
		t.Fatalf("expected email_not_verified, got %q", env.Error.Code)
	}
}

func TestVerifiedEmail_CollabUpgrade_AllowsVerified(t *testing.T) {
	srv := testServerWithCollab(t)
	srv.SetCloudMode("ve-secret")
	bootstrapFirstUser(t, srv, "admin@ve.test", "Admin")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	itemID, sessionToken := veCollabMember(t, srv, "Collab Verified", "collab-ver@ve.test", false)

	cookies := []*http.Cookie{{Name: "pad_session", Value: sessionToken}}
	conn, resp, err := dialCollab(t, ts.URL, itemID, cookies, "ve-ua")
	if err != nil {
		body := ""
		if resp != nil {
			body = "status=" + resp.Status
		}
		t.Fatalf("verified collab dial: %v (%s)", err, body)
	}
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------
// Perimeter 5: OAuth-provider authorize + decide (mints auth codes).
// ---------------------------------------------------------------------

// veOAuthUnverifiedSession creates an unverified user + session on an
// OAuth-enabled server.
func veOAuthUnverifiedSession(t *testing.T, srv *Server, email string) string {
	t.Helper()
	u, err := srv.store.CreateUser(models.UserCreate{
		Email: email, Name: email, Password: "correct-horse-battery-staple", Unverified: true,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	tok, err := srv.store.CreateSession(u.ID, "ve", "192.0.2.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return tok
}

func TestVerifiedEmail_OAuthAuthorizeDecide_BlocksUnverified(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	sessionToken := veOAuthUnverifiedSession(t, srv, "oauth-unv@ve.test")
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	form := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"scope":                 {"pad:read"},
		"code_challenge":        {s256Challenge("verifier-the-quick-brown-fox-1234567890-abcdef-1234")},
		"code_challenge_method": {"S256"},
		"audience":              {testCanonicalAudience},
		"state":                 {"test-state-12345"},
		"decision":              {"approve"},
	}
	// Verified check runs before CSRF/authorize-request parsing, so no
	// csrf_token is needed to reach it.
	req := httptest.NewRequest("POST", "/oauth/authorize/decide", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: sessionToken})
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assertBlocked(t, rr, "unverified oauth authorize/decide")
}

func TestVerifiedEmail_OAuthAuthorize_BlocksUnverified(t *testing.T) {
	srv, _ := oauthEnabledTestServer(t)
	sessionToken := veOAuthUnverifiedSession(t, srv, "oauth-unv2@ve.test")
	clientID := registerTestClient(t, srv, "https://app.test/cb")

	q := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {"https://app.test/cb"},
		"scope":                 {"pad:read"},
		"code_challenge":        {s256Challenge("verifier-the-quick-brown-fox-1234567890-abcdef-1234")},
		"code_challenge_method": {"S256"},
		"audience":              {testCanonicalAudience},
		"state":                 {"authorize-state-12345"},
	}
	rr := doAuthedRequest(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sessionToken)
	assertBlocked(t, rr, "unverified oauth authorize render")
}
