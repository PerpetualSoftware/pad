package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/xarmian/pad/internal/models"
	"golang.org/x/crypto/bcrypt"
)

const bcryptCost = 10

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

	_, err = s.db.Exec(`
		INSERT INTO users (id, email, name, password_hash, role, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, id, strings.ToLower(strings.TrimSpace(input.Email)), strings.TrimSpace(input.Name), string(hash), role, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}

	return s.GetUser(id)
}

// GetUser retrieves a user by ID.
func (s *Store) GetUser(id string) (*models.User, error) {
	var u models.User
	var createdAt, updatedAt string

	err := s.db.QueryRow(`
		SELECT id, email, name, password_hash, role, avatar_url, created_at, updated_at
		FROM users WHERE id = ?
	`, id).Scan(
		&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, &u.AvatarURL,
		&createdAt, &updatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}

	u.CreatedAt = parseTime(createdAt)
	u.UpdatedAt = parseTime(updatedAt)
	return &u, nil
}

// GetUserByEmail retrieves a user by email address (case-insensitive).
func (s *Store) GetUserByEmail(email string) (*models.User, error) {
	var u models.User
	var createdAt, updatedAt string

	err := s.db.QueryRow(`
		SELECT id, email, name, password_hash, role, avatar_url, created_at, updated_at
		FROM users WHERE email = ?
	`, strings.ToLower(strings.TrimSpace(email))).Scan(
		&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, &u.AvatarURL,
		&createdAt, &updatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by email: %w", err)
	}

	u.CreatedAt = parseTime(createdAt)
	u.UpdatedAt = parseTime(updatedAt)
	return &u, nil
}

// UpdateUser updates mutable user fields.
func (s *Store) UpdateUser(id string, input models.UserUpdate) (*models.User, error) {
	var sets []string
	var args []interface{}

	if input.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, strings.TrimSpace(*input.Name))
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
	result, err := s.db.Exec(query, args...)
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
	rows, err := s.db.Query(`
		SELECT id, email, name, password_hash, role, avatar_url, created_at, updated_at
		FROM users ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var result []models.User
	for rows.Next() {
		var u models.User
		var createdAt, updatedAt string
		if err := rows.Scan(
			&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, &u.AvatarURL,
			&createdAt, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		u.CreatedAt = parseTime(createdAt)
		u.UpdatedAt = parseTime(updatedAt)
		result = append(result, u)
	}
	return result, rows.Err()
}

// UserCount returns the total number of registered users.
func (s *Store) UserCount() (int, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return count, nil
}
