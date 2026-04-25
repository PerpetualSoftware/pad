package models

import (
	"encoding/json"
	"time"
)

// User represents a registered user in the system.
type User struct {
	ID               string    `json:"id"`
	Email            string    `json:"email"`
	Username         string    `json:"username"` // Unique handle; empty until set
	Name             string    `json:"name"`
	PasswordHash     string    `json:"-"`    // Never serialized
	Role             string    `json:"role"` // "admin" or "member"
	AvatarURL        string    `json:"avatar_url,omitempty"`
	TOTPSecret       string    `json:"-"` // Never serialized
	TOTPEnabled      bool      `json:"totp_enabled"`
	RecoveryCodes    string    `json:"-"`    // Never serialized
	Plan             string    `json:"plan"` // "free", "pro", or "self-hosted"
	PlanExpiresAt    string    `json:"plan_expires_at,omitempty"`
	StripeCustomerID string    `json:"-"`                        // Never serialized
	PlanOverrides    string    `json:"plan_overrides,omitempty"` // JSON overrides for per-user limits
	OAuthProviders   string    `json:"-"`                        // JSON array of linked providers, e.g. ["github","google"]
	PasswordSet      bool      `json:"password_set"`             // True if the user explicitly set a password (vs. OAuth placeholder hash)
	DisabledAt       string    `json:"disabled_at,omitempty"`    // Non-empty = account disabled
	LastActiveAt     string    `json:"last_active_at,omitempty"` // Last authenticated API request
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// IsDisabled returns true if the user account has been disabled.
func (u *User) IsDisabled() bool {
	return u.DisabledAt != ""
}

// GetOAuthProviders parses the JSON oauth_providers field into a string slice.
func (u *User) GetOAuthProviders() []string {
	if u.OAuthProviders == "" {
		return nil
	}
	var providers []string
	if err := json.Unmarshal([]byte(u.OAuthProviders), &providers); err != nil {
		return nil
	}
	return providers
}

// HasOAuthProvider returns true if the user has linked the given provider.
func (u *User) HasOAuthProvider(provider string) bool {
	for _, p := range u.GetOAuthProviders() {
		if p == provider {
			return true
		}
	}
	return false
}

// HasPassword returns true if the user has explicitly set a password that
// they can sign in with. OAuth-only users have a random placeholder hash
// stored in PasswordHash which can't actually be used to log in, so this
// bit is tracked separately from PasswordHash being non-empty.
func (u *User) HasPassword() bool {
	return u.PasswordSet
}

// UserCreate is the input for registering a new user.
type UserCreate struct {
	Email    string `json:"email"`
	Username string `json:"username,omitempty"` // Optional; auto-generated if empty
	Name     string `json:"name"`
	Password string `json:"password"`       // Plaintext, will be hashed
	Role     string `json:"role,omitempty"` // Defaults to "member"
}

// UserUpdate is the input for updating user profile fields.
type UserUpdate struct {
	Name      *string `json:"name,omitempty"`
	Username  *string `json:"username,omitempty"`
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
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// IsExpired reports whether an invitation is past its expiration window.
// Invitations created before the expires_at migration (nil ExpiresAt) are
// treated as non-expiring so existing codes don't break on upgrade.
func (inv *WorkspaceInvitation) IsExpired() bool {
	if inv == nil || inv.ExpiresAt == nil {
		return false
	}
	return time.Now().UTC().After(*inv.ExpiresAt)
}

// WorkspaceMember represents a user's membership in a workspace.
type WorkspaceMember struct {
	WorkspaceID      string    `json:"workspace_id"`
	UserID           string    `json:"user_id"`
	Role             string    `json:"role"`              // "owner", "editor", or "viewer"
	CollectionAccess string    `json:"collection_access"` // "all" or "specific"
	CreatedAt        time.Time `json:"created_at"`

	// Populated by joins (not stored)
	UserName     string `json:"user_name,omitempty"`
	UserEmail    string `json:"user_email,omitempty"`
	UserUsername string `json:"user_username,omitempty"`
}
