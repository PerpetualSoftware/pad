package models

import "time"

// User represents a registered user in the system.
type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	PasswordHash string    `json:"-"` // Never serialized
	Role         string    `json:"role"` // "admin" or "member"
	AvatarURL    string    `json:"avatar_url,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// UserCreate is the input for registering a new user.
type UserCreate struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Password string `json:"password"` // Plaintext, will be hashed
	Role     string `json:"role,omitempty"` // Defaults to "member"
}

// UserUpdate is the input for updating user profile fields.
type UserUpdate struct {
	Name      *string `json:"name,omitempty"`
	Password  *string `json:"password,omitempty"` // Plaintext, will be hashed
	AvatarURL *string `json:"avatar_url,omitempty"`
}

// Session represents a database-backed authentication session.
type Session struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	TokenHash  string    `json:"-"` // Never serialized
	DeviceInfo string    `json:"device_info,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
	CreatedAt  time.Time `json:"created_at"`
}

// WorkspaceInvitation represents a pending invitation to join a workspace.
type WorkspaceInvitation struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	Email       string     `json:"email"`
	Role        string     `json:"role"`
	InvitedBy   string     `json:"invited_by"`
	Code        string     `json:"code"`
	AcceptedAt  *time.Time `json:"accepted_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// WorkspaceMember represents a user's membership in a workspace.
type WorkspaceMember struct {
	WorkspaceID string    `json:"workspace_id"`
	UserID      string    `json:"user_id"`
	Role        string    `json:"role"` // "owner", "editor", or "viewer"
	CreatedAt   time.Time `json:"created_at"`

	// Populated by joins (not stored)
	UserName  string `json:"user_name,omitempty"`
	UserEmail string `json:"user_email,omitempty"`
}
