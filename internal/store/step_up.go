package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/big"
	"time"
)

// ErrStepUpSendLimit reports the remaining wait before another email code
// can be sent. Rows survive logout so the limit belongs to the account.
type ErrStepUpSendLimit struct{ RetryAfter time.Duration }

func (e *ErrStepUpSendLimit) Error() string { return "step-up email send limit reached" }

func stepUpCodeHash(id, code string) string {
	hash := sha256.Sum256([]byte(id + ":" + code))
	return hex.EncodeToString(hash[:])
}

// CreateStepUpCode replaces unused codes across the account and enforces
// three sends per rolling hour under the account lock. The code is returned
// once for delivery; the id lets the caller remove a failed delivery.
func (s *Store) CreateStepUpCode(userID, sessionID string) (id, code string, expiresAt time.Time, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("create step-up code: begin: %w", err)
	}
	defer tx.Rollback()
	if err := s.lockUserRowTx(tx, userID); err != nil {
		return "", "", time.Time{}, fmt.Errorf("create step-up code: lock user: %w", err)
	}
	current := time.Now().UTC()
	var count int
	var oldest *string
	if err := tx.QueryRow(s.q(`SELECT COUNT(*), MIN(created_at) FROM step_up_codes WHERE user_id=? AND created_at>?`), userID, current.Add(-time.Hour).Format(time.RFC3339)).Scan(&count, &oldest); err != nil {
		return "", "", time.Time{}, fmt.Errorf("count step-up codes: %w", err)
	}
	if count >= 3 {
		return "", "", time.Time{}, &ErrStepUpSendLimit{RetryAfter: parseTime(*oldest).Add(time.Hour).Sub(current)}
	}
	value, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("generate step-up code: %w", err)
	}
	id = newID()
	code = fmt.Sprintf("%06d", value.Int64())
	expiresAt = current.Add(10 * time.Minute)
	ts := current.Format(time.RFC3339)
	if _, err := tx.Exec(s.q(`UPDATE step_up_codes SET used_at=? WHERE user_id=? AND used_at IS NULL`), ts, userID); err != nil {
		return "", "", time.Time{}, fmt.Errorf("replace step-up codes: %w", err)
	}
	if _, err := tx.Exec(s.q(`INSERT INTO step_up_codes (id,user_id,session_id,code_hash,expires_at,created_at) VALUES (?,?,?,?,?,?)`), id, userID, sessionID, stepUpCodeHash(id, code), expiresAt.Format(time.RFC3339), ts); err != nil {
		return "", "", time.Time{}, fmt.Errorf("insert step-up code: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", "", time.Time{}, fmt.Errorf("create step-up code: commit: %w", err)
	}
	return id, code, expiresAt, nil
}

// RetireStepUpCode makes a code whose email delivery failed unusable. The row
// stays, so the failed send still counts toward the hourly send cap.
func (s *Store) RetireStepUpCode(id string) error {
	_, err := s.db.Exec(s.q(`UPDATE step_up_codes SET used_at=? WHERE id=? AND used_at IS NULL`), now(), id)
	return err
}

// VerifyStepUpCode charges an attempt before comparing the hash. The final
// conditional consume makes one code successful at most once, even when
// multiple correct attempts race.
func (s *Store) VerifyStepUpCode(userID, sessionID, code string) (bool, error) {
	var id, hash string
	err := s.db.QueryRow(s.q(`SELECT id,code_hash FROM step_up_codes WHERE user_id=? AND session_id=? AND used_at IS NULL AND expires_at>? ORDER BY created_at DESC LIMIT 1`), userID, sessionID, now()).Scan(&id, &hash)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find step-up code: %w", err)
	}
	result, err := s.db.Exec(s.q(`UPDATE step_up_codes SET attempts=attempts+1 WHERE id=? AND used_at IS NULL AND attempts<5 AND expires_at>?`), id, now())
	if err != nil {
		return false, fmt.Errorf("charge step-up attempt: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(stepUpCodeHash(id, code))) != 1 {
		return false, nil
	}
	result, err = s.db.Exec(s.q(`UPDATE step_up_codes SET used_at=? WHERE id=? AND used_at IS NULL AND expires_at>?`), now(), id, now())
	if err != nil {
		return false, fmt.Errorf("consume step-up code: %w", err)
	}
	n, err = result.RowsAffected()
	return n == 1, err
}

// GrantSessionStepUp keeps an existing live grant at its original deadline.
// A replacement session starts with no grant, even after credential rotation.
func (s *Store) GrantSessionStepUp(sessionID string, until time.Time) error {
	result, err := s.db.Exec(s.q(`UPDATE sessions SET step_up_until=CASE WHEN step_up_until>? THEN step_up_until ELSE ? END WHERE id=?`), now(), until.UTC().Format(time.RFC3339), sessionID)
	if err != nil {
		return fmt.Errorf("grant session step-up: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CleanExpiredStepUpCodes preserves the full send-limit window, including
// used codes, before removing old rows.
func (s *Store) CleanExpiredStepUpCodes() error {
	_, err := s.db.Exec(s.q(`DELETE FROM step_up_codes WHERE created_at<?`), time.Now().UTC().Add(-2*time.Hour).Format(time.RFC3339))
	return err
}
