package store

import (
	"strings"
	"testing"
)

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

	// Enable with recovery codes
	codes := "abcd1234\nefgh5678\nijkl9012"
	err = s.EnableTOTP(u.ID, codes)
	if err != nil {
		t.Fatalf("EnableTOTP error: %v", err)
	}

	user, _ = s.GetUser(u.ID)
	if !user.TOTPEnabled {
		t.Error("TOTP should be enabled after EnableTOTP")
	}
	if user.RecoveryCodes != codes {
		t.Errorf("expected recovery codes to be stored")
	}
}

func TestTOTPDisable(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")

	// Enable 2FA
	s.SetTOTPSecret(u.ID, "JBSWY3DPEHPK3PXP")
	s.EnableTOTP(u.ID, "code1\ncode2")

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

	codes := "aaaa1111\nbbbb2222\ncccc3333"
	s.SetTOTPSecret(u.ID, "JBSWY3DPEHPK3PXP")
	s.EnableTOTP(u.ID, codes)

	// Consume a valid code
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
	if len(remaining) != 2 {
		t.Errorf("expected 2 remaining codes, got %d: %v", len(remaining), remaining)
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

	// Verify basic user fields still work after migration
	user, err := s.GetUser(u.ID)
	if err != nil {
		t.Fatalf("GetUser error: %v", err)
	}
	if user.Email != "test@test.com" {
		t.Errorf("expected email 'test@test.com', got %q", user.Email)
	}
	if user.Name != "Test" {
		t.Errorf("expected name 'Test', got %q", user.Name)
	}

	// GetUserByEmail should also work
	user, err = s.GetUserByEmail("test@test.com")
	if err != nil {
		t.Fatalf("GetUserByEmail error: %v", err)
	}
	if user == nil {
		t.Fatal("expected user from GetUserByEmail")
	}
	if user.ID != u.ID {
		t.Errorf("expected same user ID")
	}

	// ValidatePassword should still work
	valid, err := s.ValidatePassword("test@test.com", "password123")
	if err != nil {
		t.Fatalf("ValidatePassword error: %v", err)
	}
	if valid == nil {
		t.Error("expected valid password check")
	}
}
