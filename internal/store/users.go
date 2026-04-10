package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/xarmian/pad/internal/models"
	"golang.org/x/crypto/bcrypt"
)

const bcryptCost = 12

// user SELECT columns — used by all user queries.
const userColumns = `id, email, username, name, password_hash, role, avatar_url, totp_secret, totp_enabled, recovery_codes, created_at, updated_at`

// scanUser scans a user row into a User struct.
func scanUser(row interface{ Scan(...interface{}) error }) (*models.User, error) {
	var u models.User
	var createdAt, updatedAt string

	err := row.Scan(
		&u.ID, &u.Email, &u.Username, &u.Name, &u.PasswordHash, &u.Role, &u.AvatarURL,
		&u.TOTPSecret, &u.TOTPEnabled, &u.RecoveryCodes,
		&createdAt, &updatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	u.CreatedAt = parseTime(createdAt)
	u.UpdatedAt = parseTime(updatedAt)
	return &u, nil
}

// CreateUser creates a new user with a hashed password.
func (s *Store) CreateUser(input models.UserCreate) (*models.User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	role := input.Role
	if role == "" {
		role = "member"
	}

	id := newID()
	ts := now()

	_, err = s.db.Exec(s.q(`
		INSERT INTO users (id, email, username, name, password_hash, role, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`), id, strings.ToLower(strings.TrimSpace(input.Email)), strings.TrimSpace(input.Username), strings.TrimSpace(input.Name), string(hash), role, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}

	return s.GetUser(id)
}

// GetUser retrieves a user by ID.
func (s *Store) GetUser(id string) (*models.User, error) {
	u, err := scanUser(s.db.QueryRow(s.q(`SELECT `+userColumns+` FROM users WHERE id = ?`), id))
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

// GetUserByEmail retrieves a user by email address (case-insensitive).
func (s *Store) GetUserByEmail(email string) (*models.User, error) {
	u, err := scanUser(s.db.QueryRow(s.q(`SELECT `+userColumns+` FROM users WHERE email = ?`),
		strings.ToLower(strings.TrimSpace(email))))
	if err != nil {
		return nil, fmt.Errorf("get user by email: %w", err)
	}
	return u, nil
}

// GetUserByUsername retrieves a user by username (case-insensitive).
func (s *Store) GetUserByUsername(username string) (*models.User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return nil, nil
	}
	u, err := scanUser(s.db.QueryRow(s.q(`SELECT `+userColumns+` FROM users WHERE LOWER(username) = ?`), username))
	if err != nil {
		return nil, fmt.Errorf("get user by username: %w", err)
	}
	return u, nil
}

// UpdateUser updates mutable user fields.
func (s *Store) UpdateUser(id string, input models.UserUpdate) (*models.User, error) {
	var sets []string
	var args []interface{}

	if input.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, strings.TrimSpace(*input.Name))
	}
	if input.Username != nil {
		sets = append(sets, "username = ?")
		args = append(args, strings.TrimSpace(*input.Username))
	}
	if input.Password != nil {
		hash, err := bcrypt.GenerateFromPassword([]byte(*input.Password), bcryptCost)
		if err != nil {
			return nil, fmt.Errorf("hash password: %w", err)
		}
		sets = append(sets, "password_hash = ?")
		args = append(args, string(hash))
	}
	if input.AvatarURL != nil {
		sets = append(sets, "avatar_url = ?")
		args = append(args, *input.AvatarURL)
	}

	if len(sets) == 0 {
		return s.GetUser(id)
	}

	sets = append(sets, "updated_at = ?")
	args = append(args, now())
	args = append(args, id)

	query := fmt.Sprintf("UPDATE users SET %s WHERE id = ?", strings.Join(sets, ", "))
	result, err := s.db.Exec(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return nil, sql.ErrNoRows
	}

	return s.GetUser(id)
}

// ValidatePassword checks an email/password combination. Returns the user
// if valid, nil if the credentials are wrong (not an error).
func (s *Store) ValidatePassword(email, password string) (*models.User, error) {
	u, err := s.GetUserByEmail(email)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, nil
	}

	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return nil, nil // wrong password — not an error
	}

	return u, nil
}

