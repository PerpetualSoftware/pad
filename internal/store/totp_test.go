package store

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func hashCode(code string) string {
	h := sha256.Sum256([]byte(code))
	return hex.EncodeToString(h[:])
}

func TestTOTPSetupAndEnable(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")

	// Initially TOTP is disabled
	user, _ := s.GetUser(u.ID)
	if user.TOTPEnabled {
		t.Error("TOTP should be disabled initially")
	}
	if user.TOTPSecret != "" {
		t.Error("TOTP secret should be empty initially")
	}

	// Set secret (setup phase)
	err := s.SetTOTPSecret(u.ID, "JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatalf("SetTOTPSecret error: %v", err)
	}

	user, _ = s.GetUser(u.ID)
	if user.TOTPSecret != "JBSWY3DPEHPK3PXP" {
		t.Errorf("expected secret to be stored, got %q", user.TOTPSecret)
	}
	if user.TOTPEnabled {
		t.Error("TOTP should still be disabled after setting secret")
	}

	// Enable with hashed recovery codes (matching the real flow)
	hashedCodes := hashCode("abcd1234") + "\n" + hashCode("efgh5678") + "\n" + hashCode("ijkl9012")
	err = s.EnableTOTP(u.ID, "JBSWY3DPEHPK3PXP", hashedCodes)
	if err != nil {
		t.Fatalf("EnableTOTP error: %v", err)
	}

	user, _ = s.GetUser(u.ID)
	if !user.TOTPEnabled {
		t.Error("TOTP should be enabled after EnableTOTP")
	}
	if user.RecoveryCodes != hashedCodes {
		t.Errorf("expected hashed recovery codes to be stored")
	}
}

func TestTOTPEnableSecretMismatch(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")

	s.SetTOTPSecret(u.ID, "REALKEY123")

	// Try to enable with wrong expected secret
	err := s.EnableTOTP(u.ID, "WRONGKEY", "codes")
	if err == nil {
		t.Error("expected error when secret doesn't match")
	}

	// Should still be disabled
	user, _ := s.GetUser(u.ID)
	if user.TOTPEnabled {
		t.Error("TOTP should not be enabled after secret mismatch")
	}
}

func TestTOTPDisable(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")

	// Enable 2FA
	s.SetTOTPSecret(u.ID, "JBSWY3DPEHPK3PXP")
	s.EnableTOTP(u.ID, "JBSWY3DPEHPK3PXP", "code1\ncode2")

	user, _ := s.GetUser(u.ID)
	if !user.TOTPEnabled {
		t.Fatal("TOTP should be enabled")
	}

	// Disable
	err := s.DisableTOTP(u.ID)
	if err != nil {
		t.Fatalf("DisableTOTP error: %v", err)
	}

	user, _ = s.GetUser(u.ID)
	if user.TOTPEnabled {
		t.Error("TOTP should be disabled")
	}
	if user.TOTPSecret != "" {
		t.Error("TOTP secret should be cleared")
	}
	if user.RecoveryCodes != "" {
		t.Error("recovery codes should be cleared")
	}
}

func TestConsumeRecoveryCode(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")

	// Store hashed codes (matching the real flow)
	hashedCodes := hashCode("aaaa1111") + "\n" + hashCode("bbbb2222") + "\n" + hashCode("cccc3333")
	s.SetTOTPSecret(u.ID, "JBSWY3DPEHPK3PXP")
	s.EnableTOTP(u.ID, "JBSWY3DPEHPK3PXP", hashedCodes)

	// Consume a valid code (provide plaintext, store has hash)
	consumed, err := s.ConsumeRecoveryCode(u.ID, "bbbb2222")
	if err != nil {
		t.Fatalf("ConsumeRecoveryCode error: %v", err)
	}
	if !consumed {
		t.Error("expected code to be consumed")
	}

	// Verify it was removed
	user, _ := s.GetUser(u.ID)
	remaining := strings.Split(user.RecoveryCodes, "\n")
	var nonEmpty int
	for _, c := range remaining {
		if strings.TrimSpace(c) != "" {
			nonEmpty++
		}
	}
	if nonEmpty != 2 {
		t.Errorf("expected 2 remaining codes, got %d", nonEmpty)
	}

	// Try to consume the same code again — should fail
	consumed, err = s.ConsumeRecoveryCode(u.ID, "bbbb2222")
	if err != nil {
		t.Fatalf("ConsumeRecoveryCode error: %v", err)
	}
	if consumed {
		t.Error("should not be able to consume the same code twice")
	}

	// Invalid code
	consumed, _ = s.ConsumeRecoveryCode(u.ID, "invalid")
	if consumed {
		t.Error("should not consume an invalid code")
	}
}

func TestUserFieldsSurviveTOTP(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")

	user, err := s.GetUser(u.ID)
	if err != nil {
		t.Fatalf("GetUser error: %v", err)
	}
	if user.Email != "test@test.com" {
		t.Errorf("expected email 'test@test.com', got %q", user.Email)
	}

	user, err = s.GetUserByEmail("test@test.com")
	if err != nil {
		t.Fatalf("GetUserByEmail error: %v", err)
	}
	if user == nil {
		t.Fatal("expected user from GetUserByEmail")
	}

	valid, err := s.ValidatePassword("test@test.com", "password123")
	if err != nil {
		t.Fatalf("ValidatePassword error: %v", err)
	}
	if valid == nil {
		t.Error("expected valid password check")
	}
}
