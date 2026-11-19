package main

// TASK-4507: `pad token edit`, driven against a real server (the store, the
// PATCH handler and the step-up handlers are the production ones) and, for
// the refusals a real server cannot be made to give on demand, a stub.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// tokenEditFixture is a real server that also counts the requests it sees,
// keyed "METHOD path" with the /api/v1/auth/ prefix trimmed.
type tokenEditFixture struct {
	*twoFAFixture
	mu    sync.Mutex
	calls map[string]int
}

func newTokenEditFixture(t *testing.T) *tokenEditFixture {
	t.Helper()
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)
	f := &tokenEditFixture{calls: map[string]int{}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls[r.Method+" "+strings.TrimPrefix(r.URL.Path, "/api/v1/auth/")]++
		f.mu.Unlock()
		srv.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	f.twoFAFixture = &twoFAFixture{store: s, url: ts.URL, httpClient: ts.Client()}
	return f
}

func (f *tokenEditFixture) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func (f *tokenEditFixture) mint(t *testing.T, u *models.User, scopes string) *models.APITokenWithSecret {
	t.Helper()
	tok, err := f.store.CreateAPIToken(u.ID, models.APITokenCreate{Name: "ci", Scopes: scopes}, 30, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	return tok
}

func (f *tokenEditFixture) stored(t *testing.T, u *models.User, id string) *models.APIToken {
	t.Helper()
	tok, err := f.store.GetUserAPIToken(id, u.ID)
	if err != nil || tok == nil {
		t.Fatalf("GetUserAPIToken: %v %v", tok, err)
	}
	return tok
}

func strPtr(s string) *string { return &s }

type tokenEditRun struct {
	out, errOut bytes.Buffer
	err         error
}

func runTokenEditT(client *cli.Client, id string, name, scopes *string, stdin string, interactive, asJSON bool) *tokenEditRun {
	r := &tokenEditRun{}
	r.err = runTokenEdit(client, id, name, scopes, bufio.NewReader(strings.NewReader(stdin)), &r.out, &r.errOut, interactive, asJSON)
	return r
}

func TestTask4507NameOnlySendsNoScopes(t *testing.T) {
	f := newTokenEditFixture(t)
	u := f.passwordUser(t, "name@example.com")
	tok := f.mint(t, u, `["read"]`)
	client, _ := f.session(t, u)

	r := runTokenEditT(client, tok.ID, strPtr("  renamed  "), nil, "", false, false)
	if r.err != nil {
		t.Fatalf("edit: %v", r.err)
	}
	got := f.stored(t, u, tok.ID)
	if got.Name != "renamed" || got.Scopes != `["read"]` {
		t.Fatalf("stored name %q scopes %s, want renamed and [\"read\"]", got.Name, got.Scopes)
	}
	if r.errOut.Len() != 0 {
		t.Fatalf("a rename prompted: %q", r.errOut.String())
	}
	if !strings.Contains(r.out.String(), "Token updated: renamed") || !strings.Contains(r.out.String(), "read only") {
		t.Fatalf("stdout %q", r.out.String())
	}
	if f.count("GET step-up") != 0 {
		t.Fatal("a rename fetched step-up status")
	}
}

func TestTask4507NarrowingNeedsNoStepUp(t *testing.T) {
	f := newTokenEditFixture(t)
	u := f.passwordUser(t, "narrow@example.com")
	tok := f.mint(t, u, `["*"]`)
	client, _ := f.session(t, u)

	r := runTokenEditT(client, tok.ID, nil, strPtr("read"), "", false, false)
	if r.err != nil {
		t.Fatalf("edit: %v", r.err)
	}
	if got := f.stored(t, u, tok.ID).Scopes; got != `["read"]` {
		t.Fatalf("scopes %s, want [\"read\"]", got)
	}
	if r.errOut.Len() != 0 {
		t.Fatalf("narrowing prompted: %q", r.errOut.String())
	}
	if n := f.count("PATCH tokens/" + tok.ID); n != 1 {
		t.Fatalf("%d PATCH calls, want 1", n)
	}
}

func TestTask4507WideningWithPassword(t *testing.T) {
	f := newTokenEditFixture(t)
	u := f.passwordUser(t, "widen@example.com")
	tok := f.mint(t, u, `["read"]`)
	client, _ := f.session(t, u)

	r := runTokenEditT(client, tok.ID, nil, strPtr("*"), twoFAPassword+"\n", false, false)
	if r.err != nil {
		t.Fatalf("edit: %v\n%s", r.err, r.errOut.String())
	}
	if got := f.stored(t, u, tok.ID).Scopes; got != `["*"]` {
		t.Fatalf("scopes %s, want [\"*\"]", got)
	}
	if !strings.Contains(r.errOut.String(), "Password: ") {
		t.Fatalf("no password prompt on stderr: %q", r.errOut.String())
	}
	if strings.Contains(r.out.String(), "Password") {
		t.Fatalf("prompt on stdout: %q", r.out.String())
	}
	if !strings.Contains(r.out.String(), "full access") {
		t.Fatalf("stdout %q", r.out.String())
	}
	if n := f.count("PATCH tokens/" + tok.ID); n != 2 {
		t.Fatalf("%d PATCH calls, want 2 (refused, then retried)", n)
	}
}

// totpUser turns 2FA on through the real setup, then dates the enrolment an
// hour back so a session created now selects the totp factor.
func (f *tokenEditFixture) totpUser(t *testing.T, email string) (*models.User, string, []string) {
	t.Helper()
	u := f.passwordUser(t, email)
	setupClient, _ := f.session(t, u)
	secret, codes, _ := enable(t, setupClient)
	if _, err := f.store.DB().Exec(`UPDATE users SET totp_enabled_at=? WHERE id=?`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), u.ID); err != nil {
		t.Fatal(err)
	}
	return u, secret, codes
}

