package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3938: push device registration and notification settings. Each test
// that touches SQL runs on SQLite and, under `make test-pg`, on Postgres
// (needsBackends).

const pushTestGateway = "http://gateway.test/send"

type pushEnv struct {
	t     *testing.T
	srv   *Server
	user  *models.User
	token string // the bootstrap session's bearer
}

func newPushEnv(t *testing.T, srv *Server, gateway bool) *pushEnv {
	t.Helper()
	if gateway {
		srv.SetPushGateway(pushTestGateway)
	}
	tok := bootstrapFirstUser(t, srv, "push@example.com", "Push Person")
	u, err := srv.store.GetUserByEmail("push@example.com")
	if err != nil || u == nil {
		t.Fatalf("GetUserByEmail: %v %v", u, err)
	}
	return &pushEnv{t: t, srv: srv, user: u, token: tok}
}

// extraSession is a second live session of the user, as a second device has.
func (e *pushEnv) extraSession() (token, id string) {
	e.t.Helper()
	tok, err := e.srv.store.CreateSession(e.user.ID, "web", "", "", time.Hour)
	if err != nil {
		e.t.Fatal(err)
	}
	info, err := e.srv.store.ValidateSession(tok)
	if err != nil || info == nil {
		e.t.Fatalf("ValidateSession: %v %v", info, err)
	}
	return tok, info.ID
}

func (e *pushEnv) sessionID(token string) string {
	e.t.Helper()
	info, err := e.srv.store.ValidateSession(token)
	if err != nil || info == nil {
		e.t.Fatalf("ValidateSession: %v %v", info, err)
	}
	return info.ID
}

func (e *pushEnv) devices() map[string]string {
	e.t.Helper()
	ds, err := e.srv.store.ListPushDevices(e.user.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]string{}
	for _, d := range ds {
		out[d.Token] = d.SessionID
	}
	return out
}

// rawReq sends a request with an exact body. bearer and cookie are optional.
func rawReq(srv *Server, method, path, bearer, cookie, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = "127.0.0.1:1234"
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != "" {
		const csrf = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		req.AddCookie(&http.Cookie{Name: "pad_session", Value: cookie})
		req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: csrf})
		req.Header.Set("X-CSRF-Token", csrf)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func pushBody(token string) string {
	return `{"platform":"android","token":"` + token + `","app_version":"1.4"}`
}

func putDevice(srv *Server, bearer, conn, body string) *httptest.ResponseRecorder {
	return rawReq(srv, "PUT", "/api/v1/me/push-devices/"+conn, bearer, "", body)
}

// callPush runs a handler directly with a chosen auth kind and principal,
// the way the middleware chain would have left the request.
func callPush(h http.HandlerFunc, method, conn, body string, mutate func(r *http.Request) *http.Request) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v1/me/push-devices/"+conn, strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("connection_id", conn)
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	r = mutate(r)
	rr := httptest.NewRecorder()
	h(rr, r)
	return rr
}

func TestPushDevices_RegisterAndDelete(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		e := newPushEnv(t, srv, true)
		rr := putDevice(srv, e.token, "conn-1", pushBody("fcm-1"))
		if rr.Code != http.StatusOK {
			t.Fatalf("register: %d %s", rr.Code, rr.Body.String())
		}
		if got := e.devices(); len(got) != 1 || got["fcm-1"] != e.sessionID(e.token) {
			t.Fatalf("devices = %v, want fcm-1 on the caller's session", got)
		}
		// The cookie door works too.
		cookieTok, _ := e.extraSession()
		if rr := rawReq(srv, "PUT", "/api/v1/me/push-devices/conn-2", "", cookieTok, pushBody("fcm-2")); rr.Code != http.StatusOK {
			t.Fatalf("cookie register: %d %s", rr.Code, rr.Body.String())
		}
		if len(e.devices()) != 2 {
			t.Fatalf("devices = %v, want 2", e.devices())
		}
		// DELETE removes the caller's device only, and again is still a success.
		for i := 0; i < 2; i++ {
			if rr := rawReq(srv, "DELETE", "/api/v1/me/push-devices/conn-1", e.token, "", ""); rr.Code/100 != 2 {
				t.Fatalf("delete #%d: %d %s", i, rr.Code, rr.Body.String())
			}
		}
		if got := e.devices(); len(got) != 1 || got["fcm-2"] == "" {
			t.Fatalf("devices after delete = %v, want only fcm-2", got)
		}
	})
}

