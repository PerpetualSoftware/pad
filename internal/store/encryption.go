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