func TestTask4507WideningWithTOTP(t *testing.T) {
	f := newTokenEditFixture(t)
	u, secret, _ := f.totpUser(t, "totp@example.com")
	tok := f.mint(t, u, `["read"]`)
	client, _ := f.session(t, u)

	// The next step's code: enrolment consumed the current one.
	code, err := totp.GenerateCode(secret, time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	r := runTokenEditT(client, tok.ID, nil, strPtr("*"), code+"\n", false, false)
	if r.err != nil {
		t.Fatalf("edit: %v\n%s", r.err, r.errOut.String())
	}
	if got := f.stored(t, u, tok.ID).Scopes; got != `["*"]` {
		t.Fatalf("scopes %s, want [\"*\"]", got)
	}
	if !strings.Contains(r.errOut.String(), "Code from your authenticator app (or a recovery code): ") {
		t.Fatalf("no TOTP prompt: %q", r.errOut.String())
	}
}

func TestTask4507WideningWithRecoveryCode(t *testing.T) {
	f := newTokenEditFixture(t)
	u, _, codes := f.totpUser(t, "recovery@example.com")
	if len(codes) == 0 {
		t.Fatal("setup printed no recovery codes")
	}
	tok := f.mint(t, u, `["read"]`)
	client, _ := f.session(t, u)

	r := runTokenEditT(client, tok.ID, nil, strPtr("*"), codes[0]+"\n", false, false)
	if r.err != nil {
		t.Fatalf("edit: %v\n%s", r.err, r.errOut.String())
	}
	if got := f.stored(t, u, tok.ID).Scopes; got != `["*"]` {
		t.Fatalf("scopes %s, want [\"*\"]", got)
	}
}

func TestTask4507WrongPasswordLeavesKey(t *testing.T) {
	f := newTokenEditFixture(t)
	u := f.passwordUser(t, "wrong@example.com")
	tok := f.mint(t, u, `["read"]`)
	client, _ := f.session(t, u)

	r := runTokenEditT(client, tok.ID, nil, strPtr("*"), "not-the-password\n", false, false)
	if r.err == nil || !strings.Contains(r.err.Error(), "incorrect password") || !strings.Contains(r.err.Error(), "the key is unchanged") {
		t.Fatalf("err = %v, want incorrect password", r.err)
	}
	if got := f.stored(t, u, tok.ID).Scopes; got != `["read"]` {
		t.Fatalf("scopes %s, want unchanged [\"read\"]", got)
	}
	if n := f.count("PATCH tokens/" + tok.ID); n != 1 {
		t.Fatalf("%d PATCH calls after a failed verification, want 1", n)
	}
}

func TestTask4507APITokenToldToSignIn(t *testing.T) {
	f := newTokenEditFixture(t)
	u := f.passwordUser(t, "pat@example.com")
	tok := f.mint(t, u, `["*"]`)
	client := f.clientWith(tok.Token)

	r := runTokenEditT(client, tok.ID, strPtr("x"), nil, "", false, false)
	if r.err == nil || !strings.Contains(r.err.Error(), "'pad auth login'") || !strings.Contains(r.err.Error(), "signed-in session") {
		t.Fatalf("err = %v, want a pointer at 'pad auth login'", r.err)
	}
	if got := f.stored(t, u, tok.ID).Name; got != "ci" {
		t.Fatalf("name %q, want unchanged", got)
	}
}

func TestTask4507UnavailableGuidance(t *testing.T) {
	f := newTokenEditFixture(t)
	u := f.oauthUser(t, "oauth@example.com")
	tok := f.mint(t, u, `["read"]`)
	client, _ := f.session(t, u)

	r := runTokenEditT(client, tok.ID, nil, strPtr("*"), "anything\n", false, false)
	if r.err == nil || !strings.Contains(r.err.Error(), "Turn on two-factor authentication") {
		t.Fatalf("err = %v, want the 2FA guidance", r.err)
	}
	if n := f.count("POST step-up/verify"); n != 0 {
		t.Fatalf("verify called %d times with no factor", n)
	}
	if got := f.stored(t, u, tok.ID).Scopes; got != `["read"]` {
		t.Fatalf("scopes %s, want unchanged", got)
	}
}

func TestTask4507NotFound(t *testing.T) {
	f := newTokenEditFixture(t)
	u := f.passwordUser(t, "nf@example.com")
	client, _ := f.session(t, u)
	r := runTokenEditT(client, "no-such-id", strPtr("x"), nil, "", false, false)
	if r.err == nil || r.err.Error() != "token no-such-id not found" {
		t.Fatalf("err = %v", r.err)
	}
}

func TestTask4507LocalValidation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		nameF, scope *string
		want         string
	}{
		{name: "none given", want: "nothing to change: pass --name and/or --scopes"},
		{name: "blank name", nameF: strPtr("   "), want: "--name"},
		{name: "empty scopes", scope: strPtr(""), want: "allowed values: *, read, write"},
		{name: "unknown scope", scope: strPtr("admin"), want: "unknown scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.WriteHeader(500)
			}))
			defer ts.Close()
			client := cli.NewClientFromURL(ts.URL)
			client.SetHTTPClient(ts.Client())
			r := runTokenEditT(client, "tok-1", tc.nameF, tc.scope, "", true, false)
			if r.err == nil || !strings.Contains(r.err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", r.err, tc.want)
			}
			if requests != 0 {
				t.Fatalf("%d requests for a local error", requests)
			}
		})
	}
}

