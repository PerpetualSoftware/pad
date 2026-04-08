package store

import (
	"strings"
	"testing"
	"time"

	"github.com/xarmian/pad/internal/models"
)

func TestSessionCreateAndValidate(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")

	// Create session with binding metadata
	token, err := s.CreateSession(u.ID, "cli", "127.0.0.1", "TestAgent/1.0", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}
	if !strings.HasPrefix(token, "padsess_") {
		t.Errorf("expected token prefix 'padsess_', got %q", token[:8])
	}
	if len(token) != 72 { // "padsess_" (8) + 64 hex chars
		t.Errorf("expected token length 72, got %d", len(token))
	}

	// Validate session
	session, err := s.ValidateSession(token)
	if err != nil {
		t.Fatalf("ValidateSession error: %v", err)
	}
	if session == nil {
		t.Fatal("expected session from valid token")
	}
	if session.User == nil {
		t.Fatal("expected user from valid session")
	}
	if session.User.ID != u.ID {
		t.Errorf("expected user ID %q, got %q", u.ID, session.User.ID)
	}
	if session.User.Email != "test@test.com" {
		t.Errorf("expected email 'test@test.com', got %q", session.User.Email)
	}
	if session.IPAddress != "127.0.0.1" {
		t.Errorf("expected IP '127.0.0.1', got %q", session.IPAddress)
	}
	if session.UAHash == "" {
		t.Error("expected non-empty UA hash")
	}
}

func TestSessionInvalidToken(t *testing.T) {
	s := testStore(t)

	session, err := s.ValidateSession("padsess_invalidtoken")
	if err != nil {
		t.Fatalf("ValidateSession error: %v", err)
	}
	if session != nil {
		t.Error("expected nil for invalid token")
	}
}

func TestSessionExpired(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")

	// Create session with 0 TTL (already expired)
	token, err := s.CreateSession(u.ID, "cli", "127.0.0.1", "", 0)
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	// Should not validate
	session, err := s.ValidateSession(token)
	if err != nil {
		t.Fatalf("ValidateSession error: %v", err)
	}
	if session != nil {
		t.Error("expected nil for expired session")
	}
}

func TestSessionDelete(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")

	token, _ := s.CreateSession(u.ID, "web", "127.0.0.1", "Browser/1.0", 7*24*time.Hour)

	// Delete session
	err := s.DeleteSession(token)
	if err != nil {
		t.Fatalf("DeleteSession error: %v", err)
	}

	// Should no longer validate
	session, _ := s.ValidateSession(token)
	if session != nil {
		t.Error("session should not validate after deletion")
	}
}

func TestDeleteUserSessions(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")

	token1, _ := s.CreateSession(u.ID, "web", "127.0.0.1", "Browser/1.0", 7*24*time.Hour)
	token2, _ := s.CreateSession(u.ID, "cli", "127.0.0.1", "CLI/1.0", 30*24*time.Hour)

	// Delete all user sessions
	err := s.DeleteUserSessions(u.ID)
	if err != nil {
		t.Fatalf("DeleteUserSessions error: %v", err)
	}

	// Neither should validate
	s1, _ := s.ValidateSession(token1)
	s2, _ := s.ValidateSession(token2)
	if s1 != nil || s2 != nil {
		t.Error("no sessions should validate after DeleteUserSessions")
	}
}

func TestCleanExpiredSessions(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")

	// Create one expired and one valid session
	s.CreateSession(u.ID, "expired", "", "", 0)
	validToken, _ := s.CreateSession(u.ID, "valid", "127.0.0.1", "", 7*24*time.Hour)

	// Clean expired
	err := s.CleanExpiredSessions()
	if err != nil {
		t.Fatalf("CleanExpiredSessions error: %v", err)
	}

	// Valid session should still work
	session, _ := s.ValidateSession(validToken)
	if session == nil {
		t.Error("valid session should survive cleanup")
	}
}

func TestSessionBindingMetadata(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "test@test.com", "Test", "password123")

	// Create session without binding metadata
	token, _ := s.CreateSession(u.ID, "cli", "", "", 1*time.Hour)
	session, _ := s.ValidateSession(token)
	if session == nil {
		t.Fatal("expected valid session")
	}
	if session.IPAddress != "" {
		t.Errorf("expected empty IP, got %q", session.IPAddress)
	}
	if session.UAHash != "" {
		t.Errorf("expected empty UA hash, got %q", session.UAHash)
	}

	// Create session with binding metadata
	token2, _ := s.CreateSession(u.ID, "web", "192.168.1.1", "Mozilla/5.0", 1*time.Hour)
	session2, _ := s.ValidateSession(token2)
	if session2 == nil {
		t.Fatal("expected valid session")
	}
	if session2.IPAddress != "192.168.1.1" {
		t.Errorf("expected IP '192.168.1.1', got %q", session2.IPAddress)
	}
	if session2.UAHash == "" {
		t.Error("expected non-empty UA hash for session with User-Agent")
	}
}

