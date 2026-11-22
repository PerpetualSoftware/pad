package main

// TASK-4602: an account that turned on 2FA on this session has no usable
// step-up factor until it signs in again. The CLI must say that, not tell it
// to turn 2FA on, while an account with no factor keeps today's remedy.

import (
	"bufio"
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

const task4602SignInAgain = "this session started before two-factor authentication was turned on: sign out of Pad in the browser and sign in again, then run 'pad auth logout' and 'pad auth login' and the command within 10 minutes; "

// task4602TOTPNoFactorUser is an OAuth user (no password, no email sender on
// the test server) whose 2FA was turned on now, after a stale session's
// sign-in: the session predates enrolment.
func (f *tokenEditFixture) task4602TOTPNoFactorUser(t *testing.T) (*models.User, *cli.Client) {
	t.Helper()
	u := f.oauthUser(t, "late2fa@example.com")
	client := f.staleSession(t, u)
	const secret = "JBSWY3DPEHPK3PXP"
	if err := f.store.SetTOTPSecret(u.ID, secret); err != nil {
		t.Fatal(err)
	}
	if err := f.store.EnableTOTP(u.ID, secret, ""); err != nil {
		t.Fatal(err)
	}
	return u, client
}

func TestTask4602CreateSignInAgainAfterEnrol(t *testing.T) {
	f := newTokenEditFixture(t)
	_, client := f.task4602TOTPNoFactorUser(t)
	status, err := client.StepUpStatus()
	if err != nil || status.Factor != "unavailable" || status.UnavailableReason != "sign_in_again" {
		t.Fatalf("status %+v %v, want unavailable/sign_in_again", status, err)
	}
	var errOut bytes.Buffer
	_, err = task4693Create(client, "anything\n", &errOut)
	if err == nil || err.Error() != task4602SignInAgain+"no token was created" {
		t.Fatalf("err = %v, want %q", err, task4602SignInAgain+"no token was created")
	}
	if n := f.count("POST step-up/verify"); n != 0 {
		t.Fatalf("verify called %d times with no factor", n)
	}
}

func TestTask4602CreateNoFactorKeepsTodaysText(t *testing.T) {
	f := newTokenEditFixture(t)
	u := f.oauthUser(t, "nofactor4602@example.com")
	client := f.staleSession(t, u)
	var errOut bytes.Buffer
	_, err := task4693Create(client, "anything\n", &errOut)
	if err == nil || err.Error() != signInAgainForKeys+"no token was created" {
		t.Fatalf("err = %v, want today's no-factor text", err)
	}
}

// An older server sends no unavailable_reason: today's text, unchanged.
func TestTask4602RunStepUpReasons(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"sign_in_again", `{"factor":"unavailable","unavailable_reason":"sign_in_again","active_until":null}`, task4602SignInAgain + "the key is unchanged"},
		{"no_factor", `{"factor":"unavailable","unavailable_reason":"no_factor","active_until":null}`, signInAgainForKeys + "the key is unchanged"},
		{"absent", `{"factor":"unavailable","active_until":null}`, signInAgainForKeys + "the key is unchanged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/auth/step-up" {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()
			client := cli.NewClientFromURL(ts.URL)
			client.SetHTTPClient(ts.Client())
			client.SetAuthToken("padsess_test")
			err := runStepUp(client, bufio.NewReader(strings.NewReader("")), &bytes.Buffer{}, false, "the key is unchanged")
			if !errors.Is(err, errStepUpUnavailable) {
				t.Fatalf("runStepUp err = %v, want errStepUpUnavailable", err)
			}
			if got := keyStepUpError(err, "the key is unchanged"); got == nil || got.Error() != tc.want {
				t.Fatalf("keyStepUpError = %v, want %q", got, tc.want)
			}
			if got := totpEnrolStepUp(client, bufio.NewReader(strings.NewReader("")), &bytes.Buffer{}); got != nil {
				t.Fatalf("totpEnrolStepUp = %v, want nil", got)
			}
		})
	}
}

func TestTask4602TOTPEnrolStepUpProceedsForBoth(t *testing.T) {
	f := newTokenEditFixture(t)
	_, late := f.task4602TOTPNoFactorUser(t)
	none := f.staleSession(t, f.oauthUser(t, "none4602@example.com"))
	for name, client := range map[string]*cli.Client{"sign_in_again": late, "no_factor": none} {
		if err := totpEnrolStepUp(client, bufio.NewReader(strings.NewReader("")), &bytes.Buffer{}); err != nil {
			t.Errorf("%s: totpEnrolStepUp = %v, want nil", name, err)
		}
	}
}