func TestTask4507StepUpErrors(t *testing.T) {
	type reply struct {
		status     int
		code, msg  string
		retryAfter string
	}
	for _, tc := range []struct {
		name        string
		factor      string
		interactive bool
		asJSON      bool
		stdin       string
		patch       *reply // nil: step_up_required until verified
		alwaysStep  bool   // PATCH refuses step_up_required every time
		send        *reply
		verify      *reply
		wantErr     string
		wantCalls   map[string]int
	}{
		{name: "email without a terminal", factor: "email", interactive: false, stdin: "123456\n",
			wantErr: "the key is unchanged", wantCalls: map[string]int{"POST step-up/email": 0, "POST step-up/verify": 0, "PATCH tokens/tok-1": 1}},
		{name: "email rate limited", factor: "email", interactive: true, stdin: "123456\n",
			send:    &reply{status: 429, code: "rate_limited", msg: "Too many verification emails; try again later", retryAfter: "120"},
			wantErr: "about 120s", wantCalls: map[string]int{"POST step-up/verify": 0}},
		{name: "email send failed", factor: "email", interactive: true, stdin: "123456\n",
			send:    &reply{status: 503, code: "email_send_failed", msg: "Could not send the verification email; please try again"},
			wantErr: "Could not send the verification email; please try again; the key is unchanged"},
		{name: "email factor mismatch", factor: "email", interactive: true, stdin: "123456\n",
			send:    &reply{status: 409, code: "step_up_factor_mismatch", msg: "Email is not the current verification factor"},
			wantErr: "your verification method changed; run the command again"},
		{name: "email code", factor: "email", interactive: true, stdin: "123456\n",
			wantCalls: map[string]int{"POST step-up/email": 1, "POST step-up/verify": 1, "PATCH tokens/tok-1": 2}},
		{name: "wrong email code", factor: "email", interactive: true, stdin: "123456\n",
			verify:  &reply{status: 403, code: "invalid_code", msg: "Invalid verification code"},
			wantErr: "incorrect verification code; the key is unchanged"},
		{name: "verify unavailable", factor: "password", stdin: "pw\n",
			verify:  &reply{status: 409, code: "step_up_unavailable", msg: "No verification method is available for this account"},
			wantErr: "Turn on two-factor authentication ('pad auth 2fa setup') to change key access; the key is unchanged"},
		{name: "empty code", factor: "totp", stdin: "",
			wantErr: "no verification code entered; the key is unchanged", wantCalls: map[string]int{"POST step-up/verify": 0}},
		{name: "conflict", factor: "password", patch: &reply{status: 409, code: "conflict", msg: "Token scopes changed; reload and try again"},
			wantErr: "the key's scopes changed meanwhile; check 'pad token list' and retry"},
		{name: "validation", factor: "password", patch: &reply{status: 400, code: "validation_error", msg: "unknown scope"},
			wantErr: "unknown scope"},
		{name: "second refusal", factor: "password", stdin: "pw\n", alwaysStep: true,
			wantErr: "verification did not take effect; run the command again", wantCalls: map[string]int{"PATCH tokens/tok-1": 2, "POST step-up/verify": 1}},
		{name: "json", factor: "password", stdin: "pw\n", asJSON: true,
			wantCalls: map[string]int{"PATCH tokens/tok-1": 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := map[string]int{}
			granted := false
			var verifyBody map[string]string
			write := func(w http.ResponseWriter, rp *reply) {
				if rp.retryAfter != "" {
					w.Header().Set("Retry-After", rp.retryAfter)
				}
				w.WriteHeader(rp.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": rp.code, "message": rp.msg}})
			}
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				path := strings.TrimPrefix(r.URL.Path, "/api/v1/auth/")
				calls[r.Method+" "+path]++
				switch path {
				case "tokens/tok-1":
					if tc.patch != nil {
						write(w, tc.patch)
						return
					}
					if tc.alwaysStep || !granted {
						write(w, &reply{status: 403, code: "step_up_required", msg: "Verify your identity before increasing this key's access"})
						return
					}
					_ = json.NewEncoder(w).Encode(models.APIToken{ID: "tok-1", Name: "ci", Prefix: "pad_abcd", Scopes: `["*"]`})
				case "step-up":
					_ = json.NewEncoder(w).Encode(map[string]any{"factor": tc.factor, "active_until": nil})
				case "step-up/email":
					if tc.send != nil {
						write(w, tc.send)
						return
					}
					_, _ = w.Write([]byte(`{"sent":true}`))
				case "step-up/verify":
					_ = json.NewDecoder(r.Body).Decode(&verifyBody)
					if tc.verify != nil {
						write(w, tc.verify)
						return
					}
					granted = true
					_, _ = w.Write([]byte(`{"active_until":"2099-01-01T00:00:00Z"}`))
				default:
					t.Errorf("unexpected %s %s", r.Method, path)
					w.WriteHeader(404)
				}
			}))
			defer ts.Close()
			client := cli.NewClientFromURL(ts.URL)
			client.SetHTTPClient(ts.Client())

			r := runTokenEditT(client, "tok-1", nil, strPtr("*"), tc.stdin, tc.interactive, tc.asJSON)
			if tc.wantErr != "" {
				if r.err == nil || !strings.Contains(r.err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", r.err, tc.wantErr)
				}
			} else if r.err != nil {
				t.Fatalf("err = %v\n%s", r.err, r.errOut.String())
			}
			for k, want := range tc.wantCalls {
				if calls[k] != want {
					t.Fatalf("%s called %d times, want %d (all calls %v)", k, calls[k], want, calls)
				}
			}
			if tc.name == "email code" {
				if verifyBody["code"] != "123456" || !strings.Contains(r.errOut.String(), "We emailed you a verification code.") {
					t.Fatalf("verify body %v, stderr %q", verifyBody, r.errOut.String())
				}
			}
			if tc.asJSON {
				var got models.APIToken
				if err := json.Unmarshal(r.out.Bytes(), &got); err != nil {
					t.Fatalf("stdout is not one APIToken JSON: %v\n%q", err, r.out.String())
				}
				want, _ := json.MarshalIndent(models.APIToken{ID: "tok-1", Name: "ci", Prefix: "pad_abcd", Scopes: `["*"]`}, "", "  ")
				if strings.TrimSpace(r.out.String()) != string(want) {
					t.Fatalf("stdout %q, want %q", r.out.String(), want)
				}
				if !strings.Contains(r.errOut.String(), "Password: ") {
					t.Fatalf("prompt not on stderr: %q", r.errOut.String())
				}
			}
		})
	}
}

// The TOTP split: six digits is a code, anything else a recovery code.
func TestTask4507TOTPSplit(t *testing.T) {
	for _, tc := range []struct{ in, code, recovery string }{
		{in: "123456", code: "123456"},
		{in: "ABCD-EFGH", recovery: "ABCD-EFGH"},
		{in: "1234567", recovery: "1234567"},
	} {
		var body map[string]string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch strings.TrimPrefix(r.URL.Path, "/api/v1/auth/") {
			case "step-up":
				_, _ = w.Write([]byte(`{"factor":"totp","active_until":null}`))
			case "step-up/verify":
				_ = json.NewDecoder(r.Body).Decode(&body)
				_, _ = w.Write([]byte(`{"active_until":"2099-01-01T00:00:00Z"}`))
			}
		}))
		client := cli.NewClientFromURL(ts.URL)
		client.SetHTTPClient(ts.Client())
		var out bytes.Buffer
		err := runStepUp(client, bufio.NewReader(strings.NewReader(tc.in+"\n")), &out, false, "the key is unchanged")
		ts.Close()
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if body["code"] != tc.code || body["recovery_code"] != tc.recovery {
			t.Fatalf("%s: body %v", tc.in, body)
		}
	}
}
