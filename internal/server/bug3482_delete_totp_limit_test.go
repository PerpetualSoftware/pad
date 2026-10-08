package server

import (
	"net/http"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// BUG-3482: account deletion checked the 2FA code with a bare totp.Validate,
// no attempt limit and no single-use claim, so a hijacked session inside the
// reauth window could grind the six digits against an irreversible delete.
// It now goes through checkSecondFactor, login's check.

func pinRecoveryCodeLimiter(t *testing.T, srv *Server) int {
	t.Helper()
	if srv.rateLimiters == nil || srv.rateLimiters.RecoveryCode == nil {
		t.Skip("rate limiting disabled in this environment (PAD_DISABLE_RATE_LIMITS)")
	}
	// Same burst, no refill inside the test, so "the seventh try" does not
	// depend on how fast six requests run.
	burst := srv.rateLimiters.RecoveryCode.config.Burst
	srv.rateLimiters.RecoveryCode.Stop()
	srv.rateLimiters.RecoveryCode = newIPRateLimiter(rateLimitConfig{Rate: rate.Limit(1.0 / 3600.0), Burst: burst})
	t.Cleanup(srv.rateLimiters.RecoveryCode.Stop)
	return burst
}

func TestDeleteAccount_BUG3482_TOTPAttemptsAreLimited(t *testing.T) {
	srv := testServer(t)
	burst := pinRecoveryCodeLimiter(t, srv)
	srv.SetCloudSidecar(&fakeSidecar{})

	userID, token := bootstrapAccountDeleteUser(t, srv, "")
	secret := enableTOTPForDeleteUser(t, srv, userID)
	for i := 0; i < burst; i++ {
		rr := deleteAccountReq(srv, map[string]interface{}{
			"password":  "correct-horse-battery-staple",
			"totp_code": "000000",
		}, token)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("wrong code %d: %d %s, want 401", i+1, rr.Code, rr.Body.String())
		}
	}
	over := deleteAccountReq(srv, map[string]interface{}{
		"password":  "correct-horse-battery-staple",
		"totp_code": validTOTPCode(t, secret),
	}, token)
	if over.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d with a valid code: %d %s, want 429", burst+1, over.Code, over.Body.String())
	}
	if u, err := srv.store.GetUser(userID); err != nil || u == nil {
		t.Fatalf("the account was deleted past the attempt limit (err %v)", err)
	}
}

// A code that already signed something in is spent: a replay within its
// window is refused, as at login (BUG-2054).
func TestDeleteAccount_BUG3482_TOTPCodeIsSingleUse(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudSidecar(&fakeSidecar{})

	userID, token := bootstrapAccountDeleteUser(t, srv, "")
	secret := enableTOTPForDeleteUser(t, srv, userID)
	code := validTOTPCode(t, secret)
	u, err := srv.store.GetUser(userID)
	if err != nil || u == nil {
		t.Fatalf("GetUser: %v", err)
	}
	step, ok := deriveTOTPStep(code, secret, time.Now().UTC())
	if !ok {
		t.Fatal("precondition: the minted code derives a step")
	}
	if claimed, err := srv.store.ConsumeTOTPStep(userID, u.TOTPSecret, step); err != nil || !claimed {
		t.Fatalf("precondition: claim the step (%v, %v)", claimed, err)
	}
	rr := deleteAccountReq(srv, map[string]interface{}{
		"password":  "correct-horse-battery-staple",
		"totp_code": code,
	}, token)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("a spent code: %d %s, want 401", rr.Code, rr.Body.String())
	}
	if u, err := srv.store.GetUser(userID); err != nil || u == nil {
		t.Fatalf("the account was deleted with a spent code (err %v)", err)
	}
}