// Kills: a gate that lets a token, or a cookie riding with one, register.
func TestPushDevices_PATRejected(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		e := newPushEnv(t, srv, true)
		pat, err := srv.store.CreateAPIToken(e.user.ID, models.APITokenCreate{Name: "p"}, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		sessTok, _ := e.extraSession()
		assert403 := func(name string, rr *httptest.ResponseRecorder) {
			t.Helper()
			if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "session_required") {
				t.Errorf("%s: %d %s, want 403 session_required", name, rr.Code, rr.Body.String())
			}
		}
		assert403("PAT", putDevice(srv, pat.Token, "c1", pushBody("t1")))
		assert403("PAT DELETE", rawReq(srv, "DELETE", "/api/v1/me/push-devices/c1", pat.Token, "", ""))
		// A PAT with a valid session cookie alongside is still a PAT.
		assert403("PAT+cookie", rawReq(srv, "PUT", "/api/v1/me/push-devices/c1", pat.Token, sessTok, pushBody("t1")))
		// An OAuth bearer with a valid cookie alongside: requestSessionInfo would
		// fall through to the cookie, so only the explicit gate refuses it.
		oauth := callPush(srv.handlePutPushDevice, "PUT", "c1", pushBody("t1"), func(r *http.Request) *http.Request {
			ctx := WithCurrentUser(r.Context(), e.user)
			ctx = context.WithValue(ctx, ctxAuthKind, authKindOAuth)
			r.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: sessTok})
			return r.WithContext(ctx)
		})
		assert403("OAuth+cookie", oauth)
		// An app principal.
		app := callPush(srv.handlePutPushDevice, "PUT", "c1", pushBody("t1"), func(r *http.Request) *http.Request {
			ctx := context.WithValue(WithCurrentUser(r.Context(), e.user), appCtxKey{}, &appContext{})
			return r.WithContext(ctx)
		})
		assert403("app token", app)
		// A workspace-pinned PAT is refused earlier, by the BUG-3982 gate, and
		// gets that gate's bytes rather than session_required.
		ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Pinned", OwnerID: e.user.ID})
		if err != nil {
			t.Fatal(err)
		}
		pinned, err := srv.store.CreateAPIToken(e.user.ID, models.APITokenCreate{Name: "pinned", WorkspaceID: ws.ID}, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		want := httptest.NewRecorder()
		writeTokenWorkspaceDeniedFor(want, &tokenPin{ID: ws.ID, Slug: ws.Slug})
		for _, m := range []string{"PUT", "DELETE"} {
			if rr := rawReq(srv, m, "/api/v1/me/push-devices/c1", pinned.Token, "", pushBody("t1")); rr.Code != want.Code || rr.Body.String() != want.Body.String() {
				t.Errorf("a pinned PAT %s: %d %s, want the BUG-3982 refusal %d %s", m, rr.Code, rr.Body.String(), want.Code, want.Body.String())
			}
		}
		if got := e.devices(); len(got) != 0 {
			t.Fatalf("a refused request registered %v", got)
		}
		// No credential at all is a 401, not a 403.
		if rr := callPush(srv.handlePutPushDevice, "PUT", "c1", pushBody("t1"), func(r *http.Request) *http.Request { return r }); rr.Code != http.StatusUnauthorized {
			t.Errorf("no credential: %d, want 401", rr.Code)
		}
		if rr := rawReq(srv, "PUT", "/api/v1/me/push-devices/c1", "", "", pushBody("t1")); rr.Code < 400 {
			t.Errorf("no credential over HTTP: %d, want a refusal", rr.Code)
		}
		// And the real session credential is accepted, so the 403s above are
		// the gate and not a broken route.
		if rr := putDevice(srv, sessTok, "c1", pushBody("t1")); rr.Code != http.StatusOK {
			t.Fatalf("session bearer: %d %s", rr.Code, rr.Body.String())
		}
	})
}