func TestWorkspaceMemberCRUD(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test Workspace")
	u := createTestUser(t, s, "test@test.com", "Test User", "password123")

	// Add member
	err := s.AddWorkspaceMember(ws.ID, u.ID, "owner")
	if err != nil {
		t.Fatalf("AddWorkspaceMember error: %v", err)
	}

	// Check membership
	isMember, _ := s.IsWorkspaceMember(ws.ID, u.ID)
	if !isMember {
		t.Error("expected user to be a member")
	}

	// Get member
	m, err := s.GetWorkspaceMember(ws.ID, u.ID)
	if err != nil {
		t.Fatalf("GetWorkspaceMember error: %v", err)
	}
	if m == nil {
		t.Fatal("expected member, got nil")
	}
	if m.Role != "owner" {
		t.Errorf("expected role 'owner', got %q", m.Role)
	}

	// List members (with user info join)
	members, err := s.ListWorkspaceMembers(ws.ID)
	if err != nil {
		t.Fatalf("ListWorkspaceMembers error: %v", err)
	}
	if len(members) != 1 {
		t.Errorf("expected 1 member, got %d", len(members))
	}
	if len(members) > 0 {
		if members[0].UserName != "Test User" {
			t.Errorf("expected user name 'Test User', got %q", members[0].UserName)
		}
		if members[0].UserEmail != "test@test.com" {
			t.Errorf("expected user email 'test@test.com', got %q", members[0].UserEmail)
		}
	}

	// Update role
	err = s.UpdateWorkspaceMemberRole(ws.ID, u.ID, "editor")
	if err != nil {
		t.Fatalf("UpdateWorkspaceMemberRole error: %v", err)
	}
	m, _ = s.GetWorkspaceMember(ws.ID, u.ID)
	if m.Role != "editor" {
		t.Errorf("expected role 'editor', got %q", m.Role)
	}

	// Remove member
	err = s.RemoveWorkspaceMember(ws.ID, u.ID)
	if err != nil {
		t.Fatalf("RemoveWorkspaceMember error: %v", err)
	}
	isMember, _ = s.IsWorkspaceMember(ws.ID, u.ID)
	if isMember {
		t.Error("user should no longer be a member after removal")
	}
}

func TestGetUserWorkspaces(t *testing.T) {
	s := testStore(t)
	ws1 := createTestWorkspace(t, s, "Workspace A")
	ws2 := createTestWorkspace(t, s, "Workspace B")
	_ = createTestWorkspace(t, s, "Workspace C") // user not a member

	u := createTestUser(t, s, "test@test.com", "Test", "password123")

	s.AddWorkspaceMember(ws1.ID, u.ID, "owner")
	s.AddWorkspaceMember(ws2.ID, u.ID, "editor")

	workspaces, err := s.GetUserWorkspaces(u.ID)
	if err != nil {
		t.Fatalf("GetUserWorkspaces error: %v", err)
	}
	if len(workspaces) != 2 {
		t.Errorf("expected 2 workspaces, got %d", len(workspaces))
	}
}

func TestWorkspaceMemberNotFound(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	m, err := s.GetWorkspaceMember(ws.ID, "nonexistent-user")
	if err != nil {
		t.Fatalf("GetWorkspaceMember error: %v", err)
	}
	if m != nil {
		t.Error("expected nil for nonexistent member")
	}

	isMember, _ := s.IsWorkspaceMember(ws.ID, "nonexistent-user")
	if isMember {
		t.Error("nonexistent user should not be a member")
	}
}

// Ensure createTestUser helper is usable from other test files
// by verifying it works correctly with the workspace member pattern
func TestCreateTestUserHelper(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "helper@test.com", "Helper", "pass")

	if u.ID == "" {
		t.Error("user ID should not be empty")
	}

	// Should be reusable for sessions
	token, err := s.CreateSession(u.ID, "test", "127.0.0.1", "TestAgent", 1*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession with test user error: %v", err)
	}

	session, err := s.ValidateSession(token)
	if err != nil {
		t.Fatalf("ValidateSession error: %v", err)
	}
	if session.User.Email != "helper@test.com" {
		t.Errorf("expected email 'helper@test.com', got %q", session.User.Email)
	}
}

// Suppress unused import warning — models is used in createTestUser
var _ = models.UserCreate{}
