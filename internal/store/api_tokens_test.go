package store

import (
	"testing"

	"github.com/xarmian/pad/internal/models"
)

func TestCreateAPITokenWithExpiry(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")
	ws := createTestWorkspace(t, s, "TokenTest")

	// Create token with default 90-day expiry
	token, err := s.CreateAPIToken(u.ID, models.APITokenCreate{
		Name:        "test-token",
		WorkspaceID: ws.ID,
	}, 90, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken error: %v", err)
	}
	if token.Token == "" {
		t.Fatal("expected plaintext token")
	}
	if token.ExpiresAt == nil {
		t.Fatal("expected expiry to be set with default 90 days")
	}
	if token.Name != "test-token" {
		t.Errorf("expected name 'test-token', got %q", token.Name)
	}
	if token.Scopes != `["*"]` {
		t.Errorf("expected default scopes, got %q", token.Scopes)
	}
}

func TestCreateAPITokenExpiryOverride(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")
	ws := createTestWorkspace(t, s, "TokenTest")

	// Create token with explicit 30-day expiry
	token, err := s.CreateAPIToken(u.ID, models.APITokenCreate{
		Name:        "short-lived",
		WorkspaceID: ws.ID,
		ExpiresIn:   30,
	}, 90, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken error: %v", err)
	}
	if token.ExpiresAt == nil {
		t.Fatal("expected expiry to be set")
	}
}

func TestCreateAPITokenMaxLifetimeEnforced(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")
	ws := createTestWorkspace(t, s, "TokenTest")

	// Request 365-day expiry but max is 30 days
	token, err := s.CreateAPIToken(u.ID, models.APITokenCreate{
		Name:        "capped",
		WorkspaceID: ws.ID,
		ExpiresIn:   365,
	}, 90, 30)
	if err != nil {
		t.Fatalf("CreateAPIToken error: %v", err)
	}
	if token.ExpiresAt == nil {
		t.Fatal("expected expiry to be set")
	}
}

