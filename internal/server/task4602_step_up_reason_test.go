package server

// TASK-4602: when no step-up factor is available, the status says why, and
// verify's refusal says what to do. An account that just turned on 2FA on
// this session must be told to sign in again, not to turn 2FA on.

import (
	"strings"
	"testing"
	"time"
)

func task4602NoPassword(t *testing.T, e *tokenAuthEnv) {
	t.Helper()
	if _, err := e.srv.store.DB().Exec(`UPDATE users SET password_set=0 WHERE id=?`, e.user.ID); err != nil {
		t.Fatal(err)
	}
}

// task4602Status returns the raw status body, so an absent key is visible.
func task4602Status(t *testing.T, e *tokenAuthEnv) map[string]any {
	t.Helper()
	rr := doRequestWithBearer(e.srv, "GET", "/api/v1/auth/step-up", e.sessTok, nil)
	task3867Want(t, rr, 200, "")
	var got map[string]any
	parseJSON(t, rr, &got)
	return got
}

func TestTask4602SignInAgainWhenSessionPredatesTOTP(t *testing.T) {
	e := setupTokenAuthEnv(t)
	task4602NoPassword(t, e)
	task3867EnableTOTP(t, e)
	// Enrolment after this session's sign-in: the session predates 2FA.
	if _, err := e.srv.store.DB().Exec(`UPDATE users SET totp_enabled_at=? WHERE id=?`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), e.user.ID); err != nil {
		t.Fatal(err)
	}
	got := task4602Status(t, e)
	if got["factor"] != "unavailable" || got["unavailable_reason"] != "sign_in_again" {
		t.Fatalf("status %v, want factor unavailable, unavailable_reason sign_in_again", got)
	}
	rr := task3867Verify(e, map[string]string{"code": "123456"})
	task3867Want(t, rr, 409, "step_up_unavailable")
	want := "This session started before two-factor authentication was turned on. Sign in again, then retry within 10 minutes"
	if !strings.Contains(rr.Body.String(), want) {
		t.Fatalf("verify body %s, want %q", rr.Body.String(), want)
	}
}

func TestTask4602NoFactorReason(t *testing.T) {
	e := setupTokenAuthEnv(t)
	task4602NoPassword(t, e)
	got := task4602Status(t, e)
	if got["factor"] != "unavailable" || got["unavailable_reason"] != "no_factor" {
		t.Fatalf("status %v, want factor unavailable, unavailable_reason no_factor", got)
	}
	rr := task3867Verify(e, map[string]string{"password": "x"})
	task3867Want(t, rr, 409, "step_up_unavailable")
	if !strings.Contains(rr.Body.String(), "No verification method is available for this account") {
		t.Fatalf("verify body %s, want the unchanged no-factor message", rr.Body.String())
	}
}

func TestTask4602ReasonAbsentWhenFactorAvailable(t *testing.T) {
	for _, factor := range []string{"totp", "email", "password"} {
		t.Run(factor, func(t *testing.T) {
			e := setupTokenAuthEnv(t)
			switch factor {
			case "totp":
				task3867EnableTOTP(t, e)
			case "email":
				task3867Mail(t, e)
			}
			got := task4602Status(t, e)
			if got["factor"] != factor {
				t.Fatalf("factor %v, want %s", got["factor"], factor)
			}
			if _, ok := got["unavailable_reason"]; ok {
				t.Fatalf("unavailable_reason present with factor %s: %v", factor, got)
			}
		})
	}
}
