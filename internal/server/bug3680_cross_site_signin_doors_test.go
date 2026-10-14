package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3680: every CSRF-exempt door that signs the browser in refuses a
// cross-site form, as login does (BUG-3350). Reset, register and bootstrap
// each set a web session cookie and were unguarded, so a cross-site
// text/plain form could sign a visitor into an account the attacker controls.
// Each door gets a refused leg that must change nothing, and a same-origin
// control that must complete.

type crossSiteCase struct {
	name, ct, origin string
	want             int
}

var crossSiteRefusals = []crossSiteCase{
	{"text/plain form", "text/plain", "", http.StatusUnsupportedMediaType},
	{"urlencoded form", "application/x-www-form-urlencoded", "", http.StatusUnsupportedMediaType},
	{"JSON from a foreign origin", "application/json", "https://evil.example", http.StatusForbidden},
	{"JSON from a null origin", "application/json", "null", http.StatusForbidden},
}

func postSignInDoor(srv *Server, path, body, ct, origin, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Host = "pad.example.test"
	req.RemoteAddr = remoteAddr
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func hasSessionCookie(rr *httptest.ResponseRecorder) bool {
	for _, c := range rr.Result().Cookies() {
		if strings.Contains(c.Name, "session") && c.Value != "" {
			return true
		}
	}
	return false
}

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBUG3680_ResetPasswordRefusesCrossSiteForms(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "1")
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "reset-3680@example.com", "R")
	user, err := srv.store.GetUserByEmail("reset-3680@example.com")
	if err != nil || user == nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	for _, c := range crossSiteRefusals {
		tok, err := srv.store.CreatePasswordReset(user.ID)
		if err != nil {
			t.Fatalf("CreatePasswordReset: %v", err)
		}
		body := jsonBody(t, map[string]string{"token": tok, "password": "attacker-chosen-passphrase-1"})
		rr := postSignInDoor(srv, "/api/v1/auth/reset-password", body, c.ct, c.origin, "192.0.2.7:1")
		if rr.Code != c.want {
			t.Errorf("%s: %d, want %d (%s)", c.name, rr.Code, c.want, rr.Body.String())
		}
		if hasSessionCookie(rr) {
			t.Errorf("%s: a refused reset set a session cookie", c.name)
		}
		// The refusal consumed nothing: the same token still completes a
		// same-origin reset.
		ctl := postSignInDoor(srv, "/api/v1/auth/reset-password", body, "application/json", "http://pad.example.test", "192.0.2.7:1")
		if ctl.Code != http.StatusOK || !hasSessionCookie(ctl) {
			t.Errorf("%s: the token did not survive the refusal: same-origin reset %d (%s)", c.name, ctl.Code, ctl.Body.String())
		}
	}
	// CONTROL: a headless client (no Origin, JSON) still resets.
	tok, err := srv.store.CreatePasswordReset(user.ID)
	if err != nil {
		t.Fatalf("CreatePasswordReset: %v", err)
	}
	body := jsonBody(t, map[string]string{"token": tok, "password": "headless-chosen-passphrase-2"})
	if rr := postSignInDoor(srv, "/api/v1/auth/reset-password", body, "application/json", "", "192.0.2.7:1"); rr.Code != http.StatusOK {
		t.Errorf("headless reset: %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
}

func TestBUG3680_RegisterRefusesCrossSiteForms(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "1")
	f := newAccessFixture(t, store.DriverSQLite)
	for i, c := range crossSiteRefusals {
		email := fmt.Sprintf("signup%d-3680@example.com", i)
		inv := f.invite(email, "editor")
		body := jsonBody(t, map[string]string{
			"email": email, "name": "Invitee", "password": "correct-horse-battery-staple", "invitation_code": inv.Code,
		})
		rr := postSignInDoor(f.srv, "/api/v1/auth/register", body, c.ct, c.origin, "192.0.2.7:1")
		if rr.Code != c.want {
			t.Errorf("%s: %d, want %d (%s)", c.name, rr.Code, c.want, rr.Body.String())
		}
		if hasSessionCookie(rr) {
			t.Errorf("%s: a refused signup set a session cookie", c.name)
		}
		if u, _ := f.srv.store.GetUserByEmail(email); u != nil {
			t.Errorf("%s: a refused signup created the account", c.name)
		}
		// CONTROL on the same invitation: same-origin JSON completes, so the
		// refusal above was the guard and not a broken fixture.
		if i == 0 {
			ctl := postSignInDoor(f.srv, "/api/v1/auth/register", body, "application/json", "http://pad.example.test", "192.0.2.7:1")
			if ctl.Code != http.StatusCreated || !hasSessionCookie(ctl) {
				t.Errorf("same-origin signup: %d, want 201 with a session (%s)", ctl.Code, ctl.Body.String())
			}
		}
	}
}

