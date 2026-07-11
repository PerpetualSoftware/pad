package store

import (
	"sync"
	"testing"
)

const totpStepTestSecret = "JBSWY3DPEHPK3PXP" // canonical base32 test secret

// TestConsumeTOTPStep_SingleUse exercises the compare-and-set that enforces
// single-use TOTP login codes (BUG-2054): a step is claimed once, replaying
// the same (or an older) step is rejected, and a fresh higher step succeeds
// and advances the stored value.
func TestConsumeTOTPStep_SingleUse(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "totp@example.com", "TOTP User", "password123")
	if err := s.SetTOTPSecret(u.ID, totpStepTestSecret); err != nil {
		t.Fatalf("SetTOTPSecret: %v", err)
	}

	// First claim of a step wins (stored value starts NULL).
	ok, err := s.ConsumeTOTPStep(u.ID, totpStepTestSecret, 1000)
	if err != nil {
		t.Fatalf("ConsumeTOTPStep(1000): %v", err)
	}
	if !ok {
		t.Fatal("expected first claim of step 1000 to succeed")
	}

	// Replaying the same step is rejected (this is the replay guard).
	ok, err = s.ConsumeTOTPStep(u.ID, totpStepTestSecret, 1000)
	if err != nil {
		t.Fatalf("ConsumeTOTPStep(1000) replay: %v", err)
	}
	if ok {
		t.Fatal("expected replay of step 1000 to be rejected")
	}

	// An older step (e.g. a captured earlier code) is also rejected.
	ok, err = s.ConsumeTOTPStep(u.ID, totpStepTestSecret, 999)
	if err != nil {
		t.Fatalf("ConsumeTOTPStep(999): %v", err)
	}
	if ok {
		t.Fatal("expected older step 999 to be rejected")
	}

	// A fresh, strictly-greater step succeeds and advances the watermark.
	ok, err = s.ConsumeTOTPStep(u.ID, totpStepTestSecret, 1001)
	if err != nil {
		t.Fatalf("ConsumeTOTPStep(1001): %v", err)
	}
	if !ok {
		t.Fatal("expected fresh step 1001 to succeed")
	}

	var stored int64
	if err := s.db.QueryRow(s.q(`SELECT totp_last_step FROM users WHERE id = ?`), u.ID).Scan(&stored); err != nil {
		t.Fatalf("read totp_last_step: %v", err)
	}
	if stored != 1001 {
		t.Fatalf("expected stored totp_last_step=1001, got %d", stored)
	}
}

// TestConsumeTOTPStep_SecretMismatch asserts the secret-identity guard: a code
// validated against a secret that is no longer the stored one (a 2FA
// disable/re-enroll racing an in-flight login) does NOT advance the watermark,
// so it can't spuriously lock out the freshly-enrolled authenticator.
func TestConsumeTOTPStep_SecretMismatch(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "mismatch@example.com", "Mismatch User", "password123")
	if err := s.SetTOTPSecret(u.ID, "SECRETAAAAAAAAAA"); err != nil {
		t.Fatalf("SetTOTPSecret: %v", err)
	}

	// Consuming with a stale/wrong secret is refused...
	ok, err := s.ConsumeTOTPStep(u.ID, "SECRETBBBBBBBBBB", 3000)
	if err != nil {
		t.Fatalf("ConsumeTOTPStep(wrong secret): %v", err)
	}
	if ok {
		t.Fatal("expected consumption with a mismatched secret to be rejected")
	}

	// ...and it left the watermark untouched, so the current secret can still
	// consume a LOWER step (proving no stale 3000 was written).
	ok, err = s.ConsumeTOTPStep(u.ID, "SECRETAAAAAAAAAA", 100)
	if err != nil {
		t.Fatalf("ConsumeTOTPStep(current secret): %v", err)
	}
	if !ok {
		t.Fatal("expected step 100 to succeed — the mismatched call must not have advanced the watermark")
	}
}

// TestTOTPStep_ResetOnSecretChange asserts the single-use watermark is cleared
// when the secret changes (BUG-2054 Codex follow-up): a time-step counter from
// an old secret must not reject a freshly-enrolled authenticator's codes.
func TestTOTPStep_ResetOnSecretChange(t *testing.T) {
	s := testStore(t)

	stepIsSet := func(userID string) bool {
		t.Helper()
		var step *int64
		if err := s.db.QueryRow(s.q(`SELECT totp_last_step FROM users WHERE id = ?`), userID).Scan(&step); err != nil {
			t.Fatalf("read totp_last_step: %v", err)
		}
		return step != nil
	}

	// DisableTOTP clears the watermark.
	u := createTestUser(t, s, "reset-disable@example.com", "Reset User", "password123")
	if err := s.SetTOTPSecret(u.ID, totpStepTestSecret); err != nil {
		t.Fatalf("SetTOTPSecret: %v", err)
	}
	if ok, err := s.ConsumeTOTPStep(u.ID, totpStepTestSecret, 5000); err != nil || !ok {
		t.Fatalf("seed step: ok=%v err=%v", ok, err)
	}
	if err := s.DisableTOTP(u.ID); err != nil {
		t.Fatalf("DisableTOTP: %v", err)
	}
	if stepIsSet(u.ID) {
		t.Fatal("expected totp_last_step cleared after DisableTOTP")
	}

	// SetTOTPSecret (re-enrollment) also clears the watermark, and a now-lower
	// step is accepted again with the fresh secret.
	u2 := createTestUser(t, s, "reset-setup@example.com", "Reset User 2", "password123")
	if err := s.SetTOTPSecret(u2.ID, "OLDSECRETAAAAAAA"); err != nil {
		t.Fatalf("SetTOTPSecret(old): %v", err)
	}
	if ok, err := s.ConsumeTOTPStep(u2.ID, "OLDSECRETAAAAAAA", 5000); err != nil || !ok {
		t.Fatalf("seed step: ok=%v err=%v", ok, err)
	}
	if err := s.SetTOTPSecret(u2.ID, "NEWSECRETBBBBBBB"); err != nil {
		t.Fatalf("SetTOTPSecret(new): %v", err)
	}
	if stepIsSet(u2.ID) {
		t.Fatal("expected totp_last_step cleared after SetTOTPSecret")
	}
	if ok, err := s.ConsumeTOTPStep(u2.ID, "NEWSECRETBBBBBBB", 4000); err != nil || !ok {
		t.Fatalf("expected step 4000 to succeed after secret reset: ok=%v err=%v", ok, err)
	}
}

// TestConsumeTOTPStep_ConcurrentCAS asserts that when many goroutines race to
// claim the SAME step, exactly one wins — the compare-and-set closes the
// concurrent-replay window.
func TestConsumeTOTPStep_ConcurrentCAS(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "race@example.com", "Race User", "password123")
	if err := s.SetTOTPSecret(u.ID, totpStepTestSecret); err != nil {
		t.Fatalf("SetTOTPSecret: %v", err)
	}

	const goroutines = 8
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  int
		start = make(chan struct{})
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := s.ConsumeTOTPStep(u.ID, totpStepTestSecret, 2000)
			if err != nil {
				t.Errorf("ConsumeTOTPStep: %v", err)
				return
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("expected exactly one goroutine to claim step 2000, got %d", wins)
	}
}