// Kills: answering 404 for the wrong reason, or without the token the app
// keys on; and checks the gate order (a token is refused before the 404).
func TestPushDevices_NoGateway404(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		e := newPushEnv(t, srv, false)
		for _, m := range []string{"PUT", "DELETE"} {
			body := ""
			if m == "PUT" {
				body = pushBody("t1")
			}
			rr := rawReq(srv, m, "/api/v1/me/push-devices/c1", e.token, "", body)
			if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "push_unavailable") {
				t.Errorf("%s with no gateway: %d %s, want 404 push_unavailable", m, rr.Code, rr.Body.String())
			}
		}
		if len(e.devices()) != 0 {
			t.Fatal("a device was registered with no gateway")
		}
		pat, err := srv.store.CreateAPIToken(e.user.ID, models.APITokenCreate{Name: "p"}, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if rr := putDevice(srv, pat.Token, "c1", pushBody("t1")); rr.Code != http.StatusForbidden {
			t.Errorf("a PAT with no gateway: %d, want 403 (the kind gate runs first)", rr.Code)
		}
	})
}

func TestPushDevices_ConnectionIDValidation(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		e := newPushEnv(t, srv, true)
		for _, bad := range []string{strings.Repeat("a", 65), "a_b", "a.b", "a%20b", "%C3%A9", "a%00b"} {
			for _, m := range []string{"PUT", "DELETE"} {
				body := ""
				if m == "PUT" {
					body = pushBody("t1")
				}
				if rr := rawReq(srv, m, "/api/v1/me/push-devices/"+bad, e.token, "", body); rr.Code != http.StatusBadRequest {
					t.Errorf("%s connection_id %q: %d %s, want 400", m, bad, rr.Code, rr.Body.String())
				}
			}
		}
		if len(e.devices()) != 0 {
			t.Fatal("an invalid connection_id registered a device")
		}
		for i, good := range []string{"3f1c2a9e-4b7d-4e0a-9a53-0c1d2e3f4a5b", strings.Repeat("A", 64), "x"} {
			if rr := putDevice(srv, e.token, good, pushBody("t"+string(rune('a'+i)))); rr.Code != http.StatusOK {
				t.Errorf("connection_id %q: %d %s, want 200", good, rr.Code, rr.Body.String())
			}
		}
	})
}

func TestPushDevices_RegisterBodyValidation(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		e := newPushEnv(t, srv, true)
		bad := map[string]string{
			"no platform":      `{"token":"t"}`,
			"platform web":     `{"platform":"web","token":"t"}`,
			"platform case":    `{"platform":"Android","token":"t"}`,
			"no token":         `{"platform":"ios"}`,
			"empty token":      `{"platform":"ios","token":""}`,
			"NUL in token":     `{"platform":"ios","token":"a\u0000b"}`,
			"NUL in version":   `{"platform":"ios","token":"t","app_version":"1\u0000"}`,
			"token too long":   `{"platform":"ios","token":"` + strings.Repeat("x", 4097) + `"}`,
			"version too long": `{"platform":"ios","token":"t","app_version":"` + strings.Repeat("9", 65) + `"}`,
			"unknown member":   `{"platform":"ios","token":"t","extra":1}`,
			"token not string": `{"platform":"ios","token":5}`,
			"repeated member":  `{"platform":"ios","token":"a","token":"b"}`,
			"not json":         `nope`,
			"empty body":       ``,
			"array":            `[]`,
		}
		for name, body := range bad {
			if rr := putDevice(srv, e.token, "c1", body); rr.Code != http.StatusBadRequest {
				t.Errorf("%s: %d %s, want 400", name, rr.Code, rr.Body.String())
			}
		}
		if got := e.devices(); len(got) != 0 {
			t.Fatalf("a refused body registered %v", got)
		}
		// environment is accepted, whatever it holds, and ignored.
		for i, env := range []string{`"sandbox"`, `"production"`, `"anything"`, `null`, `5`} {
			body := `{"platform":"ios","token":"env` + string(rune('a'+i)) + `","environment":` + env + `}`
			if rr := putDevice(srv, e.token, "c1", body); rr.Code != http.StatusOK {
				t.Errorf("environment %s: %d %s, want 200", env, rr.Code, rr.Body.String())
			}
		}
		// The token is stored as sent, platform and version with it.
		ds, _ := srv.store.ListPushDevices(e.user.ID)
		if len(ds) != 1 || ds[0].Platform != "ios" || ds[0].Token != "enve" {
			t.Fatalf("devices = %+v", ds)
		}
	})
}