func TestCreateAPITokenNoExpiry(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")
	ws := createTestWorkspace(t, s, "TokenTest")

	// Default of 0 means no expiry
	token, err := s.CreateAPIToken(u.ID, models.APITokenCreate{
		Name:        "no-expiry",
		WorkspaceID: ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken error: %v", err)
	}
	if token.ExpiresAt != nil {
		t.Error("expected no expiry when default is 0")
	}
}

func TestValidateTokenExpired(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")
	ws := createTestWorkspace(t, s, "TokenTest")

	tok, err := s.CreateAPIToken(u.ID, models.APITokenCreate{
		Name:        "will-expire",
		WorkspaceID: ws.ID,
		ExpiresIn:   1,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken error: %v", err)
	}

	// Token should validate now
	validated, err := s.ValidateToken(tok.Token)
	if err != nil {
		t.Fatalf("ValidateToken error: %v", err)
	}
	if validated == nil {
		t.Fatal("expected token to validate before expiry")
	}

	// Manually expire it
	_, err = s.db.Exec(s.q("UPDATE api_tokens SET expires_at = '2020-01-01T00:00:00Z' WHERE id = ?"), tok.ID)
	if err != nil {
		t.Fatalf("manual expire error: %v", err)
	}

	// Should not validate
	validated, err = s.ValidateToken(tok.Token)
	if err != nil {
		t.Fatalf("ValidateToken error: %v", err)
	}
	if validated != nil {
		t.Error("expected nil for expired token")
	}
}

func TestRotateAPIToken(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")
	ws := createTestWorkspace(t, s, "TokenTest")

	// Create original token
	original, err := s.CreateAPIToken(u.ID, models.APITokenCreate{
		Name:        "rotatable",
		WorkspaceID: ws.ID,
	}, 90, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken error: %v", err)
	}

	// Rotate
	rotated, err := s.RotateAPIToken(original.ID, u.ID, 0, 0)
	if err != nil {
		t.Fatalf("RotateAPIToken error: %v", err)
	}

	// New token should be different
	if rotated.Token == original.Token {
		t.Error("rotated token should have a new secret")
	}
	if rotated.ID != original.ID {
		t.Error("rotated token should keep same ID")
	}
	if rotated.Name != original.Name {
		t.Error("rotated token should keep same name")
	}

	// Old token should not validate
	old, err := s.ValidateToken(original.Token)
	if err != nil {
		t.Fatalf("ValidateToken error: %v", err)
	}
	if old != nil {
		t.Error("old token should not validate after rotation")
	}

	// New token should validate
	validated, err := s.ValidateToken(rotated.Token)
	if err != nil {
		t.Fatalf("ValidateToken error: %v", err)
	}
	if validated == nil {
		t.Fatal("new token should validate after rotation")
	}
	if validated.Name != "rotatable" {
		t.Errorf("expected name 'rotatable', got %q", validated.Name)
	}
}

func TestRotateAPITokenWrongUser(t *testing.T) {
	s := testStore(t)
	u1 := createTestUser(t, s, "user1@test.com", "User 1", "password123")
	u2 := createTestUser(t, s, "user2@test.com", "User 2", "password123")
	ws := createTestWorkspace(t, s, "TokenTest")

	// Create token owned by u1
	token, err := s.CreateAPIToken(u1.ID, models.APITokenCreate{
		Name:        "u1-token",
		WorkspaceID: ws.ID,
	}, 90, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken error: %v", err)
	}

	// Try to rotate as u2
	_, err = s.RotateAPIToken(token.ID, u2.ID, 0, 0)
	if err == nil {
		t.Error("expected error when rotating another user's token")
	}
}

func TestRotateAPITokenWithNewExpiry(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")
	ws := createTestWorkspace(t, s, "TokenTest")

	// Create token with 90-day expiry
	original, err := s.CreateAPIToken(u.ID, models.APITokenCreate{
		Name:        "rotate-expiry",
		WorkspaceID: ws.ID,
	}, 90, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken error: %v", err)
	}
	originalExpiry := original.ExpiresAt

	// Rotate with new 30-day expiry
	rotated, err := s.RotateAPIToken(original.ID, u.ID, 30, 0)
	if err != nil {
		t.Fatalf("RotateAPIToken error: %v", err)
	}

	if rotated.ExpiresAt == nil {
		t.Fatal("expected new expiry on rotated token")
	}
	if originalExpiry != nil && !rotated.ExpiresAt.Before(*originalExpiry) {
		t.Error("new expiry should be before original 90-day expiry")
	}
}

func TestListAndDeleteAPITokens(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")
	ws := createTestWorkspace(t, s, "TokenTest")

	// Create two tokens
	s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "token-1", WorkspaceID: ws.ID}, 90, 0)
	s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "token-2", WorkspaceID: ws.ID}, 90, 0)

	// List
	tokens, err := s.ListUserAPITokens(u.ID)
	if err != nil {
		t.Fatalf("ListUserAPITokens error: %v", err)
	}
	if len(tokens) != 2 {
		t.Errorf("expected 2 tokens, got %d", len(tokens))
	}

	// Delete one
	err = s.DeleteUserAPIToken(tokens[0].ID, u.ID)
	if err != nil {
		t.Fatalf("DeleteUserAPIToken error: %v", err)
	}

	// Should be 1 left
	tokens, _ = s.ListUserAPITokens(u.ID)
	if len(tokens) != 1 {
		t.Errorf("expected 1 token after delete, got %d", len(tokens))
	}
}

func TestValidateTokenUpdatesLastUsed(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")
	ws := createTestWorkspace(t, s, "TokenTest")

	tok, err := s.CreateAPIToken(u.ID, models.APITokenCreate{
		Name:        "tracked",
		WorkspaceID: ws.ID,
	}, 90, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken error: %v", err)
	}

	// Initially no last_used_at
	if tok.LastUsedAt != nil {
		t.Error("expected nil last_used_at on new token")
	}

	// Validate to trigger last_used_at update
	validated, _ := s.ValidateToken(tok.Token)
	if validated == nil {
		t.Fatal("expected valid token")
	}

	// Fetch again to see last_used_at
	fetched, _ := s.getAPIToken(tok.ID)
	if fetched.LastUsedAt == nil {
		t.Error("expected last_used_at to be set after validation")
	}
}
