package server

import (
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

// bug4703NoFactor turns the env's account into one with no verification
// factor: no password, no mail sender.
func bug4703NoFactor(t *testing.T, e *tokenAuthEnv) {
	t.Helper()
	if _, err := e.srv.store.DB().Exec(`UPDATE users SET password_set=0 WHERE id=?`, e.user.ID); err != nil {
		t.Fatal(err)
	}
	task3867Factor(t, e, "unavailable")
}

func TestBug4703TOTPEnrolNullSignInRefused(t *testing.T) {
	e := setupTokenAuthEnv(t)
	bug4703NoFactor(t, e)
	now := time.Now()
	task4693SignedIn(t, e, &now)
	rr := doRequestWithBearer(e.srv, "POST", "/api/v1/auth/2fa/setup", e.sessTok, nil)
	task3867Want(t, rr, 200, "")
	var setup struct {
		Secret string `json:"secret"`
	}
	parseJSON(t, rr, &setup)
	stored := task4509StoredSecret(t, e)
	if stored == "" {
		t.Fatal("setup stored no pending secret")
	}
	// No stored sign-in time; created_at stays fresh and must not stand in for it.
	task4693SignedIn(t, e, nil)
	code, err := totp.GenerateCode(setup.Secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	task3867Want(t, doRequestWithBearer(e.srv, "POST", "/api/v1/auth/2fa/setup", e.sessTok, nil), 403, "reauth_required")
	task3867Want(t, doRequestWithBearer(e.srv, "POST", "/api/v1/auth/2fa/verify", e.sessTok, map[string]string{"secret": setup.Secret, "code": code}), 403, "reauth_required")
	if got := task4509StoredSecret(t, e); got != stored {
		t.Fatal("reauth refusal changed the pending secret")
	}
	u, err := e.srv.store.GetUser(e.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.TOTPEnabled {
		t.Fatal("refused enrolment enabled TOTP")
	}
}

func TestBug4703TOTPEnrolStoredFreshSignInSucceeds(t *testing.T) {
	e := setupTokenAuthEnv(t)
	bug4703NoFactor(t, e)
	now := time.Now()
	task4693SignedIn(t, e, &now)
	rr := doRequestWithBearer(e.srv, "POST", "/api/v1/auth/2fa/setup", e.sessTok, nil)
	task3867Want(t, rr, 200, "")
	var setup struct {
		Secret string `json:"secret"`
	}
	parseJSON(t, rr, &setup)
	code, err := totp.GenerateCode(setup.Secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	task3867Want(t, doRequestWithBearer(e.srv, "POST", "/api/v1/auth/2fa/verify", e.sessTok, map[string]string{"secret": setup.Secret, "code": code}), 200, "")
	u, err := e.srv.store.GetUser(e.user.ID)
	if err != nil || !u.TOTPEnabled {
		t.Fatal("TOTP not enabled", err)
	}
}