// Kills: a register against a session that ended before the write, and one
// that is simply gone, answering anything but 401.
func TestPushDevices_GoneSession401(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		e := newPushEnv(t, srv, true)
		tok, _ := e.extraSession()
		if err := srv.store.DeleteSession(tok); err != nil {
			t.Fatal(err)
		}
		if rr := putDevice(srv, tok, "c1", pushBody("t1")); rr.Code != http.StatusUnauthorized {
			t.Fatalf("a gone session: %d %s, want 401", rr.Code, rr.Body.String())
		}
		if len(e.devices()) != 0 {
			t.Fatal("a gone session registered a device")
		}
	})
}

// Logout deletes the session, which deletes its device row.
func TestPushDevices_LogoutCascades_HTTP(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		e := newPushEnv(t, srv, true)
		other, _ := e.extraSession()
		putDevice(srv, e.token, "c1", pushBody("t-out"))
		putDevice(srv, other, "c2", pushBody("t-keep"))
		if rr := rawReq(srv, "POST", "/api/v1/auth/logout", e.token, "", ""); rr.Code != http.StatusOK {
			t.Fatalf("logout: %d %s", rr.Code, rr.Body.String())
		}
		got := e.devices()
		if _, ok := got["t-out"]; ok || len(got) != 1 {
			t.Fatalf("devices after logout = %v, want only the other session's", got)
		}
	})
}

// --- rotation callers -------------------------------------------------------

const rotatedPassword = "another-strong-passphrase-42"

func changePassword(srv *Server, bearer string) *httptest.ResponseRecorder {
	return rawReq(srv, "PATCH", "/api/v1/auth/me", bearer, "",
		`{"current_password":"correct-horse-battery-staple","new_password":"`+rotatedPassword+`"}`)
}

// The password change: the caller's device survives on the replacement
// session, the other session's device is gone with its session. Kills:
// rebinding every session's devices, none, or deleting before the rebind.
func TestPushDevices_PasswordChange_RebindsCallerOnly(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		e := newPushEnv(t, srv, true)
		other, _ := e.extraSession()
		putDevice(srv, e.token, "c1", pushBody("t-caller"))
		putDevice(srv, other, "c2", pushBody("t-other"))
		rr := changePassword(srv, e.token)
		if rr.Code != http.StatusOK {
			t.Fatalf("password change: %d %s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Token string `json:"token"`
		}
		parseJSON(t, rr, &resp)
		if resp.Token == "" {
			t.Fatal("no replacement token returned")
		}
		got := e.devices()
		if len(got) != 1 || got["t-caller"] != e.sessionID(resp.Token) {
			t.Fatalf("devices = %v, want only t-caller, on the replacement session", got)
		}
		if info, _ := srv.store.ValidateSession(e.token); info != nil {
			t.Fatal("the replaced session is still valid")
		}
		if info, _ := srv.store.ValidateSession(other); info != nil {
			t.Fatal("the other session is still valid")
		}
	})
}

