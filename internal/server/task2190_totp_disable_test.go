package server

import (
	"net/http"
	"strings"
	"testing"

	"golang.org/x/time/rate"
)

// TASK-2190: an account with no password (signed up through Google, GitHub or
// Apple) could never turn 2FA off: the door demanded a password it does not
// have. It now proves itself with the second factor, through login's own
// check (checkSecondFactor): the same limiter, counted per user, and the same
// single-use claim, so a stolen session cannot grind the six digits.

// passwordlessTOTPUser is an account created through a provider (no password)
// with TOTP on, holding the given recovery codes, and a live session.
func passwordlessTOTPUser(t *testing.T, srv *Server, email string, recoveryCodes ...string) (userID, secret, session string) {
	t.Helper()
	u, err := srv.store.CreateOAuthUser(email, "No Password", "")
	if err != nil {
		t.Fatalf("CreateOAuthUser: %v", err)
	}
	if u.HasPassword() {
		t.Fatal("precondition: an OAuth-created account has no password")
	}
	secret = "JBSWY3DPEHPK3PXP" // standard base32 test vector
	if err := srv.store.SetTOTPSecret(u.ID, secret); err != nil {
		t.Fatalf("SetTOTPSecret: %v", err)
	}
	hashed := "placeholder-hashed-recovery-code"
	if len(recoveryCodes) > 0 {
		hashed = strings.Join(hashRecoveryCodes(recoveryCodes), "\n")
	}
	if err := srv.store.EnableTOTP(u.ID, secret, hashed); err != nil {
		t.Fatalf("EnableTOTP: %v", err)
	}
	tok, err := srv.store.CreateSession(u.ID, "test", "192.0.2.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return u.ID, secret, tok
}

func totpEnabled(t *testing.T, srv *Server, userID string) bool {
	t.Helper()
	u, err := srv.store.GetUser(userID)
	if err != nil || u == nil {
		t.Fatalf("GetUser: %v", err)
	}
	return u.TOTPEnabled
}

func TestTOTPDisable_TASK2190_PasswordlessUsesTheCode(t *testing.T) {
	srv := testServer(t)
	userID, secret, tok := passwordlessTOTPUser(t, srv, "nopw-2190@example.com")

	missing := doRequestWithCookie(srv, "POST", "/api/v1/auth/2fa/disable", map[string]any{}, tok)
	if missing.Code != http.StatusBadRequest || !strings.Contains(missing.Body.String(), "no password") {
		t.Fatalf("no factor: %d %s, want 400 naming the missing password", missing.Code, missing.Body.String())
	}
	wrong := doRequestWithCookie(srv, "POST", "/api/v1/auth/2fa/disable", map[string]any{"code": "000000"}, tok)
	if wrong.Code != http.StatusForbidden || !strings.Contains(wrong.Body.String(), "invalid_code") {
		t.Fatalf("wrong code: %d %s, want 403 invalid_code", wrong.Code, wrong.Body.String())
	}
	if !totpEnabled(t, srv, userID) {
		t.Fatal("a wrong code disabled 2FA")
	}
	ok := doRequestWithCookie(srv, "POST", "/api/v1/auth/2fa/disable", map[string]any{"code": validTOTPCode(t, secret)}, tok)
	if ok.Code != http.StatusOK {
		t.Fatalf("valid code: %d %s", ok.Code, ok.Body.String())
	}
	if totpEnabled(t, srv, userID) {
		t.Fatal("a valid code left 2FA on")
	}
}

func TestTOTPDisable_TASK2190_PasswordlessRecoveryCode(t *testing.T) {
	srv := testServer(t)
	codes, err := generateRecoveryCodes(2)
	if err != nil {
		t.Fatal(err)
	}
	userID, _, tok := passwordlessTOTPUser(t, srv, "nopw-rc-2190@example.com", codes...)
	rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/2fa/disable", map[string]any{"recovery_code": strings.ToLower(codes[0])}, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("recovery code: %d %s", rr.Code, rr.Body.String())
	}
	if totpEnabled(t, srv, userID) {
		t.Fatal("a valid recovery code left 2FA on")
	}
}

// The code path is login's limiter, counted per user: after six wrong codes
// even the RIGHT one is refused, and 2FA stays on.
func TestTOTPDisable_TASK2190_AttemptsAreLimited(t *testing.T) {
	srv := testServer(t)
	if srv.rateLimiters == nil || srv.rateLimiters.RecoveryCode == nil {
		t.Skip("rate limiting disabled in this environment (PAD_DISABLE_RATE_LIMITS)")
	}
	// Same burst, no refill inside the test, so "the seventh try" does not
	// depend on how fast six requests run.
	burst := srv.rateLimiters.RecoveryCode.config.Burst
	srv.rateLimiters.RecoveryCode.Stop()
	srv.rateLimiters.RecoveryCode = newIPRateLimiter(rateLimitConfig{Rate: rate.Limit(1.0 / 3600.0), Burst: burst})
	t.Cleanup(srv.rateLimiters.RecoveryCode.Stop)

	userID, secret, tok := passwordlessTOTPUser(t, srv, "nopw-limit-2190@example.com")
	for i := 0; i < burst; i++ {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/2fa/disable", map[string]any{"code": "000000"}, tok)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("wrong code %d: %d %s, want 403", i+1, rr.Code, rr.Body.String())
		}
	}
	over := doRequestWithCookie(srv, "POST", "/api/v1/auth/2fa/disable", map[string]any{"code": validTOTPCode(t, secret)}, tok)
	if over.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d with a valid code: %d %s, want 429", burst+1, over.Code, over.Body.String())
	}
	if !totpEnabled(t, srv, userID) {
		t.Fatal("2FA was disabled past the attempt limit")
	}
}

// An account WITH a password still needs it: a code is not a way around it.
func TestTOTPDisable_TASK2190_PasswordAccountStillNeedsThePassword(t *testing.T) {
	srv := testServer(t)
	user, tok := loginTestUserAs(t, srv, "pw-2190@example.com", "Has Password", bug3322Password)
	secret := enableTOTPForDeleteUser(t, srv, user.ID)
	rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/2fa/disable", map[string]any{"code": validTOTPCode(t, secret)}, tok)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "Password is required") {
		t.Fatalf("code without password: %d %s, want 400 password required", rr.Code, rr.Body.String())
	}
	if !totpEnabled(t, srv, user.ID) {
		t.Fatal("a code alone disabled 2FA on an account with a password")
	}
}
