package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3232: an admin creating another user's account is not signing in as
// them, so the admin-created register path mints no session: no token in the
// body, no session cookie, no session row. The invitation and self-serve paths
// are the new user signing up and keep their session (pinned below).

func sessionRowsFor(t *testing.T, srv *Server, email string) int {
	t.Helper()
	u, err := srv.store.GetUserByEmail(email)
	if err != nil || u == nil {
		t.Fatalf("GetUserByEmail %s: %v", email, err)
	}
	var n int
	if err := srv.store.DB().QueryRow("SELECT COUNT(*) FROM sessions WHERE user_id = ?", u.ID).Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return n
}

func setsSessionCookie(srv *Server, rr *httptest.ResponseRecorder) bool {
	for _, c := range rr.Result().Cookies() {
		if (c.Name == sessionCookieName(srv.secureCookies) || c.Name == sessionCookieName(false)) && c.Value != "" {
			return true
		}
	}
	return false
}

func assertNoSessionMinted(t *testing.T, srv *Server, rr *httptest.ResponseRecorder, email string) {
	t.Helper()
	if rr.Code != http.StatusCreated {
		t.Fatalf("admin register: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]interface{}
	parseJSON(t, rr, &resp)
	if _, ok := resp["token"]; ok {
		t.Errorf("admin-created register returned a token for the new user: %v", resp["token"])
	}
	if u, _ := resp["user"].(map[string]interface{}); u == nil || u["email"] != email {
		t.Errorf("expected the created user in the response, got %v", resp["user"])
	}
	if setsSessionCookie(srv, rr) {
		t.Error("admin-created register set a session cookie")
	}
	if n := sessionRowsFor(t, srv, email); n != 0 {
		t.Errorf("admin-created register left %d session row(s) for the new user, want 0", n)
	}
}

func TestRegister_AdminBearer_MintsNoSession(t *testing.T) {
	srv := testServer(t)
	adminTok := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr := doRequestWithBearer(srv, http.MethodPost, "/api/v1/auth/register", adminTok, map[string]string{
		"email": "made@test.com", "name": "Made", "password": "correct-horse-battery-staple",
	})
	assertNoSessionMinted(t, srv, rr, "made@test.com")

	// The new user signs in on their own, and gets exactly their own session.
	if rec := doRequest(srv, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "made@test.com", "password": "correct-horse-battery-staple",
	}); rec.Code != http.StatusOK {
		t.Fatalf("new user login: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := sessionRowsFor(t, srv, "made@test.com"); n != 1 {
		t.Errorf("after the new user's own login: %d session rows, want 1", n)
	}
}

// A cookie-authenticated admin is not switched to the new user, and keeps
// its own session (before BUG-3232 the response overwrote its cookie, and
// since BUG-3011 that also destroyed the admin's row).
func TestRegister_AdminCookie_KeepsAdminSession(t *testing.T) {
	srv := testServer(t)
	adminTok := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr := doRequestWithCookie(srv, http.MethodPost, "/api/v1/auth/register", map[string]string{
		"email": "made@test.com", "name": "Made", "password": "correct-horse-battery-staple",
	}, adminTok)
	assertNoSessionMinted(t, srv, rr, "made@test.com")

	if !sessionAlive(t, srv, adminTok) {
		t.Error("the admin's own session was destroyed by creating an account")
	}
	if rec := doRequestWithCookie(srv, http.MethodGet, "/api/v1/auth/me", nil, adminTok); rec.Code != http.StatusOK {
		t.Errorf("admin cookie after register: expected 200 from /auth/me, got %d", rec.Code)
	}
}

// Pin: an invitation signup is the new user signing up, and still gets a
// session: token in the body, the cookie set, and a live row.
func TestRegister_Invitation_StillSignsIn(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	admin, err := srv.store.GetUserByEmail("admin@test.com")
	if err != nil || admin == nil {
		t.Fatalf("load admin: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Inv", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	inv, err := srv.store.CreateInvitation(ws.ID, "invitee@test.com", "viewer", admin.ID)
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}

	rr := doRequest(srv, http.MethodPost, "/api/v1/auth/register", map[string]string{
		"email": "invitee@test.com", "name": "Invitee", "password": "correct-horse-battery-staple",
		"invitation_code": inv.Code,
	})
	assertSignedIn(t, srv, rr, "invitee@test.com")
}

// Pin: cloud self-serve signup is the new user signing up, and still gets a
// session.
func TestRegister_SelfServe_StillSignsIn(t *testing.T) {
	srv, _ := newCloudEmailServer(t)
	rr := doRequest(srv, http.MethodPost, "/api/v1/auth/register", map[string]string{
		"email": "newbie@pad.test", "name": "Newbie", "password": "correct-horse-battery-staple",
	})
	assertSignedIn(t, srv, rr, "newbie@pad.test")
}

func assertSignedIn(t *testing.T, srv *Server, rr *httptest.ResponseRecorder, email string) {
	t.Helper()
	if rr.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
	}
	parseJSON(t, rr, &resp)
	if resp.Token == "" {
		t.Fatal("expected a session token")
	}
	if !setsSessionCookie(srv, rr) {
		t.Error("expected the session cookie to be set")
	}
	if !sessionAlive(t, srv, resp.Token) {
		t.Error("the returned token is not a live session")
	}
	if n := sessionRowsFor(t, srv, email); n != 1 {
		t.Errorf("%d session rows for the new user, want 1", n)
	}
}
