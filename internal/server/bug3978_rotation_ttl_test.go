package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3978: a credential change rotates the caller's session, and the
// replacement keeps the replaced session's own sliding window. It used to be
// minted with the 7-day web TTL whatever it replaced, so a password change
// made from a 30-day CLI token left the CLI with a 7-day one.

// rotateWithBearer rotates as a credential-change handler does for a caller
// that authenticated with a session bearer (the CLI).
func rotateWithBearer(t *testing.T, srv *Server, u *models.User, token string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req = req.WithContext(context.WithValue(req.Context(), ctxAuthKind, authKindSessionBearer))
	if _, ok := srv.rotateSessionsAfterCredentialChange(httptest.NewRecorder(), req, u); !ok {
		t.Fatal("rotation failed")
	}
}

// onlySession reads the user's one remaining session: a rotation revokes
// every other one.
func onlySession(t *testing.T, srv *Server, userID string) (expiresIn time.Duration, renewTTL time.Duration, kind string) {
	t.Helper()
	rows, err := srv.store.DB().Query(`SELECT expires_at, renew_ttl_seconds, kind FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var expiresAt string
		var renewSeconds int64
		if err := rows.Scan(&expiresAt, &renewSeconds, &kind); err != nil {
			t.Fatal(err)
		}
		exp, err := time.Parse(time.RFC3339, expiresAt)
		if err != nil {
			t.Fatalf("expires_at %q: %v", expiresAt, err)
		}
		expiresIn, renewTTL = time.Until(exp), time.Duration(renewSeconds)*time.Second
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the user has %d sessions after a rotation, want 1", n)
	}
	return expiresIn, renewTTL, kind
}

func near(got, want time.Duration) bool {
	d := got - want
	return d > -2*time.Minute && d < 2*time.Minute
}

// A browser-approved CLI token (30-day slide) keeps its 30 days through a
// credential change.
func TestBUG3978_CLITokenKeepsItsWindowThroughRotation(t *testing.T) {
	t.Parallel()
	srv, u, _ := oauthOnlyCloud(t)
	cli, err := srv.store.CreateSession(u.ID, "cli-browser-auth", "198.51.100.7", "", cliSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	rotateWithBearer(t, srv, u, cli)
	expiresIn, renewTTL, kind := onlySession(t, srv, u.ID)
	if kind != "cli" {
		t.Fatalf("the rotated session is kind %q, want cli", kind)
	}
	if renewTTL != cliSessionTTL {
		t.Fatalf("the rotated CLI session slides by %v, want %v", renewTTL, cliSessionTTL)
	}
	if !near(expiresIn, cliSessionTTL) {
		t.Fatalf("the rotated CLI session expires in %v, want about %v", expiresIn, cliSessionTTL)
	}
}

// A CLI session with a 7-day window (the shape `pad auth login -i` mints)
// keeps 7 days: a rotation changes no session's window, so it never
// lengthens one either.
func TestBUG3978_RotationDoesNotLengthenAWindow(t *testing.T) {
	t.Parallel()
	srv, u, _ := oauthOnlyCloud(t)
	cli, err := srv.store.CreateSession(u.ID, "cli", "198.51.100.7", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	rotateWithBearer(t, srv, u, cli)
	expiresIn, renewTTL, kind := onlySession(t, srv, u.ID)
	if kind != "cli" {
		t.Fatalf("the rotated session is kind %q, want cli", kind)
	}
	if renewTTL != webSessionTTL || !near(expiresIn, webSessionTTL) {
		t.Fatalf("a 7-day CLI session rotated to slide %v, expiring in %v; want 7 days for both", renewTTL, expiresIn)
	}
}

// A web session keeps its 7 days, and its cookie says so.
func TestBUG3978_WebSessionRotationUnchanged(t *testing.T) {
	t.Parallel()
	srv, u, _ := oauthOnlyCloud(t)
	web, err := srv.store.CreateSession(u.ID, "web", "198.51.100.7", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: web})
	rr := httptest.NewRecorder()
	if _, ok := srv.rotateSessionsAfterCredentialChange(rr, req, u); !ok {
		t.Fatal("rotation failed")
	}
	expiresIn, renewTTL, kind := onlySession(t, srv, u.ID)
	if kind != "web" || renewTTL != webSessionTTL || !near(expiresIn, webSessionTTL) {
		t.Fatalf("rotated web session: kind %q, slide %v, expires in %v; want web, 7 days, 7 days", kind, renewTTL, expiresIn)
	}
	var maxAge int
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookieName(srv.secureCookies) {
			maxAge = c.MaxAge
		}
	}
	if maxAge != int(webSessionTTL.Seconds()) {
		t.Fatalf("session cookie MaxAge %d, want %d", maxAge, int(webSessionTTL.Seconds()))
	}
}

// A legacy session with no sliding window (renew_ttl_seconds = 0) rotates to
// the web TTL, as every rotation did before.
func TestBUG3978_LegacyRowRotatesToTheWebTTL(t *testing.T) {
	t.Parallel()
	srv, u, _ := oauthOnlyCloud(t)
	cli, err := srv.store.CreateSession(u.ID, "cli-browser-auth", "198.51.100.7", "", cliSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.DB().Exec(`UPDATE sessions SET renew_ttl_seconds = 0 WHERE user_id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	rotateWithBearer(t, srv, u, cli)
	expiresIn, renewTTL, _ := onlySession(t, srv, u.ID)
	if renewTTL != webSessionTTL || !near(expiresIn, webSessionTTL) {
		t.Fatalf("a legacy session rotated to slide %v, expiring in %v; want 7 days for both", renewTTL, expiresIn)
	}
}

// A web session with a 30-day window (a browser bootstrap from /setup mints
// one with cliSessionTTL) keeps 30 days through a rotation, and so do its
// session and CSRF cookies, as createAuthSession sets them (BUG-3978 r1).
func TestBUG3978_LongWebSessionKeepsItsCookies(t *testing.T) {
	t.Parallel()
	srv, u, _ := oauthOnlyCloud(t)
	web, err := srv.store.CreateSession(u.ID, "web", "198.51.100.7", "", cliSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: web})
	rr := httptest.NewRecorder()
	if _, ok := srv.rotateSessionsAfterCredentialChange(rr, req, u); !ok {
		t.Fatal("rotation failed")
	}
	expiresIn, renewTTL, kind := onlySession(t, srv, u.ID)
	if kind != "web" || renewTTL != cliSessionTTL || !near(expiresIn, cliSessionTTL) {
		t.Fatalf("rotated 30-day web session: kind %q, slide %v, expires in %v; want web, 30 days, 30 days", kind, renewTTL, expiresIn)
	}
	want := int(cliSessionTTL.Seconds())
	got := map[string]int{}
	for _, c := range rr.Result().Cookies() {
		got[c.Name] = c.MaxAge
	}
	if got[sessionCookieName(srv.secureCookies)] != want || got[csrfCookieName(srv.secureCookies)] != want {
		t.Fatalf("cookie MaxAge: session %d, csrf %d; want %d for both", got[sessionCookieName(srv.secureCookies)], got[csrfCookieName(srv.secureCookies)], want)
	}
}
