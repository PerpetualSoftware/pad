package store

import "testing"

// storedTOTPSecret reads a user's totp_secret column and decrypts it, whatever
// the user's 2FA state. The User struct no longer carries the secret (BUG-4019).
func storedTOTPSecret(t *testing.T, s *Store, userID string) string {
	t.Helper()
	var raw string
	if err := s.db.QueryRow(s.q(`SELECT totp_secret FROM users WHERE id = ?`), userID).Scan(&raw); err != nil {
		t.Fatalf("read totp_secret: %v", err)
	}
	got, err := s.decrypt(raw)
	if err != nil {
		t.Fatalf("decrypt totp_secret: %v", err)
	}
	return got
}
