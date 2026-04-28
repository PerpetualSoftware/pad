package store

import (
	"crypto/rand"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestEncryptDecrypt(t *testing.T) {
	s := testStore(t)

	// Generate a random 32-byte key
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	s.SetEncryptionKey(key)

	// Test basic encrypt/decrypt
	original := "JBSWY3DPEHPK3PXP"
	encrypted, err := s.encrypt(original)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if encrypted == original {
		t.Error("encrypted value should differ from original")
	}
	if encrypted[:4] != "enc:" {
		t.Errorf("encrypted value should start with 'enc:', got %q", encrypted[:4])
	}

	decrypted, err := s.decrypt(encrypted)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if decrypted != original {
		t.Errorf("expected %q, got %q", original, decrypted)
	}
}

func TestDecryptPlaintext(t *testing.T) {
	s := testStore(t)

	key := make([]byte, 32)
	rand.Read(key)
	s.SetEncryptionKey(key)

	// Plaintext values (no "enc:" prefix) should pass through unchanged
	plain := "JBSWY3DPEHPK3PXP"
	result, err := s.decrypt(plain)
	if err != nil {
		t.Fatalf("decrypt plaintext: %v", err)
	}
	if result != plain {
		t.Errorf("expected %q, got %q", plain, result)
	}
}

func TestEncryptWithoutKey(t *testing.T) {
	s := testStore(t)

	// No encryption key — encrypt should return plaintext
	original := "JBSWY3DPEHPK3PXP"
	result, err := s.encrypt(original)
	if err != nil {
		t.Fatalf("encrypt without key: %v", err)
	}
	if result != original {
		t.Errorf("without key, encrypt should return plaintext, got %q", result)
	}
}

func TestEncryptDecryptEmpty(t *testing.T) {
	s := testStore(t)

	key := make([]byte, 32)
	rand.Read(key)
	s.SetEncryptionKey(key)

	// Empty strings should pass through
	encrypted, err := s.encrypt("")
	if err != nil {
		t.Fatalf("encrypt empty: %v", err)
	}
	if encrypted != "" {
		t.Errorf("expected empty, got %q", encrypted)
	}

	decrypted, err := s.decrypt("")
	if err != nil {
		t.Fatalf("decrypt empty: %v", err)
	}
	if decrypted != "" {
		t.Errorf("expected empty, got %q", decrypted)
	}
}

func TestTOTPEncryptionRoundTrip(t *testing.T) {
	s := testStore(t)

	key := make([]byte, 32)
	rand.Read(key)
	s.SetEncryptionKey(key)

	// Create a user
	u, err := s.CreateUser(models.UserCreate{
		Email:    "totp@test.com",
		Name:     "TOTP Tester",
		Password: "password123",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Set TOTP secret (should be encrypted in DB)
	secret := "JBSWY3DPEHPK3PXP"
	if err := s.SetTOTPSecret(u.ID, secret); err != nil {
		t.Fatalf("set totp secret: %v", err)
	}

	// Read it back — should be decrypted automatically
	fetched, err := s.GetUser(u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if fetched.TOTPSecret != secret {
		t.Errorf("expected %q, got %q", secret, fetched.TOTPSecret)
	}

	// Verify the raw value in DB is encrypted
	var rawSecret string
	s.db.QueryRow(s.q("SELECT totp_secret FROM users WHERE id = ?"), u.ID).Scan(&rawSecret)
	if rawSecret == secret {
		t.Error("raw DB value should be encrypted, not plaintext")
	}
	if rawSecret[:4] != "enc:" {
		t.Errorf("raw DB value should start with 'enc:', got %q", rawSecret[:10])
	}
}
