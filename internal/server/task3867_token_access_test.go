package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/pquerna/otp/totp"

	"github.com/PerpetualSoftware/pad/internal/email"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

func task3867Want(t *testing.T, rr *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("status %d, want %d: %s", rr.Code, status, rr.Body.String())
	}
	if code != "" && errorCode(t, rr) != code {
		t.Fatalf("want %s: %s", code, rr.Body.String())
	}
}
func task3867Patch(e *tokenAuthEnv, body any) *httptest.ResponseRecorder {
	return doRequestWithBearer(e.srv, "PATCH", "/api/v1/auth/tokens/"+e.patID, e.sessTok, body)
}
func task3867Scopes(t *testing.T, e *tokenAuthEnv, scopes string) {
	t.Helper()
	if _, err := e.srv.store.DB().Exec(`UPDATE api_tokens SET scopes=? WHERE id=?`, scopes, e.patID); err != nil {
		t.Fatal(err)
	}
}
func task3867Verify(e *tokenAuthEnv, body any) *httptest.ResponseRecorder {
	return doRequestWithBearer(e.srv, "POST", "/api/v1/auth/step-up/verify", e.sessTok, body)
}
func task3867Factor(t *testing.T, e *tokenAuthEnv, want string) {
	t.Helper()
	rr := doRequestWithBearer(e.srv, "GET", "/api/v1/auth/step-up", e.sessTok, nil)
	task3867Want(t, rr, 200, "")
	var got struct {
		Factor      string     `json:"factor"`
		ActiveUntil *time.Time `json:"active_until"`
	}
	parseJSON(t, rr, &got)
	if got.Factor != want {
		t.Fatalf("factor %s, want %s", got.Factor, want)
	}
}
func task3867EnableTOTP(t *testing.T, e *tokenAuthEnv) string {
	t.Helper()
	const secret = "JBSWY3DPEHPK3PXP"
	if err := e.srv.store.SetTOTPSecret(e.user.ID, secret); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.store.EnableTOTP(e.user.ID, secret, strings.Join(hashRecoveryCodes([]string{"ABCDEFGHIJKLMNOP"}), "\n")); err != nil {
		t.Fatal(err)
	}

	return secret
}

func TestTask3867WideningWithoutStepUpRefused(t *testing.T) {
	e := setupTokenAuthEnv(t)
	task3867Scopes(t, e, `["read"]`)
	task3867Want(t, task3867Patch(e, map[string]any{"scopes": `["*"]`}), 403, "step_up_required")
	var scopes string
	if err := e.srv.store.DB().QueryRow(`SELECT scopes FROM api_tokens WHERE id=?`, e.patID).Scan(&scopes); err != nil || scopes != `["read"]` {
		t.Fatalf("refusal changed scopes: %s %v", scopes, err)
	}
}
func TestTask3867NarrowingNoStepUp(t *testing.T) {
	e := setupTokenAuthEnv(t)
	rr := task3867Patch(e, map[string]any{"scopes": `[ "read" ]`})
	task3867Want(t, rr, 200, "")
	var tok models.APIToken
	parseJSON(t, rr, &tok)
	if tok.Scopes != `["read"]` {
		t.Fatalf("scopes %s", tok.Scopes)
	}
	task3867Want(t, task3867Patch(e, map[string]any{"scopes": `["pad:read"]`}), 200, "")
}
func TestTask3867NameOnlyNoStepUp(t *testing.T) {
	e := setupTokenAuthEnv(t)
	task3867Scopes(t, e, `["read"]`)
	rr := task3867Patch(e, map[string]any{"name": "  Renamed  "})
	task3867Want(t, rr, 200, "")
	var tok models.APIToken
	parseJSON(t, rr, &tok)
	if tok.Name != "Renamed" || tok.Scopes != `["read"]` {
		t.Fatalf("metadata: %+v", tok)
	}
}
func TestTask3867TOTPStepUp(t *testing.T) {
	e := setupTokenAuthEnv(t)
	secret := task3867EnableTOTP(t, e)
	task3867Factor(t, e, "totp")
	task3867Scopes(t, e, `["read"]`)
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	task3867Want(t, task3867Verify(e, map[string]string{"code": "wrong"}), 403, "invalid_code")
	task3867Want(t, task3867Verify(e, map[string]string{"code": code}), 200, "")
	task3867Want(t, task3867Patch(e, map[string]any{"scopes": `["*"]`}), 200, "")
	task3867Want(t, task3867Verify(e, map[string]string{"code": code}), 403, "invalid_code")
}
func TestTask3867RecoveryCodeStepUp(t *testing.T) {
	e := setupTokenAuthEnv(t)
	task3867EnableTOTP(t, e)
	task3867Want(t, task3867Verify(e, map[string]string{"recovery_code": "abcd efgh ijkl mnop"}), 200, "")
	task3867Scopes(t, e, `["read"]`)
	task3867Want(t, task3867Patch(e, map[string]any{"scopes": `["write"]`}), 200, "")
	task3867Want(t, task3867Verify(e, map[string]string{"recovery_code": "ABCDEFGHIJKLMNOP"}), 403, "invalid_code")
}

