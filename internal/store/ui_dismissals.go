package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// ErrUnknownUIDismissal is returned for a key outside models.UIDismissalKeys.
var ErrUnknownUIDismissal = fmt.Errorf("unknown UI dismissal key")

// decodeUIDismissals reads the stored set. Keys no longer in the allow-list
// (a suggestion that was retired) are dropped, so they never reach a client.
func decodeUIDismissals(raw string) []string {
	var keys []string
	if raw == "" || json.Unmarshal([]byte(raw), &keys) != nil {
		return []string{}
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if models.IsUIDismissalKey(k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ListUIDismissals returns the keys userID has dismissed, sorted. An unknown
// user answers sql.ErrNoRows.
func (s *Store) ListUIDismissals(userID string) ([]string, error) {
	var raw string
	if err := s.db.QueryRow(s.q(`SELECT ui_dismissals FROM users WHERE id = ?`), userID).Scan(&raw); err != nil {
		return nil, err
	}
	return decodeUIDismissals(raw), nil
}

// DismissUI adds key to userID's dismissed set (idempotent) and returns the
// set as this write left it. The read-modify-write holds the users row, the
// same lock a workspace-tab write takes, so two dismissals from two devices
// cannot drop one another.
func (s *Store) DismissUI(userID, key string) ([]string, error) {
	if !models.IsUIDismissalKey(key) {
		return nil, ErrUnknownUIDismissal
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("dismiss ui: begin: %w", err)
	}
	defer tx.Rollback()
	sel := `SELECT ui_dismissals FROM users WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
		sel += ` FOR NO KEY UPDATE`
	}
	var raw string
	if err := tx.QueryRow(s.q(sel), userID).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil, err
		}
		return nil, fmt.Errorf("dismiss ui: read: %w", err)
	}
	keys := decodeUIDismissals(raw)
	for _, k := range keys {
		if k == key {
			return keys, tx.Commit()
		}
	}
	keys = append(keys, key)
	sort.Strings(keys)
	enc, _ := json.Marshal(keys)
	if _, err := tx.Exec(s.q(`UPDATE users SET ui_dismissals = ? WHERE id = ?`), string(enc), userID); err != nil {
		return nil, fmt.Errorf("dismiss ui: write: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("dismiss ui: commit: %w", err)
	}
	return keys, nil
}