// ListUsers returns all users.
func (s *Store) ListUsers() ([]models.User, error) {
	rows, err := s.db.Query(s.q(`SELECT ` + userColumns + ` FROM users ORDER BY created_at ASC`))
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var result []models.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		result = append(result, *u)
	}
	return result, rows.Err()
}

// UserCount returns the total number of registered users.
func (s *Store) UserCount() (int, error) {
	var count int
	err := s.db.QueryRow(s.q("SELECT COUNT(*) FROM users")).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return count, nil
}

// --- TOTP 2FA ---

// TODO: Encrypt TOTP secret at rest using an app-level encryption key
// (e.g., AES-256-GCM with a key from PAD_ENCRYPTION_KEY env var).
// The secret must remain decryptable for TOTP validation, so hashing
// is not an option here. For now, it is stored as plaintext.

// SetTOTPSecret stores the TOTP secret for a user (before 2FA is verified).
func (s *Store) SetTOTPSecret(userID, secret string) error {
	_, err := s.db.Exec(s.q(`UPDATE users SET totp_secret = ?, updated_at = ? WHERE id = ?`), secret, now(), userID)
	if err != nil {
		return fmt.Errorf("set totp secret: %w", err)
	}
	return nil
}

// EnableTOTP atomically enables 2FA for a user and stores hashed recovery codes.
// The expectedSecret must match the currently stored totp_secret to prevent
// TOCTOU races (e.g., a concurrent setup call overwriting the secret).
func (s *Store) EnableTOTP(userID, expectedSecret, hashedRecoveryCodes string) error {
	result, err := s.db.Exec(s.q(
		`UPDATE users SET totp_enabled = ?, recovery_codes = ?, updated_at = ?
		 WHERE id = ? AND totp_secret = ? AND totp_enabled = ?`),
		s.dialect.BoolToInt(true), hashedRecoveryCodes, now(), userID, expectedSecret, s.dialect.BoolToInt(false))
	if err != nil {
		return fmt.Errorf("enable totp: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("enable totp: secret mismatch or user not found")
	}
	return nil
}

// DisableTOTP disables 2FA and clears the secret and recovery codes.
func (s *Store) DisableTOTP(userID string) error {
	_, err := s.db.Exec(s.q(`UPDATE users SET totp_enabled = ?, totp_secret = '', recovery_codes = '', updated_at = ? WHERE id = ?`),
		s.dialect.BoolToInt(false), now(), userID)
	if err != nil {
		return fmt.Errorf("disable totp: %w", err)
	}
	return nil
}

// ConsumeRecoveryCode validates and removes a single recovery code.
// Recovery codes are stored as SHA-256 hashes. The provided plaintext
// code is hashed before comparison. Uses a transaction to prevent
// concurrent consumption of the same code.
func (s *Store) ConsumeRecoveryCode(userID, code string) (bool, error) {
	// Hash the input code for comparison against stored hashes
	inputHash := sha256.Sum256([]byte(code))
	inputHashStr := hex.EncodeToString(inputHash[:])

	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var recoveryCodes string
	err = tx.QueryRow(s.q(`SELECT recovery_codes FROM users WHERE id = ?`), userID).Scan(&recoveryCodes)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("select recovery codes: %w", err)
	}

	codes := strings.Split(recoveryCodes, "\n")
	var remaining []string
	found := false
	for _, c := range codes {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !found && c == inputHashStr {
			found = true
			continue // consume this one
		}
		remaining = append(remaining, c)
	}

	if !found {
		return false, nil
	}

	// Use optimistic locking: include the original recovery_codes in the WHERE
	// clause so a concurrent transaction that already consumed a code will cause
	// this UPDATE to match 0 rows, preventing double-spend.
	result, err := tx.Exec(s.q(`UPDATE users SET recovery_codes = ?, updated_at = ? WHERE id = ? AND recovery_codes = ?`),
		strings.Join(remaining, "\n"), now(), userID, recoveryCodes)
	if err != nil {
		return false, fmt.Errorf("consume recovery code: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		// Another request consumed or modified the codes concurrently
		return false, nil
	}
	return true, tx.Commit()
}
