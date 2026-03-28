package store

import (
	"testing"

	"github.com/xarmian/pad/internal/models"
)

func createTestUser(t *testing.T, s *Store, email, name, password string) *models.User {
	t.Helper()
	u, err := s.CreateUser(models.UserCreate{
		Email:    email,
		Name:     name,
		Password: password,
	})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	return u
}

func TestUserCRUD(t *testing.T) {
	s := testStore(t)

	// Create
	u, err := s.CreateUser(models.UserCreate{
		Email:    "Dave@Example.com",
		Name:     "Dave",
		Password: "password123",
	})
	if err != nil {
		t.Fatalf("CreateUser error: %v", err)
	}
	if u.Name != "Dave" {
		t.Errorf("expected name 'Dave', got %q", u.Name)
	}
	if u.Email != "dave@example.com" {
		t.Errorf("expected lowercased email, got %q", u.Email)
	}
	if u.Role != "member" {
		t.Errorf("expected default role 'member', got %q", u.Role)
	}
	if u.PasswordHash == "" {
		t.Error("password hash should not be empty")
	}
	if u.PasswordHash == "password123" {
		t.Error("password should be hashed, not stored plaintext")
	}

	// Get by ID
	got, err := s.GetUser(u.ID)
	if err != nil {
		t.Fatalf("GetUser error: %v", err)
	}
	if got == nil || got.ID != u.ID {
		t.Error("GetUser returned wrong user")
	}

	// Get by email (case-insensitive)
	got, err = s.GetUserByEmail("DAVE@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail error: %v", err)
	}
	if got == nil || got.ID != u.ID {
		t.Error("GetUserByEmail returned wrong user")
	}

	// Get nonexistent
	got, err = s.GetUser("nonexistent-id")
	if err != nil {
		t.Fatalf("GetUser nonexistent error: %v", err)
	}
	if got != nil {
		t.Error("expected nil for nonexistent user")
	}

	// Update name
	newName := "David"
	updated, err := s.UpdateUser(u.ID, models.UserUpdate{Name: &newName})
	if err != nil {
		t.Fatalf("UpdateUser error: %v", err)
	}
	if updated.Name != "David" {
		t.Errorf("expected updated name 'David', got %q", updated.Name)
	}

	// Update password
	newPass := "newpassword456"
	_, err = s.UpdateUser(u.ID, models.UserUpdate{Password: &newPass})
	if err != nil {
		t.Fatalf("UpdateUser password error: %v", err)
	}
	// Old password should no longer work
	result, _ := s.ValidatePassword("dave@example.com", "password123")
	if result != nil {
		t.Error("old password should not validate after change")
	}
	// New password should work
	result, _ = s.ValidatePassword("dave@example.com", "newpassword456")
	if result == nil {
		t.Error("new password should validate after change")
	}
}

func TestUserCreateAdmin(t *testing.T) {
	s := testStore(t)

	u, err := s.CreateUser(models.UserCreate{
		Email:    "admin@test.com",
		Name:     "Admin",
		Password: "admin123",
		Role:     "admin",
	})
	if err != nil {
		t.Fatalf("CreateUser error: %v", err)
	}
	if u.Role != "admin" {
		t.Errorf("expected role 'admin', got %q", u.Role)
	}
}

func TestUserDuplicateEmail(t *testing.T) {
	s := testStore(t)

	_, err := s.CreateUser(models.UserCreate{
		Email: "dave@test.com", Name: "Dave", Password: "pass123",
	})
	if err != nil {
		t.Fatalf("first CreateUser error: %v", err)
	}

	_, err = s.CreateUser(models.UserCreate{
		Email: "dave@test.com", Name: "Other Dave", Password: "pass456",
	})
	if err == nil {
		t.Error("expected error for duplicate email")
	}
}

func TestValidatePassword(t *testing.T) {
	s := testStore(t)

	createTestUser(t, s, "test@test.com", "Test", "correctpassword")

	// Correct password
	u, err := s.ValidatePassword("test@test.com", "correctpassword")
	if err != nil {
		t.Fatalf("ValidatePassword error: %v", err)
	}
	if u == nil {
		t.Error("expected user for correct password")
	}

	// Wrong password
	u, err = s.ValidatePassword("test@test.com", "wrongpassword")
	if err != nil {
		t.Fatalf("ValidatePassword wrong password error: %v", err)
	}
	if u != nil {
		t.Error("expected nil for wrong password")
	}

	// Nonexistent email
	u, err = s.ValidatePassword("nobody@test.com", "anything")
	if err != nil {
		t.Fatalf("ValidatePassword nonexistent error: %v", err)
	}
	if u != nil {
		t.Error("expected nil for nonexistent email")
	}
}

func TestListUsers(t *testing.T) {
	s := testStore(t)

	createTestUser(t, s, "a@test.com", "Alice", "pass1")
	createTestUser(t, s, "b@test.com", "Bob", "pass2")

	users, err := s.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers error: %v", err)
	}
	if len(users) != 2 {
		t.Errorf("expected 2 users, got %d", len(users))
	}
}

func TestUserCount(t *testing.T) {
	s := testStore(t)

	count, _ := s.UserCount()
	if count != 0 {
		t.Errorf("expected 0 users, got %d", count)
	}

	createTestUser(t, s, "a@test.com", "Alice", "pass1")
	createTestUser(t, s, "b@test.com", "Bob", "pass2")

	count, _ = s.UserCount()
	if count != 2 {
		t.Errorf("expected 2 users, got %d", count)
	}
}