// Each credential-change door runs the same rotation: with the fault seam
// armed, every caller fails closed (500, every session revoked, the devices
// with them). Kills: dropping the fail-closed fallback; a caller that
// bypasses RotateUserSessions (the seam would never fire and the sessions
// would survive).
func TestPushDevices_Rotation_InjectedFailure_RevokesAll(t *testing.T) {
	type door struct {
		name string
		run  func(t *testing.T, srv *Server) (userID string, rr *httptest.ResponseRecorder)
	}
	doors := []door{
		{"password change", func(t *testing.T, srv *Server) (string, *httptest.ResponseRecorder) {
			e := newPushEnv(t, srv, true)
			other, _ := e.extraSession()
			putDevice(srv, e.token, "c1", pushBody("t1"))
			putDevice(srv, other, "c2", pushBody("t2"))
			return e.user.ID, changePassword(srv, e.token)
		}},
		{"2fa disable", func(t *testing.T, srv *Server) (string, *httptest.ResponseRecorder) {
			srv.SetPushGateway(pushTestGateway)
			userID, secret, tok := passwordlessTOTPUser(t, srv, "nopw-3938@example.com")
			other, err := srv.store.CreateSession(userID, "test", "192.0.2.1", testSessionUA, webSessionTTL)
			if err != nil {
				t.Fatal(err)
			}
			for i, tk := range []string{tok, other} {
				info, _ := srv.store.ValidateSession(tk)
				if _, err := srv.store.RegisterPushDevice(userID, info.ID, "c"+string(rune('1'+i)), "ios", "t"+string(rune('1'+i)), ""); err != nil {
					t.Fatal(err)
				}
			}
			return userID, doRequestWithCookie(srv, "POST", "/api/v1/auth/2fa/disable", map[string]any{"code": validTOTPCode(t, secret)}, tok)
		}},
		{"cloud oauth unlink", func(t *testing.T, srv *Server) (string, *httptest.ResponseRecorder) {
			srv.cloudMode = true
			srv.SetPushGateway(pushTestGateway)
			u, err := srv.store.CreateUser(models.UserCreate{Email: "unlink-3938@example.com", Name: "Unlink", Password: "correct-horse-battery-staple"})
			if err != nil {
				t.Fatal(err)
			}
			if err := srv.store.LinkOAuthProvider(u.ID, "github", "gh-3938"); err != nil {
				t.Fatal(err)
			}
			tok, err := srv.store.CreateSession(u.ID, "test", "192.0.2.1", testSessionUA, webSessionTTL)
			if err != nil {
				t.Fatal(err)
			}
			other, _ := srv.store.CreateSession(u.ID, "test", "192.0.2.1", testSessionUA, webSessionTTL)
			for i, tk := range []string{tok, other} {
				info, _ := srv.store.ValidateSession(tk)
				if _, err := srv.store.RegisterPushDevice(u.ID, info.ID, "c"+string(rune('1'+i)), "ios", "t"+string(rune('1'+i)), ""); err != nil {
					t.Fatal(err)
				}
			}
			return u.ID, doRequestWithCookie(srv, "POST", "/api/v1/auth/oauth-unlink", map[string]any{"provider": "github"}, tok)
		}},
	}
	for _, d := range doors {
		t.Run(d.name, func(t *testing.T) {
			needsBackends(t, func(t *testing.T, srv *Server) {
				fired := 0
				restore := srv.store.SetRotateFaultHookForTesting(func() error {
					fired++
					return errors.New("injected rotation failure")
				})
				defer restore()
				userID, rr := d.run(t, srv)
				if fired == 0 {
					t.Fatalf("the rotation fault seam never fired: this door does not go through RotateUserSessions (%d %s)", rr.Code, rr.Body.String())
				}
				if rr.Code != http.StatusInternalServerError {
					t.Fatalf("%d %s, want 500", rr.Code, rr.Body.String())
				}
				ph := "?"
				if srv.store.D().Driver() == store.DriverPostgres {
					ph = "$1"
				}
				var n int
				if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = `+ph, userID).Scan(&n); err != nil || n != 0 {
					t.Fatalf("%d sessions survived a failed rotation (%v), want 0", n, err)
				}
				ds, err := srv.store.ListPushDevices(userID)
				if err != nil || len(ds) != 0 {
					t.Fatalf("devices survived a failed rotation: %v %v", ds, err)
				}
			})
		})
	}
}

// --- settings ----------------------------------------------------------------

func settingsJSON(t *testing.T, rr *httptest.ResponseRecorder) models.NotificationSettings {
	t.Helper()
	var s models.NotificationSettings
	parseJSON(t, rr, &s)
	return s
}

const allSettings = `{"push_enabled":true,"notify_assigned":true,"notify_comments":false,"notify_replies":true,"notify_reminders":false,"notify_mentions":true,"include_agent_activity":true}`

func TestNotificationSettings_DefaultsAndUnknownKey400(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		e := newPushEnv(t, srv, false) // settings do not need a gateway
		path := "/api/v1/me/notification-settings"

		rr := rawReq(srv, "GET", path, e.token, "", "")
		if rr.Code != http.StatusOK || settingsJSON(t, rr) != models.DefaultNotificationSettings() {
			t.Fatalf("defaults: %d %s", rr.Code, rr.Body.String())
		}

		// Unknown key, missing key, wrong type, partial: all 400, none saved.
		for name, body := range map[string]string{
			"unknown key":   strings.Replace(allSettings, `"include_agent_activity":true`, `"include_agent_activity":true,"notify_bogus":true`, 1),
			"missing key":   strings.Replace(allSettings, `,"include_agent_activity":true`, ``, 1),
			"missing first": strings.Replace(allSettings, `"push_enabled":true,`, ``, 1),
			"empty object":  `{}`,
			"wrong type":    strings.Replace(allSettings, `"push_enabled":true`, `"push_enabled":"yes"`, 1),
			"null value":    strings.Replace(allSettings, `"push_enabled":true`, `"push_enabled":null`, 1),
			"not json":      `x`,
		} {
			if rr := rawReq(srv, "PUT", path, e.token, "", body); rr.Code != http.StatusBadRequest {
				t.Errorf("%s: %d %s, want 400", name, rr.Code, rr.Body.String())
			}
		}
		if got := settingsJSON(t, rawReq(srv, "GET", path, e.token, "", "")); got != models.DefaultNotificationSettings() {
			t.Fatalf("a refused PUT changed the settings: %+v", got)
		}

		// Round trip: a full replace, a false stays false.
		rr = rawReq(srv, "PUT", path, e.token, "", allSettings)
		want := models.NotificationSettings{PushEnabled: true, NotifyAssigned: true, NotifyReplies: true, NotifyMentions: true, IncludeAgentActivity: true}
		if rr.Code != http.StatusOK || settingsJSON(t, rr) != want {
			t.Fatalf("put: %d %s", rr.Code, rr.Body.String())
		}
		if got := settingsJSON(t, rawReq(srv, "GET", path, e.token, "", "")); got != want {
			t.Fatalf("round trip: %+v, want %+v", got, want)
		}
		// The cookie door and an unauthenticated caller.
		if rr := rawReq(srv, "GET", path, "", "", ""); rr.Code != http.StatusUnauthorized {
			t.Errorf("no credential: %d, want 401", rr.Code)
		}
	})
}

// Any authenticated user may use the settings, a PAT included; an app
// principal and a workspace-pinned PAT may not, the latter refused by the
// BUG-3982 gate. Kills: admitting these routes to a pinned token, or gating
// the settings on the session kind.
func TestNotificationSettings_Auth(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		e := newPushEnv(t, srv, false)
		path := "/api/v1/me/notification-settings"
		ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Pinned", OwnerID: e.user.ID})
		if err != nil {
			t.Fatal(err)
		}
		free, err := srv.store.CreateAPIToken(e.user.ID, models.APITokenCreate{Name: "free"}, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		pinned, err := srv.store.CreateAPIToken(e.user.ID, models.APITokenCreate{Name: "pinned", WorkspaceID: ws.ID}, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if rr := rawReq(srv, "GET", path, free.Token, "", ""); rr.Code != http.StatusOK {
			t.Errorf("an unpinned PAT GET: %d %s, want 200", rr.Code, rr.Body.String())
		}
		if rr := rawReq(srv, "PUT", path, free.Token, "", allSettings); rr.Code != http.StatusOK {
			t.Errorf("an unpinned PAT PUT: %d %s, want 200", rr.Code, rr.Body.String())
		}
		// The refusal is BUG-3982's gate's, byte for byte, so a consumer keyed
		// on it sees one answer at every pinned-token door. Kills: listing these
		// routes in pinnedTokenExtraRoute.
		want := httptest.NewRecorder()
		writeTokenWorkspaceDeniedFor(want, &tokenPin{ID: ws.ID, Slug: ws.Slug})
		wantPinned := func(what string, rr *httptest.ResponseRecorder) {
			t.Helper()
			if rr.Code != want.Code || rr.Body.String() != want.Body.String() {
				t.Errorf("a pinned PAT %s: %d %s, want the BUG-3982 refusal %d %s", what, rr.Code, rr.Body.String(), want.Code, want.Body.String())
			}
		}
		wantPinned("GET", rawReq(srv, "GET", path, pinned.Token, "", ""))
		wantPinned("PUT", rawReq(srv, "PUT", path, pinned.Token, "", strings.Replace(allSettings, `"push_enabled":true`, `"push_enabled":false`, 1)))
		if got := settingsJSON(t, rawReq(srv, "GET", path, e.token, "", "")); !got.PushEnabled {
			t.Error("a refused pinned PUT changed the settings")
		}
		for _, h := range []http.HandlerFunc{srv.handleGetNotificationSettings, srv.handlePutNotificationSettings} {
			r := httptest.NewRequest("PUT", path, strings.NewReader(allSettings))
			ctx := context.WithValue(WithCurrentUser(r.Context(), e.user), appCtxKey{}, &appContext{})
			rr := httptest.NewRecorder()
			h(rr, r.WithContext(ctx))
			if rr.Code != http.StatusForbidden {
				t.Errorf("an app principal: %d, want 403", rr.Code)
			}
		}
	})
}

// push_available is a JSON bool on /auth/session for a SIGNED-IN caller, true
// only with a gateway, and is absent for an anonymous caller and on the
// pre-auth setup state. Kills: a string or number, an omitted false, emitting
// it to an anonymous caller.
func TestPushDevices_Session_PushAvailable_IsJSONBool(t *testing.T) {
	for _, gateway := range []bool{false, true} {
		srv := testServer(t)
		if gateway {
			srv.SetPushGateway(pushTestGateway)
		}
		// Pre-auth setup state first (no users yet): no push_available.
		rr := doRequest(srv, "GET", "/api/v1/auth/session", nil)
		var pre map[string]json.RawMessage
		parseJSON(t, rr, &pre)
		if _, ok := pre["push_available"]; ok {
			t.Errorf("gateway=%v: setup state carries push_available", gateway)
		}
		tok := bootstrapFirstUser(t, srv, "pa@example.com", "PA")
		var raw map[string]json.RawMessage
		parseJSON(t, rawReq(srv, "GET", "/api/v1/auth/session", tok, "", ""), &raw)
		got, ok := raw["push_available"]
		want := "false"
		if gateway {
			want = "true"
		}
		if !ok || !bytes.Equal(bytes.TrimSpace(got), []byte(want)) {
			t.Errorf("gateway=%v signed in: push_available = %s (present %v), want JSON %s", gateway, got, ok, want)
		}
		// After setup, an anonymous caller still gets the session payload, but
		// not the gateway configuration (ruling b878d7f8).
		var anon map[string]json.RawMessage
		parseJSON(t, doRequest(srv, "GET", "/api/v1/auth/session", nil), &anon)
		if v, ok := anon["push_available"]; ok {
			t.Errorf("gateway=%v anonymous: carries push_available = %s, want it absent", gateway, v)
		}
	}
}
