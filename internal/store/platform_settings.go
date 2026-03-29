package store

import "database/sql"

// GetPlatformSetting returns a single platform setting value, or empty string if not set.
func (s *Store) GetPlatformSetting(key string) (string, error) {
	var value string
	err := s.db.QueryRow("SELECT value FROM platform_settings WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// SetPlatformSetting upserts a platform setting.
func (s *Store) SetPlatformSetting(key, value string) error {
	_, err := s.db.Exec(`
		INSERT INTO platform_settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, key, value, now())
	return err
}

// GetPlatformSettings returns all platform settings as a map.
func (s *Store) GetPlatformSettings() (map[string]string, error) {
	rows, err := s.db.Query("SELECT key, value FROM platform_settings ORDER BY key")
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
	_, err := s.db.Exec("DELETE FROM platform_settings WHERE key = ?", key)
	return err
}