func TestBUG3680_BootstrapRefusesCrossSiteForms(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "1")
	srv := testServer(t)
	body := jsonBody(t, map[string]string{"email": "boot-3680@example.com", "name": "B", "password": "correct-horse-battery-staple"})
	for _, c := range crossSiteRefusals {
		rr := postSignInDoor(srv, "/api/v1/auth/bootstrap", body, c.ct, c.origin, "127.0.0.1:1")
		if rr.Code != c.want {
			t.Errorf("%s: %d, want %d (%s)", c.name, rr.Code, c.want, rr.Body.String())
		}
		if hasSessionCookie(rr) {
			t.Errorf("%s: a refused bootstrap set a session cookie", c.name)
		}
		if n, err := srv.store.UserCount(); err != nil || n != 0 {
			t.Fatalf("%s: a refused bootstrap created an admin (count %d, err %v)", c.name, n, err)
		}
	}
	// CONTROL: the CLI's shape (loopback, JSON, no Origin) still bootstraps.
	if rr := postSignInDoor(srv, "/api/v1/auth/bootstrap", body, "application/json", "", "127.0.0.1:1"); rr.Code != http.StatusCreated {
		t.Errorf("headless loopback bootstrap: %d, want 201 (%s)", rr.Code, rr.Body.String())
	}
}

// The legitimate callers the guard must keep working beyond the same-origin
// and headless controls above (Codex r2 on #2146): an allowed CORS origin,
// the X-Bootstrap-Token bootstrap from a remote host, and an admin creating
// an account from the SPA with its session.

func TestBUG3680_ResetPasswordAcceptsAllowedCORSOrigin(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "1")
	srv := testServer(t)
	srv.SetCORSOrigins("https://app.example.test")
	bootstrapFirstUser(t, srv, "cors-3680@example.com", "C")
	user, err := srv.store.GetUserByEmail("cors-3680@example.com")
	if err != nil || user == nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	tok, err := srv.store.CreatePasswordReset(user.ID)
	if err != nil {
		t.Fatalf("CreatePasswordReset: %v", err)
	}
	body := jsonBody(t, map[string]string{"token": tok, "password": "cors-chosen-passphrase-3"})
	// Refused first from an origin NOT on the list, so the accept below is the
	// list doing it and not a guard that ignores Origin once CORS is set.
	if rr := postSignInDoor(srv, "/api/v1/auth/reset-password", body, "application/json", "https://other.example.test", "192.0.2.7:1"); rr.Code != http.StatusForbidden {
		t.Errorf("unlisted origin: %d, want 403 (%s)", rr.Code, rr.Body.String())
	}
	if rr := postSignInDoor(srv, "/api/v1/auth/reset-password", body, "application/json", "https://app.example.test", "192.0.2.7:1"); rr.Code != http.StatusOK || !hasSessionCookie(rr) {
		t.Errorf("allowed CORS origin: %d, want 200 with a session (%s)", rr.Code, rr.Body.String())
	}
}

func TestBUG3680_BootstrapTokenHeaderFromRemoteStillWorks(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "1")
	srv := testServer(t)
	srv.SetBootstrapToken("bug3680-bootstrap-token", t.TempDir()+"/bootstrap-token")
	body := jsonBody(t, map[string]string{"email": "remote-boot-3680@example.com", "name": "R", "password": "correct-horse-battery-staple"})
	post := func(ct, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/v1/auth/bootstrap", strings.NewReader(body))
		req.Host = "pad.example.test"
		req.RemoteAddr = "198.51.100.9:1"
		req.Header.Set("Content-Type", ct)
		req.Header.Set(BootstrapTokenHeader, "bug3680-bootstrap-token")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}
	// The token does not lift the guard: a cross-site form carrying it is
	// still refused, and consumes nothing.
	if rr := post("text/plain", "https://evil.example"); rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("cross-site form with the token: %d, want 415 (%s)", rr.Code, rr.Body.String())
	}
	rr := post("application/json", "")
	if rr.Code != http.StatusCreated || !hasSessionCookie(rr) {
		t.Errorf("remote bootstrap with the token header: %d, want 201 with a session (%s)", rr.Code, rr.Body.String())
	}
	if u, err := srv.store.GetUserByEmail("remote-boot-3680@example.com"); err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	} else if u == nil {
		t.Error("the remote token bootstrap did not create the admin")
	}
}

func TestBUG3680_AdminRegisterFromTheSPA(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "1")
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin-3680@example.com", "A")
	const testCSRF = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	post := func(email, origin string) *httptest.ResponseRecorder {
		body := jsonBody(t, map[string]string{"email": email, "name": "New", "password": "correct-horse-battery-staple"})
		req := httptest.NewRequest("POST", "/api/v1/auth/register", strings.NewReader(body))
		req.Host = "pad.example.test"
		req.RemoteAddr = "192.0.2.7:1"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		req.AddCookie(&http.Cookie{Name: "pad_session", Value: adminToken})
		req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: testCSRF})
		req.Header.Set("X-CSRF-Token", testCSRF)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}
	if rr := post("admin-made-1-3680@example.com", "https://evil.example"); rr.Code != http.StatusForbidden {
		t.Errorf("admin register from a foreign origin: %d, want 403 (%s)", rr.Code, rr.Body.String())
	}
	if u, err := srv.store.GetUserByEmail("admin-made-1-3680@example.com"); err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	} else if u != nil {
		t.Error("a refused admin register created the account")
	}
	if rr := post("admin-made-2-3680@example.com", "http://pad.example.test"); rr.Code != http.StatusCreated {
		t.Errorf("admin register from the SPA (same origin): %d, want 201 (%s)", rr.Code, rr.Body.String())
	}
	if u, err := srv.store.GetUserByEmail("admin-made-2-3680@example.com"); err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	} else if u == nil {
		t.Error("the admin's SPA register did not create the account")
	}
}
