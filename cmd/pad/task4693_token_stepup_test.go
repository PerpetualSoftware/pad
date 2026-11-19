package main

// TASK-4693: `pad token create` and `pad token rotate` step up on
// step_up_required and retry once, against a real server.

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// staleSession is a session signed in 30 minutes ago, past the 10-minute
// window and after a totpUser's enrolment (dated an hour back).
func (f *tokenEditFixture) staleSession(t *testing.T, u *models.User) *cli.Client {
	t.Helper()
	tok, err := f.store.CreateSessionIssuedAt(u.ID, "cli-test", "127.0.0.1", "", 2*time.Hour, time.Now().Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("CreateSessionIssuedAt: %v", err)
	}
	return f.clientWith(tok)
}

func task4693Create(client *cli.Client, stdin string, errOut *bytes.Buffer) (*models.APITokenWithSecret, error) {
	var token *models.APITokenWithSecret
	err := retryAfterStepUp(client, bufio.NewReader(strings.NewReader(stdin)), errOut, false, "no token was created", func() (err error) {
		token, err = client.CreateUserToken(models.APITokenCreate{Name: "scripted"})
		return err
	})
	return token, keyStepUpError(err, "no token was created")
}

func TestTask4693CreateFreshSessionNoPrompt(t *testing.T) {
	f := newTokenEditFixture(t)
	u := f.passwordUser(t, "fresh@example.com")
	client, _ := f.session(t, u)
	var errOut bytes.Buffer
	if tok, err := task4693Create(client, "", &errOut); err != nil || tok == nil || tok.Token == "" {
		t.Fatalf("create: %v %v", tok, err)
	}
	if f.count("GET step-up") != 0 || errOut.Len() != 0 {
		t.Fatalf("a fresh session prompted: %q", errOut.String())
	}
}

func TestTask4693CreateStaleSessionStepsUpOnce(t *testing.T) {
	f := newTokenEditFixture(t)
	u := f.passwordUser(t, "stale@example.com")
	client := f.staleSession(t, u)
	var errOut bytes.Buffer
	tok, err := task4693Create(client, twoFAPassword, &errOut)
	if err != nil || tok == nil || tok.Token == "" {
		t.Fatalf("create: %v %v\n%s", tok, err, errOut.String())
	}
	if !strings.Contains(errOut.String(), "Password: ") {
		t.Fatalf("no password prompt: %q", errOut.String())
	}
	if n := f.count("POST tokens"); n != 2 {
		t.Fatalf("%d create calls, want 2 (refused, then retried)", n)
	}
}

func TestTask4693RotateStaleSessionStepsUp(t *testing.T) {
	f := newTokenEditFixture(t)
	u := f.passwordUser(t, "rotate@example.com")
	old := f.mint(t, u, `["read"]`)
	client := f.staleSession(t, u)
	var errOut bytes.Buffer
	var rotated *models.APITokenWithSecret
	err := retryAfterStepUp(client, bufio.NewReader(strings.NewReader("wrong-password\n")), &errOut, false, "the old secret still works", func() (err error) {
		rotated, err = client.RotateUserToken(old.ID, 0)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "incorrect password; the old secret still works") {
		t.Fatalf("err = %v, want the wrong-password refusal", err)
	}
	if v, verr := f.store.ValidateToken(old.Token); verr != nil || v == nil {
		t.Fatalf("refused rotate killed the old secret: %v", verr)
	}
	err = retryAfterStepUp(client, bufio.NewReader(strings.NewReader(twoFAPassword+"\n")), &errOut, false, "the old secret still works", func() (err error) {
		rotated, err = client.RotateUserToken(old.ID, 0)
		return err
	})
	if err != nil || rotated == nil || rotated.Token == old.Token {
		t.Fatalf("rotate: %v %v", rotated, err)
	}
}

func TestTask4693CreateNoFactorToldToSignIn(t *testing.T) {
	f := newTokenEditFixture(t)
	u := f.oauthUser(t, "nofactor@example.com")
	client := f.staleSession(t, u)
	var errOut bytes.Buffer
	_, err := task4693Create(client, "anything\n", &errOut)
	if err == nil || !strings.Contains(err.Error(), "sign out of Pad in the browser and sign in again") || !strings.Contains(err.Error(), "no token was created") {
		t.Fatalf("err = %v, want the sign-in-again guidance", err)
	}
	if n := f.count("POST step-up/verify"); n != 0 {
		t.Fatalf("verify called %d times with no factor", n)
	}
	if toks, lerr := f.store.ListUserAPITokens(u.ID); lerr != nil || len(toks) != 0 {
		t.Fatalf("a refused create left %d keys: %v", len(toks), lerr)
	}
}

// A browser-approved CLI session carries the approving browser session's
// sign-in time, so 'pad auth login' alone does not reopen the window: the
// remedy must say to sign out and in again in the browser first, and to
// drop the stored CLI session so 'pad auth login' does not short-circuit.
func TestTask4693SignInAgainNamesBrowserSignOutAndIn(t *testing.T) {
	for _, want := range []string{
		"sign out of Pad in the browser and sign in again",
		"'pad auth logout'",
		"'pad auth login'",
		"within 10 minutes",
	} {
		if !strings.Contains(signInAgainForKeys, want) {
			t.Errorf("signInAgainForKeys = %q, want it to contain %q", signInAgainForKeys, want)
		}
	}
	if strings.Contains(signInAgainForKeys, "sign in again ('pad auth login')") {
		t.Errorf("signInAgainForKeys still says 'pad auth login' alone reopens the window: %q", signInAgainForKeys)
	}
}

// The help must not promise that 'pad auth login' alone reopens the window.
func TestTask4693HelpNamesTheWorkingSignIn(t *testing.T) {
	cmds := map[string]string{"token": tokenCmd().Long, "create": tokenCreateCmd().Long, "rotate": tokenRotateCmd().Long, "edit": tokenEditCmd().Long}
	for name, long := range cmds {
		flat := strings.Join(strings.Fields(long), " ")
		if !strings.Contains(flat, "sign out of Pad in the browser and sign in again") || !strings.Contains(flat, "'pad auth login -i'") {
			t.Errorf("%s help does not name the browser sign-out-and-in and 'pad auth login -i' remedies:\n%s", name, long)
		}
	}
}
