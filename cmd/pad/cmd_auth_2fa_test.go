package main

// TASK-403: `pad auth 2fa setup|disable`, driven against a real server (the
// store, handlers and TOTP check are the production ones) through an
// httptest listener, so every branch below is the server's real answer.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/config"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

const twoFAPassword = "correct-horse-battery-staple"

type twoFAFixture struct {
	store *store.Store
	url   string
}

func newTwoFAFixture(t *testing.T) *twoFAFixture {
	t.Helper()
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &twoFAFixture{store: s, url: ts.URL}
}

// session returns a CLI client signed in as user with a fresh session.
func (f *twoFAFixture) session(t *testing.T, user *models.User) (*cli.Client, string) {
	t.Helper()
	tok, err := f.store.CreateSession(user.ID, "cli-test", "127.0.0.1", "", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return f.clientWith(tok), tok
}

func (f *twoFAFixture) clientWith(token string) *cli.Client {
	c := cli.NewClientFromURL(f.url)
	c.SetAuthToken(token)
	return c
}

func (f *twoFAFixture) passwordUser(t *testing.T, email string) *models.User {
	t.Helper()
	u, err := f.store.CreateUser(models.UserCreate{Email: email, Name: "Pw User", Password: twoFAPassword})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

func (f *twoFAFixture) oauthUser(t *testing.T, email string) *models.User {
	t.Helper()
	u, err := f.store.CreateOAuthUser(email, "OAuth Only", "")
	if err != nil {
		t.Fatalf("CreateOAuthUser: %v", err)
	}
	if u.HasPassword() {
		t.Fatal("fixture: an OAuth user must have no password")
	}
	return u
}

func (f *twoFAFixture) totpEnabled(t *testing.T, id string) bool {
	t.Helper()
	u, err := f.store.GetUser(id)
	if err != nil || u == nil {
		t.Fatalf("GetUser: %v", err)
	}
	return u.TOTPEnabled
}

var secretLine = regexp.MustCompile(`(?m)^  ([A-Z2-7]{16,})$`)

// enable runs setup with a code computed from the secret it printed, so the
// test types what a user would read off the app.
func enable(t *testing.T, client *cli.Client) (secret string, codes []string, out string) {
	t.Helper()
	pr, pw := ioPipe()
	var buf bytes.Buffer
	w := &codeTypingWriter{buf: &buf, pw: pw}
	err := runTwoFactorSetup(client, bufio.NewReader(pr), w)
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, buf.String())
	}
	out = buf.String()
	m := secretLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("setup printed no secret:\n%s", out)
	}
	return m[1], recoveryCodesIn(out), out
}

func recoveryCodesIn(out string) []string {
	_, after, ok := strings.Cut(out, "store them somewhere safe.")
	if !ok {
		return nil
	}
	var codes []string
	for _, l := range strings.Split(after, "\n") {
		if c := strings.TrimSpace(l); c != "" {
			codes = append(codes, c)
		}
	}
	return codes
}

func TestTASK403_SetupEnablesAndShowsRecoveryCodesOnce(t *testing.T) {
	f := newTwoFAFixture(t)
	u := f.passwordUser(t, "setup@example.com")
	client, _ := f.session(t, u)

	secret, codes, out := enable(t, client)
	if !f.totpEnabled(t, u.ID) {
		t.Fatal("2FA is not enabled after setup")
	}
	if !strings.Contains(out, "otpauth://totp/") {
		t.Fatalf("no otpauth URI in the output:\n%s", out)
	}
	if len(codes) == 0 {
		t.Fatalf("no recovery codes printed:\n%s", out)
	}
	for _, c := range codes {
		if n := strings.Count(out, c); n != 1 {
			t.Fatalf("recovery code %q printed %d times, want once", c, n)
		}
	}
	// The secret appears in the URI and on its own line, and nowhere else.
	if n := strings.Count(out, secret); n != 2 {
		t.Fatalf("secret printed %d times, want 2 (the URI and the manual line):\n%s", n, out)
	}
}

