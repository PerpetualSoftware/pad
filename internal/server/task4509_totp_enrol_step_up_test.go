package server

import (
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func TestTask4509TOTPRequiresStepUp(t *testing.T) {
	for _, factor := range []string{"password", "email"} {
		t.Run(factor, func(t *testing.T) {
			e := setupTokenAuthEnv(t)
			if factor == "email" {
				task3867Mail(t, e)
			}
			for _, path := range []string{"setup", "verify"} {
				rr := doRequestWithBearer(e.srv, "POST", "/api/v1/auth/2fa/"+path, e.sessTok, map[string]string{})
				task3867Want(t, rr, 403, "step_up_required")
			}
			u, err := e.srv.store.GetUser(e.user.ID)
			if err != nil {
				t.Fatal(err)
			}
			if task4509StoredSecret(t, e) != "" || u.TOTPEnabled {
				t.Fatal("refused enrolment changed TOTP")
			}
		})
	}
}

func TestTask4509TOTPGrantAndExpiry(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "expired"}[expire], func(t *testing.T) {
			e := setupTokenAuthEnv(t)
			grant := func() {
				task3867Want(t, task3867Verify(e, map[string]string{"password": "correct-horse-battery-staple"}), 200, "")
			}
			grant()
			rr := doRequestWithBearer(e.srv, "POST", "/api/v1/auth/2fa/setup", e.sessTok, nil)
			task3867Want(t, rr, 200, "")
			var setup struct {
				Secret string `json:"secret"`
			}
			parseJSON(t, rr, &setup)
			storedSecret := task4509StoredSecret(t, e)
			code, err := totp.GenerateCode(setup.Secret, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			body := map[string]string{"secret": setup.Secret, "code": code}
			if expire {
				info, err := e.srv.store.ValidateSession(e.sessTok)
				if err != nil || info == nil {
					t.Fatal("missing session", err)
				}
				if _, err := e.srv.store.DB().Exec(`UPDATE sessions SET step_up_until=? WHERE id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), info.ID); err != nil {
					t.Fatal(err)
				}
				task3867Want(t, doRequestWithBearer(e.srv, "POST", "/api/v1/auth/2fa/verify", e.sessTok, body), 403, "step_up_required")
				u, err := e.srv.store.GetUser(e.user.ID)
				if err != nil {
					t.Fatal(err)
				}
				if u.TOTPEnabled || task4509StoredSecret(t, e) != storedSecret {
					t.Fatal("expired grant changed pending enrolment")
				}
				grant()
			}
			task3867Want(t, doRequestWithBearer(e.srv, "POST", "/api/v1/auth/2fa/verify", e.sessTok, body), 200, "")
			u, err := e.srv.store.GetUser(e.user.ID)
			if err != nil || !u.TOTPEnabled {
				t.Fatal("TOTP not enabled", err)
			}
		})
	}
}

func TestTask4509TOTPUnavailableNeedsRecentSignIn(t *testing.T) {
	e := setupTokenAuthEnv(t)
	if _, err := e.srv.store.DB().Exec(`UPDATE users SET password_set=0 WHERE id=?`, e.user.ID); err != nil {
		t.Fatal(err)
	}
	task3867Factor(t, e, "unavailable")
	rr := doRequestWithBearer(e.srv, "POST", "/api/v1/auth/2fa/setup", e.sessTok, nil)
	task3867Want(t, rr, 200, "")
	var setup struct {
		Secret string `json:"secret"`
	}
	parseJSON(t, rr, &setup)
	info, err := e.srv.store.ValidateSession(e.sessTok)
	if err != nil || info == nil {
		t.Fatal("missing session", err)
	}
	// created_at stays fresh: the fallback must use the actual sign-in time.
	if _, err := e.srv.store.DB().Exec(`UPDATE sessions SET signed_in_at=? WHERE id=?`, time.Now().Add(-11*time.Minute).UTC().Format(time.RFC3339), info.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"setup", "verify"} {
		task3867Want(t, doRequestWithBearer(e.srv, "POST", "/api/v1/auth/2fa/"+path, e.sessTok, map[string]string{"secret": setup.Secret, "code": "123456"}), 403, "reauth_required")
	}
}

func TestTask4509TOTPExistingRefusals(t *testing.T) {
	e := setupTokenAuthEnv(t)
	for _, path := range []string{"setup", "verify"} {
		task3867Want(t, doRequestWithBearer(e.srv, "POST", "/api/v1/auth/2fa/"+path, e.patTok, nil), 403, "forbidden")
	}
	task3867EnableTOTP(t, e)
	for _, path := range []string{"setup", "verify"} {
		task3867Want(t, doRequestWithBearer(e.srv, "POST", "/api/v1/auth/2fa/"+path, e.sessTok, nil), 409, "conflict")
	}
}

func task4509StoredSecret(t *testing.T, e *tokenAuthEnv) string {
	t.Helper()
	var secret string
	if err := e.srv.store.DB().QueryRow(`SELECT COALESCE(totp_secret, '') FROM users WHERE id=?`, e.user.ID).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	return secret
}
