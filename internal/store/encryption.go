package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

const encryptedPrefix = "enc:"

// SetEncryptionKey configures the store's AES-256 encryption key for
// encrypting sensitive fields (e.g., TOTP secrets) at rest.
// The key must be exactly 32 bytes (256 bits). If empty, encryption is disabled
// and secrets are stored in plaintext (with a warning logged at startup).
func (s *Store) SetEncryptionKey(key []byte) {
	s.encryptionKey = key
}

// HasEncryptionKey reports whether an encryption key is configured.
func (s *Store) HasEncryptionKey() bool {
	return len(s.encryptionKey) == 32
}

// encrypt encrypts plaintext using AES-256-GCM and returns a base64-encoded
// ciphertext prefixed with "enc:" to distinguish from plaintext values.
func (s *Store) encrypt(plaintext string) (string, error) {
	if !s.HasEncryptionKey() {
		return plaintext, nil // No key — store as plaintext
	}
	if plaintext == "" {
		return "", nil
	}

	block, err := aes.NewCipher(s.encryptionKey)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return encryptedPrefix + base64.StdEncoding.EncodeToString(ciphertext), nil
}

// decrypt decrypts a value that was encrypted with encrypt().
// If the value doesn't have the "enc:" prefix, it's treated as plaintext
// (backward compatibility with pre-encryption data).
func (s *Store) decrypt(value string) (string, error) {
	if value == "" {
		return "", nil
	}

	// Not encrypted — return as-is (plaintext from before encryption was enabled)
	if !strings.HasPrefix(value, encryptedPrefix) {
		return value, nil
	}

	if !s.HasEncryptionKey() {
		return "", fmt.Errorf("encrypted value found but no encryption key configured")
	}

	encoded := strings.TrimPrefix(value, encryptedPrefix)
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}

	block, err := aes.NewCipher(s.encryptionKey)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}

	return string(plaintext), nil
}

// BackfillEncryptTOTPSecrets encrypts any plaintext TOTP secrets in the database.
// Idempotent: skips secrets that already have the "enc:" prefix.
// Called on startup when an encryption key is first configured.
func (s *Store) BackfillEncryptTOTPSecrets() (int, error) {
	if !s.HasEncryptionKey() {
		return 0, nil
	}

	rows, err := s.db.Query(s.q(`SELECT id, totp_secret FROM users WHERE totp_secret != '' AND totp_secret NOT LIKE 'enc:%'`))
	if err != nil {
		return 0, fmt.Errorf("query plaintext secrets: %w", err)
	}
	defer rows.Close()

	type row struct {
		id, secret string
	}
	var toEncrypt []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.secret); err != nil {
			return 0, fmt.Errorf("scan row: %w", err)
		}
		toEncrypt = append(toEncrypt, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, r := range toEncrypt {
		encrypted, err := s.encrypt(r.secret)
		if err != nil {
			return 0, fmt.Errorf("encrypt secret for user %s: %w", r.id, err)
		}
		if _, err := s.db.Exec(s.q(`UPDATE users SET totp_secret = ? WHERE id = ?`), encrypted, r.id); err != nil {
			return 0, fmt.Errorf("update secret for user %s: %w", r.id, err)
		}
	}

	return len(toEncrypt), nil
}

// webhookSecretsEncryptedFlag marks that the one-time legacy migration of
// pre-encryption webhook secrets has run. Stored in platform_settings.
const webhookSecretsEncryptedFlag = "webhook_secrets_encrypted_v1"

// EncryptWebhookSecretsAtRest encrypts plaintext webhook HMAC secrets (BUG-2057).
// Called on startup when an encryption key is configured. It handles two
// populations without ever corrupting genuine ciphertext:
//
//   - First run (flag unset): webhook-secret encryption is new in this release,
//     so every existing secret in the DB is plaintext — even one that
//     coincidentally starts with the reserved "enc:" marker. Encrypt them ALL,
//     then persist the flag. Because this runs exactly once, ciphertext written
//     by later encrypted creates is never re-wrapped under a rotated key.
//   - Steady state (flag set): only unprefixed plaintext is encrypted (a
//     webhook created while the instance was keyless). "enc:" values are always
//     genuine ciphertext now — handleCreateWebhook rejects "enc:"-prefixed
//     secrets — so they are left alone and a bad key fails loud via decrypt().
//
// Idempotent and safe to run on every boot.
func (s *Store) EncryptWebhookSecretsAtRest() (int, error) {
	if !s.HasEncryptionKey() {
		// Keyless: nothing to encrypt, and the flag stays unset so the full
		// legacy migration still runs if a key is configured later.
		return 0, nil
	}

	migrated, err := s.GetPlatformSetting(webhookSecretsEncryptedFlag)
	if err != nil {
		return 0, fmt.Errorf("read webhook-secret migration flag: %w", err)
	}
	firstRun := migrated != "1"

	query := `SELECT id, secret FROM webhooks WHERE secret != '' AND secret NOT LIKE 'enc:%'`
	if firstRun {
		// Every pre-migration secret is plaintext — include "enc:"-prefixed ones.
		query = `SELECT id, secret FROM webhooks WHERE secret != ''`
	}

	rows, err := s.db.Query(s.q(query))
	if err != nil {
		return 0, fmt.Errorf("query webhook secrets: %w", err)
	}
	defer rows.Close()

	type row struct {
		id, secret string
	}
	var toEncrypt []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.secret); err != nil {
			return 0, fmt.Errorf("scan row: %w", err)
		}
		// First-run only: an "enc:" value that already decrypts under the
		// current key is genuine ciphertext — skip it so it isn't double-
		// encrypted. A decrypt failure means legacy plaintext that merely looks
		// prefixed (no ciphertext under a different key can exist before the
		// migration has ever run), so fall through and encrypt it.
		if firstRun && strings.HasPrefix(r.secret, encryptedPrefix) {
			if _, derr := s.decrypt(r.secret); derr == nil {
				continue
			}
		}
		toEncrypt = append(toEncrypt, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close() // release the read before opening the write transaction (SQLite)

	if len(toEncrypt) == 0 && !firstRun {
		return 0, nil
	}

	// Apply every row update AND the completion flag in one transaction. A crash
	// mid-migration must not leave some rows encrypted while the flag is unset —
	// otherwise a later key change would mis-read those rows as legacy plaintext
	// and double-encrypt them into nested ciphertext (BUG-2057 review follow-up).
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin webhook-secret migration: %w", err)
	}
	defer tx.Rollback()

	for _, r := range toEncrypt {
		encrypted, err := s.encrypt(r.secret)
		if err != nil {
			return 0, fmt.Errorf("encrypt secret for webhook %s: %w", r.id, err)
		}
		if _, err := tx.Exec(s.q(`UPDATE webhooks SET secret = ? WHERE id = ?`), encrypted, r.id); err != nil {
			return 0, fmt.Errorf("update secret for webhook %s: %w", r.id, err)
		}
	}

	if firstRun {
		if _, err := tx.Exec(s.q(`
			INSERT INTO platform_settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
		`), webhookSecretsEncryptedFlag, "1", now()); err != nil {
			return 0, fmt.Errorf("persist webhook-secret migration flag: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit webhook-secret migration: %w", err)
	}

	return len(toEncrypt), nil
}