func TestTASK403_SetupRetriesAWrongCode(t *testing.T) {
	f := newTwoFAFixture(t)
	u := f.passwordUser(t, "retry@example.com")
	client, _ := f.session(t, u)

	pr, pw := ioPipe()
	var buf bytes.Buffer
	w := &codeTypingWriter{buf: &buf, pw: pw, wrongFirst: 1}
	if err := runTwoFactorSetup(client, bufio.NewReader(pr), w); err != nil {
		t.Fatalf("setup: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "That code didn't match") {
		t.Fatalf("no retry prompt after a wrong code:\n%s", buf.String())
	}
	if !f.totpEnabled(t, u.ID) {
		t.Fatal("the right code on the second try did not enable 2FA")
	}
}

func TestTASK403_SetupGivesUpAfterThreeWrongCodesWithoutLeakingTheSecret(t *testing.T) {
	f := newTwoFAFixture(t)
	u := f.passwordUser(t, "giveup@example.com")
	client, _ := f.session(t, u)

	pr, pw := ioPipe()
	var buf bytes.Buffer
	w := &codeTypingWriter{buf: &buf, pw: pw, wrongFirst: maxTOTPVerifyAttempts}
	err := runTwoFactorSetup(client, bufio.NewReader(pr), w)
	if err == nil || !strings.Contains(err.Error(), "still off") {
		t.Fatalf("err = %v, want a refusal saying 2FA is still off", err)
	}
	if f.totpEnabled(t, u.ID) {
		t.Fatal("2FA was enabled by wrong codes")
	}
	m := secretLine.FindStringSubmatch(buf.String())
	if m == nil {
		t.Fatal("no secret displayed")
	}
	if strings.Contains(err.Error(), m[1]) {
		t.Fatal("the error message carries the TOTP secret")
	}
}

func TestTASK403_SetupEmptyCodeLeavesItOff(t *testing.T) {
	f := newTwoFAFixture(t)
	u := f.passwordUser(t, "empty@example.com")
	client, _ := f.session(t, u)
	var out bytes.Buffer
	err := runTwoFactorSetup(client, bufio.NewReader(strings.NewReader("\n")), &out)
	if err == nil || !strings.Contains(err.Error(), "still off") {
		t.Fatalf("err = %v, want 'still off'", err)
	}
	if f.totpEnabled(t, u.ID) {
		t.Fatal("2FA enabled with no code")
	}
}

func TestTASK403_SetupWhenAlreadyOnRefusesAndKeepsTheSecret(t *testing.T) {
	f := newTwoFAFixture(t)
	u := f.passwordUser(t, "already@example.com")
	client, _ := f.session(t, u)
	enable(t, client)
	before, _ := f.store.GetUser(u.ID)

	var out bytes.Buffer
	err := runTwoFactorSetup(client, bufio.NewReader(strings.NewReader("")), &out)
	if err == nil || !strings.Contains(err.Error(), "already on") {
		t.Fatalf("err = %v, want 'already on'", err)
	}
	if strings.Contains(out.String(), "otpauth://") {
		t.Fatalf("a new secret was shown for an account that has 2FA on:\n%s", out.String())
	}
	after, _ := f.store.GetUser(u.ID)
	if after.TOTPSecret != before.TOTPSecret {
		t.Fatal("the stored secret changed")
	}
}

func TestTASK403_APITokenIsToldToSignIn(t *testing.T) {
	f := newTwoFAFixture(t)
	u := f.passwordUser(t, "pat@example.com")
	tok, err := f.store.CreateAPIToken(u.ID, models.APITokenCreate{Name: "t"}, 30, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	var out bytes.Buffer
	err = runTwoFactorSetup(f.clientWith(tok.Token), bufio.NewReader(strings.NewReader("")), &out)
	if err == nil || !strings.Contains(err.Error(), "pad auth login") {
		t.Fatalf("err = %v, want the sign-in hint", err)
	}
}

func TestTASK403_DisableWithPassword(t *testing.T) {
	f := newTwoFAFixture(t)
	u := f.passwordUser(t, "disable@example.com")
	client, oldTok := f.session(t, u)
	enable(t, client)

	// A wrong password leaves it on.
	var out bytes.Buffer
	err := runTwoFactorDisable(client, bufio.NewReader(strings.NewReader("wrong-password\n")), &out, func(string) error {
		t.Fatal("saveToken called on a refused disable")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "incorrect password") {
		t.Fatalf("err = %v, want 'incorrect password'", err)
	}
	if !f.totpEnabled(t, u.ID) {
		t.Fatal("a wrong password disabled 2FA")
	}
	if !strings.Contains(out.String(), "Password: ") {
		t.Fatalf("a password account was not asked for its password:\n%s", out.String())
	}

	// The right one turns it off and hands back a working session.
	var saved string
	out.Reset()
	if err := runTwoFactorDisable(client, bufio.NewReader(strings.NewReader(twoFAPassword+"\n")), &out, func(tok string) error {
		saved = tok
		return nil
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if f.totpEnabled(t, u.ID) {
		t.Fatal("2FA still on")
	}
	if saved == "" || saved == oldTok {
		t.Fatalf("saved token %q, want a new session", saved)
	}
	if _, err := f.clientWith(saved).GetAccountSecurity(); err != nil {
		t.Fatalf("the replacement session does not work: %v", err)
	}
	if _, err := f.clientWith(oldTok).GetAccountSecurity(); err == nil {
		t.Fatal("the old session still works after the rotation, so the saved token was never needed: this test proves nothing")
	}
}

func TestTASK403_DisableWithoutPasswordTakesACode(t *testing.T) {
	f := newTwoFAFixture(t)
	u := f.oauthUser(t, "oauth-code@example.com")
	client, _ := f.session(t, u)
	secret, _, _ := enable(t, client)

	// A wrong code leaves it on.
	var out bytes.Buffer
	err := runTwoFactorDisable(client, bufio.NewReader(strings.NewReader("000000\n")), &out, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "didn't work") {
		t.Fatalf("err = %v, want the code refusal", err)
	}
	if !f.totpEnabled(t, u.ID) {
		t.Fatal("a wrong code disabled 2FA")
	}
	if strings.Contains(out.String(), "Password: ") || !strings.Contains(out.String(), "no password") {
		t.Fatalf("an account with no password was asked for one:\n%s", out.String())
	}

	// A current code turns it off. Login's single-use claim refuses a code
	// already spent in this window, so take the next one.
	code, err := totp.GenerateCode(secret, time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runTwoFactorDisable(client, bufio.NewReader(strings.NewReader(code+"\n")), &out, func(string) error { return nil }); err != nil {
		t.Fatalf("disable with a code: %v\n%s", err, out.String())
	}
	if f.totpEnabled(t, u.ID) {
		t.Fatal("2FA still on")
	}
}

func TestTASK403_DisableWithoutPasswordTakesARecoveryCode(t *testing.T) {
	f := newTwoFAFixture(t)
	u := f.oauthUser(t, "oauth-recovery@example.com")
	client, _ := f.session(t, u)
	_, codes, _ := enable(t, client)
	if len(codes) == 0 {
		t.Fatal("no recovery codes")
	}
	var out bytes.Buffer
	if err := runTwoFactorDisable(client, bufio.NewReader(strings.NewReader(codes[0]+"\n")), &out, func(string) error { return nil }); err != nil {
		t.Fatalf("disable with a recovery code: %v\n%s", err, out.String())
	}
	if f.totpEnabled(t, u.ID) {
		t.Fatal("2FA still on")
	}
}

func TestTASK403_DisableWhenOffRefuses(t *testing.T) {
	f := newTwoFAFixture(t)
	u := f.passwordUser(t, "off@example.com")
	client, _ := f.session(t, u)
	var out bytes.Buffer
	err := runTwoFactorDisable(client, bufio.NewReader(strings.NewReader("")), &out, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "not on") {
		t.Fatalf("err = %v, want 'not on'", err)
	}
}

// A server that predates `password_set` took only a password, so the CLI
// asks for one when the key is missing.
func TestTASK403_DisableAgainstAnOlderServerAsksForThePassword(t *testing.T) {
	var sent map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "u1", "totp_enabled": true})
		case "/api/v1/auth/2fa/disable":
			_ = json.NewDecoder(r.Body).Decode(&sent)
			_ = json.NewEncoder(w).Encode(map[string]any{"enabled": false, "token": "padsess_new"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()
	c := cli.NewClientFromURL(ts.URL)
	c.SetAuthToken("padsess_old")
	var out bytes.Buffer
	if err := runTwoFactorDisable(c, bufio.NewReader(strings.NewReader("pw\n")), &out, func(string) error { return nil }); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if sent["password"] != "pw" || sent["code"] != "" || sent["recovery_code"] != "" {
		t.Fatalf("sent %v, want only the password", sent)
	}
}

func ioPipe() (*io.PipeReader, *io.PipeWriter) { return io.Pipe() }

// codeTypingWriter stands in for the user at setup's prompt: each time the
// prompt is written it types a code into stdin, the first wrongFirst of them
// wrong (the current code with its first digit changed, so never valid).
type codeTypingWriter struct {
	buf        *bytes.Buffer
	pw         *io.PipeWriter
	wrongFirst int
	prompts    int
}

func (w *codeTypingWriter) Write(p []byte) (int, error) {
	n, _ := w.buf.Write(p)
	if strings.Contains(string(p), "Code from the app: ") {
		w.prompts++
		m := secretLine.FindStringSubmatch(w.buf.String())
		code := "000000"
		if m != nil {
			if c, err := totp.GenerateCode(m[1], time.Now()); err == nil {
				code = c
			}
		}
		if w.prompts <= w.wrongFirst {
			code = wrongCode(code)
		}
		go func() { _, _ = w.pw.Write([]byte(code + "\n")) }()
	}
	return n, nil
}

// wrongCode changes a code's first digit, so it can never be the valid one.
func wrongCode(code string) string {
	first := (code[0]-'0'+1)%10 + '0'
	return string(first) + code[1:]
}

// Disable's rotation replaces this CLI's session; the saved credential must
// follow it, and only the token changes.
func TestTASK403_ReplaceSessionTokenKeepsTheRestOfTheCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_TOKEN", "")
	cfg := &config.Config{URL: "http://pad.test:7777"}
	st, err := cli.LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	st.Set(cfg.BaseURL(), &cli.Credentials{Token: "padsess_old", UserID: "u1", Email: "a@example.com", Name: "A"})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if err := replaceSessionToken(cfg, "padsess_new"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	st, _ = cli.LoadStore()
	got := st.Get(cfg.BaseURL())
	if got == nil || got.Token != "padsess_new" || got.UserID != "u1" || got.Email != "a@example.com" || got.Name != "A" {
		t.Fatalf("saved %+v, want the new token and the rest unchanged", got)
	}

	// A PAD_TOKEN session is in the environment; it is reported, not written.
	t.Setenv("PAD_TOKEN", "padsess_env")
	if err := replaceSessionToken(cfg, "padsess_newer"); err == nil || !strings.Contains(err.Error(), "PAD_TOKEN") {
		t.Fatalf("err = %v, want the PAD_TOKEN refusal", err)
	}
	st, _ = cli.LoadStore()
	if st.Get(cfg.BaseURL()).Token != "padsess_new" {
		t.Fatal("the saved credential was overwritten for a PAD_TOKEN session")
	}
}
