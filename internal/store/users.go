package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/xarmian/pad/internal/models"
	"golang.org/x/crypto/bcrypt"
)

var usernameCleanRe = regexp.MustCompile(`[^a-z0-9-]+`)

const bcryptCost = 12

// user SELECT columns — used by all user queries.
const userColumns = `id, email, username, name, password_hash, role, avatar_url, totp_secret, totp_enabled, recovery_codes, plan, plan_expires_at, stripe_customer_id, plan_overrides, created_at, updated_at`

// scanUser scans a user row into a User struct.
// Note: does NOT decrypt the TOTP secret — call store.decryptUserTOTP() after
// scanning if you need the plaintext secret for validation.
func scanUser(row interface{ Scan(...interface{}) error }) (*models.User, error) {
	var u models.User
	var createdAt, updatedAt string

	err := row.Scan(
		&u.ID, &u.Email, &u.Username, &u.Name, &u.PasswordHash, &u.Role, &u.AvatarURL,
		&u.TOTPSecret, &u.TOTPEnabled, &u.RecoveryCodes,
		&u.Plan, &u.PlanExpiresAt, &u.StripeCustomerID, &u.PlanOverrides,
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

// decryptUserTOTP decrypts the TOTP secret on a User struct in place.
func (s *Store) decryptUserTOTP(u *models.User) error {
	if u == nil || u.TOTPSecret == "" {
		return nil
	}
	decrypted, err := s.decrypt(u.TOTPSecret)
	if err != nil {
		return fmt.Errorf("decrypt user TOTP: %w", err)
	}
	u.TOTPSecret = decrypted
	return nil
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
	if err := s.decryptUserTOTP(u); err != nil {
		return nil, err
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
	if err := s.decryptUserTOTP(u); err != nil {
		return nil, err
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
	if err := s.decryptUserTOTP(u); err != nil {
		return nil, err
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
		_ = s.decryptUserTOTP(u) // Best-effort decrypt for list (TOTP secret is json:"-" anyway)
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

// CreateOAuthUser creates a user from an OAuth provider with a random unusable password.
// OAuth users can later set a password via the password reset flow if they want.
func (s *Store) CreateOAuthUser(email, name, avatarURL string) (*models.User, error) {
	// Generate a random 64-byte password the user will never use
	randomPwd := make([]byte, 64)
	if _, err := rand.Read(randomPwd); err != nil {
		return nil, fmt.Errorf("generate random password: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword(randomPwd, bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	id := newID()
	ts := now()

	username := GenerateUsername(name, email)
	username, err = s.EnsureUniqueUsername(username)
	if err != nil {
		return nil, fmt.Errorf("generate username: %w", err)
	}

	_, err = s.db.Exec(s.q(`
		INSERT INTO users (id, email, username, name, password_hash, role, avatar_url, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), id, strings.ToLower(strings.TrimSpace(email)), username, strings.TrimSpace(name), string(hash), "member", avatarURL, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("insert oauth user: %w", err)
	}

	return s.GetUser(id)
}

// DeleteUser permanently deletes a user by ID.
func (s *Store) DeleteUser(id string) error {
	_, err := s.db.Exec(s.q(`DELETE FROM users WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	return nil
}

// --- Username backfill ---

// GenerateUsername derives a URL-safe username from a display name.
// Falls back to the email local part if the name produces an empty result.
func GenerateUsername(name, email string) string {
	// Lowercase and replace spaces/special chars with hyphens
	u := strings.ToLower(strings.TrimSpace(name))
	u = usernameCleanRe.ReplaceAllString(u, "-")

	// Collapse consecutive hyphens, strip leading/trailing
	for strings.Contains(u, "--") {
		u = strings.ReplaceAll(u, "--", "-")
	}
	u = strings.Trim(u, "-")

	// Truncate to 39 chars (GitHub-style limit)
	if len(u) > 39 {
		u = u[:39]
		u = strings.TrimRight(u, "-")
	}

	// Fall back to email local part
	if u == "" && email != "" {
		local := strings.Split(email, "@")[0]
		u = strings.ToLower(local)
		u = usernameCleanRe.ReplaceAllString(u, "-")
		u = strings.Trim(u, "-")
		if len(u) > 39 {
			u = u[:39]
			u = strings.TrimRight(u, "-")
		}
	}

	if u == "" {
		u = "user"
	}
	return u
}

// EnsureUniqueUsername takes a candidate username and returns a unique variant
// by appending -2, -3, etc. if the candidate already exists in the database.
func (s *Store) EnsureUniqueUsername(base string) (string, error) {
	username := base
	suffix := 2
	for {
		existing, err := s.GetUserByUsername(username)
		if err != nil {
			return "", fmt.Errorf("check username uniqueness: %w", err)
		}
		if existing == nil {
			return username, nil
		}
		username = fmt.Sprintf("%s-%d", base, suffix)
		suffix++
	}
}

// backfillUsernames generates usernames for existing users that don't have one.
// Idempotent: skips users who already have a non-empty username.
func (s *Store) backfillUsernames() error {
	// Find users with empty username
	rows, err := s.db.Query(s.q(`SELECT id, name, email FROM users WHERE username = '' OR username IS NULL`))
	if err != nil {
		return fmt.Errorf("find users without username: %w", err)
	}
	defer rows.Close()

	type userRow struct {
		id, name, email string
	}
	var users []userRow
	for rows.Next() {
		var u userRow
		if err := rows.Scan(&u.id, &u.name, &u.email); err != nil {
			return fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if len(users) == 0 {
		return nil // Nothing to backfill
	}

	// Collect all existing usernames to detect collisions
	existing := make(map[string]bool)
	existingRows, err := s.db.Query(s.q(`SELECT username FROM users WHERE username != ''`))
	if err != nil {
		return fmt.Errorf("list existing usernames: %w", err)
	}
	defer existingRows.Close()
	for existingRows.Next() {
		var u string
		if err := existingRows.Scan(&u); err != nil {
			return err
		}
		existing[strings.ToLower(u)] = true
	}

	for _, u := range users {
		base := GenerateUsername(u.name, u.email)
		username := base

		// Handle collisions: append -2, -3, etc.
		suffix := 2
		for existing[username] {
			username = fmt.Sprintf("%s-%d", base, suffix)
			suffix++
		}

		existing[username] = true

		_, err := s.db.Exec(s.q(`UPDATE users SET username = ?, updated_at = ? WHERE id = ?`),
			username, now(), u.id)
		if err != nil {
			return fmt.Errorf("set username for user %s: %w", u.id, err)
		}
	}

	return nil
}

// --- TOTP 2FA ---

// SetTOTPSecret stores the TOTP secret for a user (before 2FA is verified).
// The secret is encrypted at rest if an encryption key is configured.
func (s *Store) SetTOTPSecret(userID, secret string) error {
	encrypted, err := s.encrypt(secret)
	if err != nil {
		return fmt.Errorf("encrypt totp secret: %w", err)
	}
	_, err = s.db.Exec(s.q(`UPDATE users SET totp_secret = ?, updated_at = ? WHERE id = ?`), encrypted, now(), userID)
	if err != nil {
		return fmt.Errorf("set totp secret: %w", err)
	}
	return nil
}

// EnableTOTP atomically enables 2FA for a user and stores hashed recovery codes.
// The expectedSecret is the plaintext secret — it's compared against the stored
// (possibly encrypted) value to prevent TOCTOU races.
func (s *Store) EnableTOTP(userID, expectedSecret, hashedRecoveryCodes string) error {
	// Read the stored (possibly encrypted) secret to compare
	var storedSecret string
	err := s.db.QueryRow(s.q(`SELECT totp_secret FROM users WHERE id = ? AND totp_enabled = ?`),
		userID, s.dialect.BoolToInt(false)).Scan(&storedSecret)
	if err != nil {
		return fmt.Errorf("enable totp: read secret: %w", err)
	}

	// Decrypt stored secret for comparison
	decrypted, err := s.decrypt(storedSecret)
	if err != nil {
		return fmt.Errorf("enable totp: decrypt stored secret: %w", err)
	}
	if decrypted != expectedSecret {
		return fmt.Errorf("enable totp: secret mismatch or user not found")
	}

	// Update — use the stored (encrypted) value in WHERE for atomicity
	result, err := s.db.Exec(s.q(
		`UPDATE users SET totp_enabled = ?, recovery_codes = ?, updated_at = ?
		 WHERE id = ? AND totp_secret = ? AND totp_enabled = ?`),
		s.dialect.BoolToInt(true), hashedRecoveryCodes, now(), userID, storedSecret, s.dialect.BoolToInt(false))
	if err != nil {
		return fmt.Errorf("enable totp: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("enable totp: concurrent modification or user not found")
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