type task3867Mailbox struct {
	codes chan string
	fail  atomic.Bool
}

func task3867Mail(t *testing.T, e *tokenAuthEnv) *task3867Mailbox {
	t.Helper()
	m := &task3867Mailbox{codes: make(chan string, 10)}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.fail.Load() {
			w.WriteHeader(503)
			return
		}
		var payload struct {
			Plain string `json:"plain"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		code := regexp.MustCompile(`\b[0-9]{6}\b`).FindString(payload.Plain)
		if code == "" {
			t.Error("email has no six digit code")
		}
		m.codes <- code
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	t.Cleanup(stub.Close)
	sender := email.NewSender("test-key", "pad@example.com", "Pad", "")
	sender.SetEndpoint(stub.URL)
	e.srv.SetEmailSender(sender)
	return m
}
func task3867Send(e *tokenAuthEnv) *httptest.ResponseRecorder {
	return doRequestWithBearer(e.srv, "POST", "/api/v1/auth/step-up/email", e.sessTok, nil)
}
func TestTask3867EmailStepUp(t *testing.T) {
	e := setupTokenAuthEnv(t)
	m := task3867Mail(t, e)
	task3867Factor(t, e, "email")
	task3867Want(t, task3867Send(e), 200, "")
	code := <-m.codes
	wrong := "000000"
	if code == wrong {
		wrong = "000001"
	}
	for i := 0; i < 5; i++ {
		task3867Want(t, task3867Verify(e, map[string]string{"code": wrong}), 403, "invalid_code")
	}
	task3867Want(t, task3867Verify(e, map[string]string{"code": code}), 403, "invalid_code")
	task3867Want(t, task3867Send(e), 200, "")
	fresh := <-m.codes
	task3867Want(t, task3867Verify(e, map[string]string{"code": fresh}), 200, "")
	task3867Want(t, task3867Verify(e, map[string]string{"code": fresh}), 403, "invalid_code")
	task3867Scopes(t, e, `["read"]`)
	task3867Want(t, task3867Patch(e, map[string]any{"scopes": `["*"]`}), 200, "")
	task3867Want(t, task3867Send(e), 200, "")
	expired := <-m.codes
	if _, err := e.srv.store.DB().Exec(`UPDATE step_up_codes SET expires_at=? WHERE user_id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), e.user.ID); err != nil {
		t.Fatal(err)
	}
	task3867Want(t, task3867Verify(e, map[string]string{"code": expired}), 403, "invalid_code")
	rr := task3867Send(e)
	task3867Want(t, rr, 429, "rate_limited")
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
}
func TestTask3867EmailFailureAndFactorMismatch(t *testing.T) {
	e := setupTokenAuthEnv(t)
	task3867Want(t, task3867Send(e), 409, "step_up_factor_mismatch")
	m := task3867Mail(t, e)
	m.fail.Store(true)
	for i := 0; i < 4; i++ {
		task3867Want(t, task3867Send(e), 503, "email_send_failed")
	}
	var n int
	if err := e.srv.store.DB().QueryRow(`SELECT COUNT(*) FROM step_up_codes`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("failed sends kept %d rows: %v", n, err)
	}
	m.fail.Store(false)
	task3867Want(t, task3867Send(e), 200, "")
	<-m.codes
	task3867EnableTOTP(t, e)
	task3867Factor(t, e, "totp")
	task3867Want(t, task3867Send(e), 409, "step_up_factor_mismatch")
}
func TestTask3867PasswordStepUpWithoutEmail(t *testing.T) {
	e := setupTokenAuthEnv(t)
	task3867Factor(t, e, "password")
	task3867Want(t, task3867Verify(e, map[string]string{"password": "wrong"}), 403, "invalid_password")
	task3867Want(t, task3867Verify(e, map[string]string{"password": "correct-horse-battery-staple"}), 200, "")
	task3867Scopes(t, e, `["read"]`)
	task3867Want(t, task3867Patch(e, map[string]any{"scopes": `["*"]`}), 200, "")
}
func TestTask3867Unavailable409(t *testing.T) {
	e := setupTokenAuthEnv(t)
	if _, err := e.srv.store.DB().Exec(`UPDATE users SET password_set=0 WHERE id=?`, e.user.ID); err != nil {
		t.Fatal(err)
	}
	task3867Factor(t, e, "unavailable")
	rr := task3867Verify(e, map[string]string{"password": "correct-horse-battery-staple"})
	task3867Want(t, rr, 409, "step_up_unavailable")
	if !strings.Contains(rr.Body.String(), "create a new key and delete this one") {
		t.Fatal("missing recovery guidance")
	}
}
func TestTask3867GrantPerSession(t *testing.T) {
	e := setupTokenAuthEnv(t)
	other, err := e.srv.store.CreateSession(e.user.ID, "web", "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	task3867Want(t, task3867Verify(e, map[string]string{"password": "correct-horse-battery-staple"}), 200, "")
	task3867Scopes(t, e, `["read"]`)
	rr := doRequestWithBearer(e.srv, "PATCH", "/api/v1/auth/tokens/"+e.patID, other, map[string]any{"scopes": `["*"]`})
	task3867Want(t, rr, 403, "step_up_required")
	task3867Want(t, task3867Patch(e, map[string]any{"scopes": `["*"]`}), 200, "")
	task3867Scopes(t, e, `["read"]`)
	task3867Want(t, task3867Patch(e, map[string]any{"scopes": `["write"]`}), 200, "")
}
func TestTask3867GrantExpires(t *testing.T) {
	e := setupTokenAuthEnv(t)
	task3867Want(t, task3867Verify(e, map[string]string{"password": "correct-horse-battery-staple"}), 200, "")
	info, err := e.srv.store.ValidateSession(e.sessTok)
	if err != nil || info == nil {
		t.Fatal(err)
	}
	var original string
	if err := e.srv.store.DB().QueryRow(`SELECT step_up_until FROM sessions WHERE id=?`, info.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	task3867Want(t, task3867Verify(e, map[string]string{"password": "correct-horse-battery-staple"}), 200, "")
	var after string
	if err := e.srv.store.DB().QueryRow(`SELECT step_up_until FROM sessions WHERE id=?`, info.ID).Scan(&after); err != nil || after != original {
		t.Fatalf("grant extended: %v", err)
	}
	if _, err := e.srv.store.DB().Exec(`UPDATE sessions SET step_up_until=? WHERE id=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339), info.ID); err != nil {
		t.Fatal(err)
	}
	task3867Scopes(t, e, `["read"]`)
	task3867Want(t, task3867Patch(e, map[string]any{"scopes": `["*"]`}), 403, "step_up_required")
}
func TestTask3867PATAuthRefused(t *testing.T) {
	e := setupTokenAuthEnv(t)
	for _, route := range []struct{ method, path string }{{"PATCH", "/api/v1/auth/tokens/" + e.patID}, {"GET", "/api/v1/auth/step-up"}, {"POST", "/api/v1/auth/step-up/email"}, {"POST", "/api/v1/auth/step-up/verify"}} {
		rr := doRequestWithBearer(e.srv, route.method, route.path, e.patTok, map[string]any{"name": "Name"})
		task3867Want(t, rr, 403, "session_required")
	}
}
func TestTask3867ForeignAndUnknownID404Identical(t *testing.T) {
	e := setupTokenAuthEnv(t)
	foreign, err := e.srv.store.CreateUser(models.UserCreate{Email: "foreign@example.com", Name: "Foreign", Password: "correct-horse-battery-staple", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := e.srv.store.CreateAPIToken(foreign.ID, models.APITokenCreate{Name: "Foreign", Scopes: `["read"]`}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"scopes": `["*"]`}
	a := doRequestWithBearer(e.srv, "PATCH", "/api/v1/auth/tokens/"+tok.ID, e.sessTok, body)
	b := doRequestWithBearer(e.srv, "PATCH", "/api/v1/auth/tokens/unknown", e.sessTok, body)
	task3867Want(t, a, 404, "not_found")
	task3867Want(t, b, 404, "not_found")
	if a.Body.String() != b.Body.String() {
		t.Fatal("foreign and unknown differ")
	}
}
func TestTask3867ValidationErrors(t *testing.T) {
	e := setupTokenAuthEnv(t)
	for _, body := range []any{map[string]any{}, map[string]any{"name": " "}, map[string]any{"name": nil}, map[string]any{"scopes": nil}, map[string]any{"scopes": []string{"read"}}, map[string]any{"scopes": ""}, map[string]any{"scopes": "[]"}, map[string]any{"scopes": "null"}, map[string]any{"scopes": `["unknown"]`}} {
		task3867Want(t, task3867Patch(e, body), 400, "validation_error")
	}
	task3867Want(t, task3867Verify(e, map[string]string{"code": "123456"}), 400, "validation_error")
	task3867Mail(t, e)
	task3867Want(t, task3867Verify(e, map[string]string{"password": "x"}), 400, "validation_error")
	task3867EnableTOTP(t, e)
	task3867Want(t, task3867Verify(e, map[string]string{"password": "x"}), 400, "validation_error")
}
func TestTask3867AuditRecordsChange(t *testing.T) {
	e := setupTokenAuthEnv(t)
	task3867Want(t, task3867Patch(e, map[string]any{"scopes": `["read"]`, "name": "Changed"}), 200, "")
	task3867Want(t, task3867Patch(e, map[string]any{"scopes": `["read"]`, "name": "Changed"}), 200, "")
	task3867Want(t, task3867Verify(e, map[string]string{"password": "wrong"}), 403, "invalid_password")
	task3867Want(t, task3867Verify(e, map[string]string{"password": "correct-horse-battery-staple"}), 200, "")
	task3867Want(t, task3867Patch(e, map[string]any{"scopes": `["*"]`}), 200, "")
	rows, err := e.srv.store.ListUserActivity(e.user.ID, models.ActivityListParams{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, a := range rows {
		counts[a.Action]++
		if a.Action == "token_updated" {
			var meta map[string]string
			if err := json.Unmarshal([]byte(a.Metadata), &meta); err != nil {
				t.Fatal(err)
			}
			if meta["token_id"] != e.patID || meta["old_scopes"] == "" || meta["new_scopes"] == "" {
				t.Fatalf("metadata: %v", meta)
			}
			if meta["new_scopes"] == `["*"]` && meta["step_up"] != "used" {
				t.Fatal("widen audit missing grant use")
			}
			if meta["new_scopes"] == `["read"]` && (meta["step_up"] != "not_needed" || meta["old_name"] != "the-pat" || meta["name"] != "Changed") {
				t.Fatal("narrow audit missing details")
			}
		}
		if strings.Contains(a.Metadata, "correct-horse") || strings.Contains(a.Metadata, "wrong") {
			t.Fatal("credential in audit")
		}
	}
	if counts["token_updated"] != 2 || counts["step_up_failed"] != 1 || counts["step_up_verified"] != 1 {
		t.Fatalf("audit counts: %v", counts)
	}
}
func TestTask3867TokenAccessWidensMatchesLevels(t *testing.T) {
	raw, err := os.ReadFile("testdata/token_scope_levels.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Rows []struct{ Scopes, Level string }
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Rows) == 0 {
		t.Fatal("empty scope fixture")
	}
	rank := map[string]int{"none": 0, "read": 1, "write": 2}
	for _, old := range fixture.Rows {
		for _, next := range fixture.Rows {
			got := tokenAccessWidens(tokenAccess{Scopes: old.Scopes}, tokenAccess{Scopes: next.Scopes})
			want := rank[next.Level] > rank[old.Level]
			if got != want {
				t.Fatalf("old %q (%s), new %q (%s): widening %v, want %v", old.Scopes, old.Level, next.Scopes, next.Level, got, want)
			}
		}
	}
}

func TestTask3867LivenessClosesOnScopeChange(t *testing.T) {
	for _, kind := range []string{"collab", "sse", "watch"} {
		t.Run(kind, func(t *testing.T) {
			var srv *Server
			if kind == "collab" {
				prev := collabMembershipRevalInterval
				collabMembershipRevalInterval = time.Hour
				t.Cleanup(func() { collabMembershipRevalInterval = prev })
				srv = testServerWithCollab(t)
			} else {
				prev := sseMembershipRevalInterval
				sseMembershipRevalInterval = time.Hour
				t.Cleanup(func() { sseMembershipRevalInterval = prev })
				prevWatch := watchListRevalInterval
				watchListRevalInterval = time.Hour
				t.Cleanup(func() { watchListRevalInterval = prevWatch })
				srv = testServerWithEvents(t)
				srv.SetWatchEventsBus(watchevents.New())
			}
			deterministicFirstTick(t)
			u, err := srv.store.CreateUser(models.UserCreate{Email: "stream@example.com", Name: "Stream", Password: "correct-horse-battery-staple", Role: "admin"})
			if err != nil {
				t.Fatal(err)
			}
			ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Stream", OwnerID: u.ID})
			if err != nil {
				t.Fatal(err)
			}
			if err := srv.store.AddWorkspaceMember(ws.ID, u.ID, "owner"); err != nil {
				t.Fatal(err)
			}
			secret, id := mintPAT(t, srv, u.ID, "Streaming key")
			session, err := srv.store.CreateSession(u.ID, "web", "", "", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(srv)
			defer ts.Close()
			ended := make(chan struct{})
			if kind == "collab" {
				col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: `{"fields":[]}`})
				if err != nil {
					t.Fatal(err)
				}
				item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Stream item"})
				if err != nil {
					t.Fatal(err)
				}
				parsed, _ := url.Parse(ts.URL)
				hdr := http.Header{"Authorization": []string{"Bearer " + secret}}
				dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
				conn, resp, err := dialer.Dial("ws://"+parsed.Host+"/api/v1/collab/"+item.ID+"?schema_version=1", hdr)
				if err != nil {
					if resp != nil {
						t.Log(resp.StatusCode)
					}
					t.Fatal(err)
				}
				defer conn.Close()
				go func() {
					defer close(ended)
					for {
						if _, _, err := conn.ReadMessage(); err != nil {
							return
						}
					}
				}()
			} else {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				path := "/api/v1/events?workspace=" + ws.Slug
				if kind == "watch" {
					path = "/api/v1/events/stream"
				}
				req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+secret)
				resp, err := ts.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("stream did not open: %d", resp.StatusCode)
				}
				go func() {
					defer close(ended)
					sc := bufio.NewScanner(resp.Body)
					for sc.Scan() {
					}
				}()
			}
			e := &tokenAuthEnv{srv: srv, user: u, patID: id, sessTok: session}
			task3867Want(t, task3867Patch(e, map[string]any{"name": "Still streaming"}), 200, "")
			select {
			case <-ended:
				t.Fatal("name change closed stream")
			case <-time.After(150 * time.Millisecond):
			}
			task3867Want(t, task3867Patch(e, map[string]any{"scopes": `["read"]`}), 200, "")
			select {
			case <-ended:
			case <-time.After(3 * time.Second):
				t.Fatal("stream stayed open after scope change")
			}
		})
	}
}

func TestTask3867VerifyRateLimits(t *testing.T) {
	for _, factor := range []string{"password", "totp", "recovery"} {
		t.Run(factor, func(t *testing.T) {
			e := setupTokenAuthEnv(t)
			body := map[string]string{"password": "wrong"}
			code := "invalid_password"
			if factor != "password" {
				task3867EnableTOTP(t, e)
				code = "invalid_code"
				if factor == "totp" {
					body = map[string]string{"code": "wrong"}
				} else {
					body = map[string]string{"recovery_code": "wrong"}
				}
			}
			for i := 0; i < 6; i++ {
				task3867Want(t, task3867Verify(e, body), 403, code)
			}
			task3867Want(t, task3867Verify(e, body), 429, "rate_limited")
		})
	}
}

func TestTask3867AnonymousRefused(t *testing.T) {
	e := setupTokenAuthEnv(t)
	for _, route := range []struct{ method, path string }{
		{"PATCH", "/api/v1/auth/tokens/" + e.patID},
		{"GET", "/api/v1/auth/step-up"},
		{"POST", "/api/v1/auth/step-up/email"},
		{"POST", "/api/v1/auth/step-up/verify"},
	} {
		task3867Want(t, doRequest(e.srv, route.method, route.path, map[string]string{"name": "Name"}), 401, "unauthorized")
	}
}
