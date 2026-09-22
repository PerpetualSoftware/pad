package store

import (
	"database/sql"
	"errors"
	"strings"
)

// GetPlatformSetting returns a single platform setting value, or empty string if not set.
func (s *Store) GetPlatformSetting(key string) (string, error) {
	return s.GetPlatformSettingQ(s.db, key)
}

// GetPlatformSettingQ is GetPlatformSetting parameterized over its executor
// (see Queryer).
func (s *Store) GetPlatformSettingQ(q Queryer, key string) (string, error) {
	var value string
	err := q.QueryRow(s.q("SELECT value FROM platform_settings WHERE key = ?"), key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// SetPlatformSetting upserts a platform setting.
func (s *Store) SetPlatformSetting(key, value string) error {
	_, err := s.db.Exec(s.q(`
		INSERT INTO platform_settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`), key, value, now())
	return err
}

// GetPlatformSettings returns all platform settings as a map.
func (s *Store) GetPlatformSettings() (map[string]string, error) {
	rows, err := s.db.Query(s.q("SELECT key, value FROM platform_settings ORDER BY key"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	settings := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		settings[k] = v
	}
	return settings, rows.Err()
}

// DeletePlatformSetting removes a platform setting.
func (s *Store) DeletePlatformSetting(key string) error {
	_, err := s.db.Exec(s.q("DELETE FROM platform_settings WHERE key = ?"), key)
	return err
}

// ErrEncryptionUnavailable reports a secret write refused because the store
// has no encryption key. See SetSecretPlatformSetting.
var ErrEncryptionUnavailable = errors.New("store: no encryption key configured; refusing to store a secret in plaintext")

// SetSecretPlatformSetting stores a secret platform setting encrypted with
// the store's key (the PAD_ENCRYPTION_KEY path). An empty value deletes the
// row, so "no secret" has exactly one stored form.
//
// Unlike encrypt()'s other callers, which fall back to plaintext without a
// key, this REFUSES with ErrEncryptionUnavailable: a secret setting is
// documented as stored encrypted, and a plaintext fallback would make that
// false. `pad server` never reaches the refusal — it will not boot without a
// 32-byte key (cmd/pad/cmd_server.go, TASK-668) — so this is a defence for a
// store built without one, not a user-facing state (TASK-3121).
func (s *Store) SetSecretPlatformSetting(key, value string) error {
	if value == "" {
		return s.DeletePlatformSetting(key)
	}
	if !s.HasEncryptionKey() {
		return ErrEncryptionUnavailable
	}
	enc, err := s.encrypt(value)
	if err != nil {
		return err
	}
	return s.SetPlatformSetting(key, enc)
}

// ErrSecretNotEncrypted reports a secret setting row that is not ciphertext.
var ErrSecretNotEncrypted = errors.New("store: secret platform setting is not encrypted")

// GetSecretPlatformSetting returns a secret platform setting decrypted, or ""
// when unset.
//
// A row WITHOUT the encrypted prefix is refused rather than returned: the
// generic decrypt() passes plaintext through for callers that predate
// encryption, but a secret setting has only ever been written encrypted, so a
// plaintext row came from somewhere other than SetSecretPlatformSetting and
// must not become a live credential (codex round 2).
func (s *Store) GetSecretPlatformSetting(key string) (string, error) {
	v, err := s.GetPlatformSetting(key)
	if err != nil || v == "" {
		return "", err
	}
	if !strings.HasPrefix(v, encryptedPrefix) {
		return "", ErrSecretNotEncrypted
	}
	return s.decrypt(v)
}
