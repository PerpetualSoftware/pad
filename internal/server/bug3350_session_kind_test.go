package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3350 (a): a session records how it was issued, and a CLI session is
// never accepted as a browser cookie.

func TestBUG3350_CLISessionIsRefusedAsACookie(t *testing.T) {
	srv := testServer(t)
	admin, err := srv.store.CreateUser(models.UserCreate{Email: "kind-3350@example.com", Name: "K", Password: "correct-horse-battery-staple", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, device := range []string{"cli-browser-auth", "cli"} {
		tok, err := srv.store.CreateSession(admin.ID, device, "192.0.2.1", "", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if info, _ := srv.store.ValidateSession(tok); info == nil || info.Kind != store.SessionKindCLI {
			t.Fatalf("%s session kind: %+v, want cli", device, info)
		}
		// As a cookie it is no credential at all: no cookie-only admin bypass.
		if rr := doRequestWithCookie(srv, "GET", "/api/v1/auth/me", nil, tok); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s session as a cookie on /auth/me: %d, want 401", device, rr.Code)
		}
		// As the bearer it was issued as, it works.
		if rr := doRequestWithBearer(srv, "GET", "/api/v1/auth/me", tok, nil); rr.Code != http.StatusOK {
			t.Errorf("%s session as a bearer on /auth/me: %d, want 200", device, rr.Code)
		}
	}
	web, err := srv.store.CreateSession(admin.ID, "web", "192.0.2.1", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rr := doRequestWithCookie(srv, "GET", "/api/v1/auth/me", nil, web); rr.Code != http.StatusOK {
		t.Fatalf("web session as a cookie: %d, want 200", rr.Code)
	}
}

// The CLI marks its sign-ins: it gets a CLI session and no cookie. The
// browser's sign-in is unchanged.
func TestBUG3350_CLISignInMintsACLISession(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "signin-3350@example.com", "S")
	login := func(cli bool) *httptest.ResponseRecorder {
		headers := map[string]string{}
		if cli {
			headers["X-Pad-Client"] = "cli"
		}
		return doRequestWithHeaders(srv, "POST", "/api/v1/auth/login",
			map[string]any{"email": "signin-3350@example.com", "password": "correct-horse-battery-staple"}, headers)
	}
	for _, c := range []struct {
		cli      bool
		kind     string
		cookieOK bool
	}{{true, store.SessionKindCLI, false}, {false, store.SessionKindWeb, true}} {
		rr := login(c.cli)
		if rr.Code != http.StatusOK {
			t.Fatalf("cli=%v login: %d %s", c.cli, rr.Code, rr.Body.String())
		}
		var body struct {
			Token string `json:"token"`
		}
		parseJSON(t, rr, &body)
		info, _ := srv.store.ValidateSession(body.Token)
		if info == nil || info.Kind != c.kind {
			t.Errorf("cli=%v login minted %+v, want kind %s", c.cli, info, c.kind)
		}
		setsCookie := strings.Contains(strings.Join(rr.Header().Values("Set-Cookie"), ";"), "pad_session")
		if setsCookie != c.cookieOK {
			t.Errorf("cli=%v login set a session cookie: %v, want %v", c.cli, setsCookie, c.cookieOK)
		}
	}
}

// A credential-change rotation keeps the replaced session's kind.
func TestBUG3350_RotationKeepsTheKind(t *testing.T) {
	srv := testServer(t)
	u, err := srv.store.CreateUser(models.UserCreate{Email: "rot-3350@example.com", Name: "R", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := srv.store.CreateSession(u.ID, "cli", "192.0.2.1", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req = req.WithContext(withAuthKindForTest(req, authKindSessionBearer))
	rec := httptest.NewRecorder()
	rotated, ok := srv.rotateSessionsAfterCredentialChange(rec, req, u)
	if !ok {
		t.Fatal("rotation failed")
	}
	if info, _ := srv.store.ValidateSession(rotated); info == nil || info.Kind != store.SessionKindCLI {
		t.Fatalf("rotated CLI session: %+v, want kind cli", info)
	}
	if strings.Contains(strings.Join(rec.Header().Values("Set-Cookie"), ";"), "pad_session") {
		t.Fatal("rotating a CLI session set a session cookie")
	}
}

// Migration 105's backfill: the browser-approval rows become cli, the rest
// stay web.
func TestBUG3350_MigrationBackfillsCLISessions(t *testing.T) {
	srv := testServer(t)
	u, err := srv.store.CreateUser(models.UserCreate{Email: "mig-3350@example.com", Name: "M", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	cliTok, _ := srv.store.CreateSession(u.ID, "cli-browser-auth", "192.0.2.1", "", time.Hour)
	webTok, _ := srv.store.CreateSession(u.ID, "web", "192.0.2.1", "", time.Hour)
	// Rows as they stood before the migration: kind at its default.
	if _, err := srv.store.DB().Exec(`UPDATE sessions SET kind = 'web' WHERE user_id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	sql, err := os.ReadFile("../store/migrations/105_session_kind.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range strings.Split(string(sql), ";") {
		if strings.Contains(stmt, "UPDATE sessions") {
			if _, err := srv.store.DB().Exec(stmt); err != nil {
				t.Fatalf("backfill: %v", err)
			}
		}
	}
	if info, _ := srv.store.ValidateSession(cliTok); info == nil || info.Kind != store.SessionKindCLI {
		t.Errorf("backfilled browser-approval session: %+v, want cli", info)
	}
	if info, _ := srv.store.ValidateSession(webTok); info == nil || info.Kind != store.SessionKindWeb {
		t.Errorf("backfilled web session: %+v, want web", info)
	}
}

// BUG-3350 (b): sign-in refuses a cross-site form. It must be JSON, and an
// Origin the browser names must be this server or an allowed CORS origin.
func TestBUG3350_SignInRefusesCrossSiteForms(t *testing.T) {
	// Seven sign-ins from one address; the auth limiter is not under test.
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "1")
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "csrf-3350@example.com", "C")
	body := `{"email":"csrf-3350@example.com","password":"correct-horse-battery-staple"}`
	post := func(contentType, origin string) int {
		req := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(body))
		req.Host = "pad.example.test"
		req.RemoteAddr = "192.0.2.7:1"
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr.Code
	}
	for _, c := range []struct {
		name, ct, origin string
		want             int
	}{
		{"text/plain form", "text/plain", "", http.StatusUnsupportedMediaType},
		{"urlencoded form", "application/x-www-form-urlencoded", "", http.StatusUnsupportedMediaType},
		{"JSON from a foreign origin", "application/json", "https://evil.example", http.StatusForbidden},
		{"JSON from a null origin", "application/json", "null", http.StatusForbidden},
		{"JSON from this server", "application/json", "http://pad.example.test", http.StatusOK},
		{"JSON from the dev server (localhost default)", "application/json", "http://localhost:5173", http.StatusOK},
		{"JSON with no Origin (CLI, curl)", "application/json", "", http.StatusOK},
		{"userinfo smuggling a foreign host past the localhost default", "application/json", "http://localhost:80@evil.example", http.StatusForbidden},
		{"userinfo in front of an allowed host", "application/json", "http://evil.example@localhost:5173", http.StatusForbidden},
		{"a look-alike localhost host", "application/json", "http://localhost.evil.example:5173", http.StatusForbidden},
		{"this host, upper-cased", "application/json", "HTTP://PAD.EXAMPLE.TEST", http.StatusOK},
		{"this host over https, TLS ended upstream", "application/json", "https://pad.example.test", http.StatusOK},
	} {
		if got := post(c.ct, c.origin); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

// withAuthKindForTest records the credential kind as the auth middleware
// would, for a request driven straight at a handler helper.
func withAuthKindForTest(r *http.Request, kind string) context.Context {
	return context.WithValue(r.Context(), ctxAuthKind, kind)
}

// An upper-cased CORS pattern still admits the origin it names, as the CORS
// middleware itself does (go-chi/cors normalizes case).
func TestBUG3350_SignInOriginPatternsIgnoreCase(t *testing.T) {
	srv := testServer(t)
	srv.SetCORSOrigins("HTTP://LOCALHOST:*")
	req := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	req.Host = "pad.example.test"
	if !srv.signInOriginAllowed("http://localhost:5173", req) {
		t.Fatal("an upper-cased CORS pattern refused the origin it names")
	}
}

// The second sign-in step takes the same guard.
func TestBUG3350_TOTPVerifyRefusesCrossSiteForms(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "1")
	srv := testServer(t)
	for _, c := range []struct {
		name, ct, origin string
		want             int
	}{
		{"text/plain form", "text/plain", "", http.StatusUnsupportedMediaType},
		{"JSON from a foreign origin", "application/json", "https://evil.example", http.StatusForbidden},
	} {
		req := httptest.NewRequest("POST", "/api/v1/auth/2fa/login-verify", strings.NewReader(`{"challenge_token":"x","code":"000000"}`))
		req.Header.Set("Content-Type", c.ct)
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != c.want {
			t.Errorf("2FA verify, %s: %d, want %d", c.name, rr.Code, c.want)
		}
	}
}

// Equivalent spellings a browser normalizes match their configured form: an
// IPv6 literal and a port with leading zeros.
func TestBUG3350_SignInOriginCanonicalizesHostAndPort(t *testing.T) {
	srv := testServer(t)
	srv.SetCORSOrigins("http://[0:0:0:0:0:0:0:1]:5173,https://frontend.example:0443")
	req := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	req.Host = "pad.example.test"
	for _, origin := range []string{"http://[::1]:5173", "https://frontend.example"} {
		if !srv.signInOriginAllowed(origin, req) {
			t.Errorf("%s was refused against its configured equivalent", origin)
		}
	}
	// An https server is never posted to from an http page of its own host.
	tls := httptest.NewRequest("POST", "https://pad.example.test/api/v1/auth/login", nil)
	if srv.signInOriginAllowed("http://pad.example.test", tls) {
		t.Error("an http Origin was accepted for an https request")
	}
}
