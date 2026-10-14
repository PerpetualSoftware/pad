package store

import (
	"crypto/rand"
	"testing"
)

// BUG-3805: the startup at-rest encryption passes read every plaintext secret,
// then write the ciphertext of the value they READ. A write landing between the
// two (another replica rotating the secret, a user re-enrolling or disabling
// 2FA) was overwritten with the stale value. These tests run that write inside
// the window via the after-read hooks. They set package-level hooks, so they
// must not be parallel.

func setTestEncryptionKey(t *testing.T, s *Store) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	s.SetEncryptionKey(key)
}

func rawWebhookSecret(t *testing.T, s *Store, id string) string {
	t.Helper()
	var raw string
	if err := s.db.QueryRow(s.q("SELECT secret FROM webhooks WHERE id = ?"), id).Scan(&raw); err != nil {
		t.Fatalf("read webhook secret: %v", err)
	}
	return raw
}

func TestEncryptWebhookSecretsAtRest_RotationDuringPassSurvives(t *testing.T) {
	for _, tc := range []struct {
		name      string
		steadySet bool // run once beforehand so the migration flag is set
	}{
		{name: "first run"},
		{name: "steady state", steadySet: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			wsID := newWebhookTestWorkspace(t, s)
			setTestEncryptionKey(t, s)
			if tc.steadySet {
				if _, err := s.EncryptWebhookSecretsAtRest(); err != nil {
					t.Fatalf("set migration flag: %v", err)
				}
			}

			// A legacy plaintext row, as a keyless boot would have written it.
			id := insertRawWebhook(t, s, wsID, "secret-before-rotation")

			const rotated = "secret-after-rotation"
			fired := false
			encryptWebhookSecretsAfterReadHook = func() {
				encryptWebhookSecretsAfterReadHook = nil
				fired = true
				enc, err := s.encrypt(rotated)
				if err != nil {
					t.Errorf("encrypt rotated secret: %v", err)
					return
				}
				if _, err := s.db.Exec(s.q(`UPDATE webhooks SET secret = ? WHERE id = ?`), enc, id); err != nil {
					t.Errorf("rotate secret: %v", err)
				}
			}
			t.Cleanup(func() { encryptWebhookSecretsAfterReadHook = nil })

			n, err := s.EncryptWebhookSecretsAtRest()
			if err != nil {
				t.Fatalf("migrate: %v", err)
			}
			if !fired {
				t.Fatal("after-read hook never fired; the test did not exercise the window")
			}
			if n != 0 {
				t.Errorf("a lost race must count as skipped, not encrypted: n=%d", n)
			}
			wh, err := s.GetWebhook(id)
			if err != nil {
				t.Fatalf("get webhook: %v", err)
			}
			if wh.Secret != rotated {
				t.Errorf("secret = %q, want the rotated %q (the pass overwrote a concurrent rotation)", wh.Secret, rotated)
			}
			if got, _ := s.GetPlatformSetting(webhookSecretsEncryptedFlag); got != "1" {
				t.Errorf("migration flag = %q, want 1 even when every row lost its race", got)
			}
		})
	}
}

// The untouched rows in the same pass are still encrypted: the compare-and-set
// skips only the row that moved.
func TestEncryptWebhookSecretsAtRest_OnlyTheMovedRowIsSkipped(t *testing.T) {
	s := testStore(t)
	wsID := newWebhookTestWorkspace(t, s)
	setTestEncryptionKey(t, s)

	moved := insertRawWebhook(t, s, wsID, "moved-before")
	still := insertRawWebhook(t, s, wsID, "still-plaintext")

	encryptWebhookSecretsAfterReadHook = func() {
		encryptWebhookSecretsAfterReadHook = nil
		if _, err := s.db.Exec(s.q(`DELETE FROM webhooks WHERE id = ?`), moved); err != nil {
			t.Errorf("delete: %v", err)
		}
	}
	t.Cleanup(func() { encryptWebhookSecretsAfterReadHook = nil })

	n, err := s.EncryptWebhookSecretsAtRest()
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1 (the deleted row is skipped, the other encrypted)", n)
	}
	if raw := rawWebhookSecret(t, s, still); raw == "still-plaintext" {
		t.Error("the unmoved row was left plaintext")
	}
	wh, err := s.GetWebhook(still)
	if err != nil {
		t.Fatalf("get webhook: %v", err)
	}
	if wh.Secret != "still-plaintext" {
		t.Errorf("unmoved row decrypts to %q", wh.Secret)
	}
}

func TestBackfillEncryptTOTPSecrets_WriteDuringPassSurvives(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(s *Store, userID string) error
		want  string // the plaintext the row must hold afterwards; "" = cleared
	}{
		{
			name:  "re-enroll",
			write: func(s *Store, userID string) error { return s.SetTOTPSecret(userID, "NEWSECRETBASE32") },
			want:  "NEWSECRETBASE32",
		},
		{
			name:  "disable",
			write: func(s *Store, userID string) error { return s.DisableTOTP(userID) },
			want:  "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			u := createTestUser(t, s, "totp-race@test.com", "TOTP Race", "password123")
			// Keyless: stored plaintext, as a legacy row.
			if err := s.SetTOTPSecret(u.ID, "OLDSECRETBASE32"); err != nil {
				t.Fatalf("seed secret: %v", err)
			}
			setTestEncryptionKey(t, s)

			fired := false
			backfillTOTPAfterReadHook = func() {
				backfillTOTPAfterReadHook = nil
				fired = true
				if err := tc.write(s, u.ID); err != nil {
					t.Errorf("concurrent write: %v", err)
				}
			}
			t.Cleanup(func() { backfillTOTPAfterReadHook = nil })

			n, err := s.BackfillEncryptTOTPSecrets()
			if err != nil {
				t.Fatalf("backfill: %v", err)
			}
			if !fired {
				t.Fatal("after-read hook never fired; the test did not exercise the window")
			}
			if n != 0 {
				t.Errorf("a lost race must count as skipped, not encrypted: n=%d", n)
			}
			var raw string
			if err := s.db.QueryRow(s.q("SELECT totp_secret FROM users WHERE id = ?"), u.ID).Scan(&raw); err != nil {
				t.Fatalf("read secret: %v", err)
			}
			got, err := s.decrypt(raw)
			if err != nil {
				t.Fatalf("decrypt: %v", err)
			}
			if got != tc.want {
				t.Errorf("totp secret = %q, want %q (the pass overwrote a concurrent write)", got, tc.want)
			}
		})
	}
}
